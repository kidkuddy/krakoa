// Package engine wires the pure interpreter to the ports: it admits runs,
// executes actions, routes events, fires timers, and recovers from restarts.
// Nothing in memory is authoritative — every decision round-trips the store.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kidkuddy/krakoa/internal/contact"
	"github.com/kidkuddy/krakoa/internal/core"
	"github.com/kidkuddy/krakoa/internal/runner"
	"github.com/kidkuddy/krakoa/internal/store"
	"github.com/kidkuddy/krakoa/internal/workspace"
)

// Clock is fakeable time.
type Clock interface{ Now() time.Time }

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

type Engine struct {
	mu sync.Mutex

	Store      *store.Store
	Runner     runner.Runner
	Clock      Clock
	Channels   []contact.Channel
	Workspaces map[string]*workspace.Workspace
	DataDir    string // per-step base dirs live here

	// Spawn runs an agent-step job: `go f()` in the daemon, `f()` in tests
	// (deterministic, synchronous seam tests).
	Spawn func(f func())

	// pending holds agent jobs queued under the lock, dispatched by drain()
	// after release — a sync Spawn re-locks, so it must never fire while
	// e.mu is held.
	pending []func()

	// watcherFails counts consecutive derailed sweeps per "ws/watcher";
	// in-memory by design (a restart resets the strike count, the cadence
	// keeps sweeping).
	watcherFails map[string]int

	// Exec runs a deterministic command probe (cwd = workspace dir) and
	// returns its stdout. Overridable for dry-run and tests.
	Exec func(dir, command string) ([]byte, error)

	Log *log.Logger
}

func realExec(dir, command string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	return cmd.Output()
}

// drain dispatches queued jobs without holding the lock. Every public entry
// point and every job completion path ends with a drain.
func (e *Engine) drain() {
	for {
		e.mu.Lock()
		if len(e.pending) == 0 {
			e.mu.Unlock()
			return
		}
		job := e.pending[0]
		e.pending = e.pending[1:]
		e.mu.Unlock()
		e.Spawn(job)
	}
}

func New(st *store.Store, rn runner.Runner, clk Clock, wss map[string]*workspace.Workspace, dataDir string) *Engine {
	return &Engine{
		Store:        st,
		Runner:       rn,
		Clock:        clk,
		Workspaces:   wss,
		DataDir:      dataDir,
		Spawn:        func(f func()) { go f() },
		watcherFails: map[string]int{},
		Exec:         realExec,
		Log:          log.New(os.Stdout, "", log.LstdFlags),
	}
}

func (e *Engine) def(ws, wf string) (*workspace.Workspace, *core.WorkflowDefinition, error) {
	w, ok := e.Workspaces[ws]
	if !ok {
		return nil, nil, fmt.Errorf("unknown workspace %q", ws)
	}
	d, ok := w.Workflows[wf]
	if !ok {
		return nil, nil, fmt.Errorf("workspace %s: unknown workflow %q", ws, wf)
	}
	return w, d, nil
}

func newID(prefix string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func (e *Engine) event(runID, state, kind string, data map[string]any, ws string) {
	ev := &core.Event{Workspace: ws, RunID: runID, State: state, Kind: kind, Data: data, At: e.Clock.Now()}
	if err := e.Store.AppendEvent(ev); err != nil {
		e.Log.Printf("event append failed: %v", err)
	}
}

// --- run lifecycle ---

// StartRun validates inputs and admits (or FIFO-queues) a new run.
func (e *Engine) StartRun(wsName, wfName string, inputs map[string]any, parent string) (*core.Run, error) {
	e.mu.Lock()
	run, err := e.startRunLocked(wsName, wfName, inputs, parent)
	e.mu.Unlock()
	e.drain()
	return run, err
}

func (e *Engine) startRunLocked(wsName, wfName string, inputs map[string]any, parent string) (*core.Run, error) {
	ws, def, err := e.def(wsName, wfName)
	if err != nil {
		return nil, err
	}
	if inputs == nil {
		inputs = map[string]any{}
	}
	for name, spec := range def.Inputs {
		if _, ok := inputs[name]; !ok {
			if spec.Default != "" {
				inputs[name] = spec.Default
			} else if spec.Required {
				return nil, fmt.Errorf("missing required input %q", name)
			}
		}
	}
	now := e.Clock.Now()
	run := &core.Run{
		ID: newID(wfName), Workspace: wsName, Workflow: wfName,
		DefHash: def.Hash, WSVersion: ws.GitVersion,
		State: def.Start, Status: core.StatusQueued,
		Inputs: inputs, Context: map[string]any{}, EdgeCounts: map[string]int{},
		Parent: parent, CreatedAt: now, UpdatedAt: now,
	}
	if err := e.Store.CreateRun(run); err != nil {
		return nil, err
	}
	e.event(run.ID, "", "run-created", map[string]any{"inputs": inputs, "def_hash": def.Hash}, wsName)
	e.stampThreadLocked(def, run)

	active, err := e.Store.CountActive(wsName, wfName)
	if err != nil {
		return nil, err
	}
	if def.Concurrency > 0 && active >= def.Concurrency {
		e.event(run.ID, "", "run-queued", map[string]any{"active": active, "limit": def.Concurrency}, wsName)
		return run, nil // stays queued; admitted when a slot frees
	}
	e.admitLocked(def, run)
	return run, nil
}

// stampThreadLocked resolves the definition's thread template the moment it
// becomes resolvable and stamps the run once. Runs sharing a thread key
// serve the same piece of work (F4).
func (e *Engine) stampThreadLocked(def *core.WorkflowDefinition, run *core.Run) {
	if run.Thread != "" || def.Thread == "" {
		return
	}
	v, err := core.Resolve(run, def.Thread)
	if err != nil {
		return // not resolvable yet; try again after the next transition
	}
	key := fmt.Sprintf("%v", v)
	if key == "" || key == "<nil>" {
		return
	}
	run.Thread = key
	if err := e.Store.SetRunThread(run.ID, key); err != nil {
		e.Log.Printf("stamp thread %s: %v", run.ID, err)
		return
	}
	e.event(run.ID, run.State, "thread-stamped", map[string]any{"thread": key}, run.Workspace)
}

func (e *Engine) admitLocked(def *core.WorkflowDefinition, run *core.Run) {
	e.event(run.ID, run.State, "run-admitted", nil, run.Workspace)
	d := core.Enter(def, *run)
	e.applyLocked(def, d)
}

// applyLocked persists a decision and executes its actions. Must hold e.mu.
func (e *Engine) applyLocked(def *core.WorkflowDefinition, d core.Decision) {
	run := d.Run
	run.UpdatedAt = e.Clock.Now()
	if err := e.Store.SaveRun(&run); err != nil {
		e.Log.Printf("save run %s: %v", run.ID, err)
		return
	}
	e.stampThreadLocked(def, &run)
	for _, a := range d.Actions {
		switch act := a.(type) {
		case core.ActionRunAgent:
			e.startAgentStepLocked(def, run, act)
		case core.ActionOpenGate:
			e.openGateLocked(run, act)
		case core.ActionArmWait:
			e.armWaitLocked(def, run, act)
		case core.ActionPark:
			e.parkLocked(run, act.Reason)
		case core.ActionFinish:
			e.finishLocked(def, run, act)
		default:
			e.Log.Printf("unknown action %T", a)
		}
	}
}

func (e *Engine) finishLocked(def *core.WorkflowDefinition, run core.Run, act core.ActionFinish) {
	e.Store.DisarmRunTimers(run.ID)
	e.event(run.ID, act.State, "run-finished", map[string]any{"terminal": act.State}, run.Workspace)
	e.admitNextLocked(def, run.Workspace, run.Workflow)
}

// admitNextLocked pulls the oldest queued run into the freed slot.
func (e *Engine) admitNextLocked(def *core.WorkflowDefinition, ws, wf string) {
	if def.Concurrency <= 0 {
		return
	}
	active, err := e.Store.CountActive(ws, wf)
	if err != nil || active >= def.Concurrency {
		return
	}
	next, err := e.Store.NextQueued(ws, wf)
	if err != nil || next == nil {
		return
	}
	e.admitLocked(def, next)
}

// --- gates ---

func (e *Engine) openGateLocked(run core.Run, act core.ActionOpenGate) {
	g := &core.Gate{
		ID: newID("g"), Workspace: run.Workspace, RunID: run.ID, State: act.State,
		Kind: act.Kind, Payload: act.Payload, Options: act.Options,
		Status: core.GateOpen, CreatedAt: e.Clock.Now(),
	}
	if err := e.Store.CreateGate(g); err != nil {
		e.Log.Printf("create gate: %v", err)
		return
	}
	e.event(run.ID, act.State, "gate-opened", map[string]any{"gate": g.ID, "kind": string(act.Kind), "payload": act.Payload}, run.Workspace)
	e.deliver(g)
}

func (e *Engine) deliver(g *core.Gate) {
	delivery := map[string]string{}
	for _, ch := range e.Channels {
		if err := ch.Deliver(g); err != nil {
			delivery[ch.Name()] = err.Error()
			e.Log.Printf("deliver gate %s via %s: %v", g.ID, ch.Name(), err)
			e.event(g.RunID, g.State, "gate-delivery-failed", map[string]any{"gate": g.ID, "channel": ch.Name(), "error": err.Error()}, g.Workspace)
		} else {
			delivery[ch.Name()] = "ok"
			e.event(g.RunID, g.State, "gate-delivered", map[string]any{"gate": g.ID, "channel": ch.Name()}, g.Workspace)
		}
	}
	if err := e.Store.SetGateDelivery(g.ID, delivery); err != nil {
		e.Log.Printf("record delivery %s: %v", g.ID, err)
	}
}

// parkLocked flips the run to needs-attention and raises the fixed gate.
func (e *Engine) parkLocked(run core.Run, reason string) {
	run.Status = core.StatusNeedsAttention
	run.UpdatedAt = e.Clock.Now()
	if err := e.Store.SaveRun(&run); err != nil {
		e.Log.Printf("park %s: %v", run.ID, err)
	}
	e.Store.DisarmRunTimers(run.ID)
	e.event(run.ID, run.State, "parked", map[string]any{"reason": reason}, run.Workspace)
	e.openGateLocked(run, core.ActionOpenGate{
		State: run.State, Kind: core.GateChoice,
		Payload: "needs attention: " + reason,
		Options: core.AttentionOptions,
	})
}

// AnswerGate resolves an open gate (first response wins) and advances the run.
func (e *Engine) AnswerGate(gateID, response string, answers map[string]any, responder string) error {
	e.mu.Lock()
	err := e.answerGateLocked(gateID, response, answers, responder)
	e.mu.Unlock()
	e.drain()
	return err
}

func (e *Engine) answerGateLocked(gateID, response string, answers map[string]any, responder string) error {
	g, err := e.Store.GetGate(gateID)
	if err != nil {
		return fmt.Errorf("gate %s: %w", gateID, err)
	}
	won, err := e.Store.ResolveGate(gateID, response, answers, responder, e.Clock.Now())
	if err != nil {
		return err
	}
	if !won {
		e.event(g.RunID, g.State, "gate-late-response", map[string]any{"gate": gateID, "response": response, "responder": responder}, g.Workspace)
		return fmt.Errorf("gate %s already resolved", gateID)
	}
	e.event(g.RunID, g.State, "gate-answered", map[string]any{"gate": gateID, "response": response, "responder": responder}, g.Workspace)

	if g.RunID == "" {
		return nil // engine-level gate (e.g. watcher failures): ack is enough
	}
	run, err := e.Store.GetRun(g.RunID)
	if err != nil {
		return err
	}
	_, def, err := e.def(run.Workspace, run.Workflow)
	if err != nil {
		return err
	}

	if run.Status == core.StatusNeedsAttention {
		d := core.ResolveAttention(def, *run, response)
		e.applyLocked(def, d)
		return nil
	}

	// Normal gate: outcome depends on gate kind.
	outcome := response
	result := map[string]any{"response": response}
	if g.Kind == core.GateQuestion {
		outcome = "answered"
		// Answers ACCUMULATE across every round of this gate for the life
		// of the run — each round asks new questions, but earlier decisions
		// stay settled (found live: round 3 overwrote round 1's answers and
		// the verifier re-asked them verbatim).
		merged := map[string]any{}
		if prev, ok := run.Context[g.State].(map[string]any); ok {
			if prevAnswers, ok := prev["answers"].(map[string]any); ok {
				for k, v := range prevAnswers {
					merged[k] = v
				}
			}
		}
		for k, v := range answers {
			merged[k] = v
		}
		result["answers"] = merged
	}
	if g.Kind == core.GateApproval && response != "approved" {
		outcome = "rejected"
	}
	d := core.Advance(def, *run, outcome, result)
	e.applyLocked(def, d)
	return nil
}

// --- agent steps ---

func (e *Engine) startAgentStepLocked(def *core.WorkflowDefinition, run core.Run, act core.ActionRunAgent) {
	prior, _ := e.Store.StepsForRun(run.ID)
	attempt := 1
	for _, p := range prior {
		if p.State == act.State {
			attempt++
		}
	}
	st := &core.StepExecution{
		RunID: run.ID, State: act.State, Kind: core.StepAgent, Attempt: attempt,
		Inputs: act.Inputs, StartedAt: e.Clock.Now(),
	}
	if err := e.Store.InsertStep(st); err != nil {
		e.Log.Printf("insert step: %v", err)
		return
	}
	e.event(run.ID, act.State, "step-started", map[string]any{"step": st.ID, "agent": act.Agent, "attempt": attempt}, run.Workspace)

	ws := e.Workspaces[run.Workspace]
	spec := ws.Agents[act.Agent]
	// Working folders may be templates ("$input.repo") and resolve through
	// the workspace repos: map — the refiner must follow the repo input,
	// never a hardcoded clone.
	if spec.WorkingFolder != "" {
		folder := core.Interpolate(&run, spec.WorkingFolder)
		if mapped, ok := ws.Repos[folder]; ok {
			folder = mapped
		}
		if folder != spec.WorkingFolder {
			cp := *spec
			cp.WorkingFolder = folder
			spec = &cp
		}
	}
	skills := map[string]string{}
	for _, sk := range spec.Skills {
		skills[sk] = ws.Skills[sk]
	}
	base := filepath.Join(e.DataDir, "runs", run.ID, fmt.Sprintf("%s-attempt-%d", act.State, attempt))
	req := runner.Request{
		RunID: run.ID, State: act.State, Spec: spec,
		Instruction: act.Instruction, Inputs: act.Inputs, Skills: skills,
		BaseDir: base, HandoffDir: filepath.Join(base, "handoff"),
	}
	e.pending = append(e.pending, func() { e.executeStep(def, run.ID, st, req, act) })
}

// executeStep runs outside the lock (agent steps take minutes), re-entering
// via stepCompleted. It owns the schema-violation resume-once policy.
func (e *Engine) executeStep(def *core.WorkflowDefinition, runID string, st *core.StepExecution, req runner.Request, act core.ActionRunAgent) {
	defer e.drain() // completion may queue the next agent step
	st.HandoffDir = req.HandoffDir
	res, err := e.Runner.Run(context.Background(), req)
	if err != nil {
		e.stepFailed(def, runID, st, act, fmt.Sprintf("runner: %v", err))
		return
	}
	st.SessionID, st.SessionPath, st.CostUSD = res.SessionID, res.SessionPath, res.CostUSD

	result, verr := readResult(req.HandoffDir, def, act.State)
	if verr != "" && res.SessionID != "" && req.Resume == "" {
		// resume the same session once with the validation error appended
		e.mu.Lock()
		e.event(runID, act.State, "schema-violation", map[string]any{"step": st.ID, "agent": act.Agent, "error": verr}, def.Workspace)
		e.mu.Unlock()
		req.Resume = res.SessionID
		req.ResumeMessage = "Your result.json was rejected: " + verr +
			"\nWrite a corrected $KRAKOA_HANDOFF/result.json now."
		res2, err2 := e.Runner.Run(context.Background(), req)
		if err2 != nil {
			e.stepFailed(def, runID, st, act, fmt.Sprintf("runner (schema retry): %v", err2))
			return
		}
		st.CostUSD += res2.CostUSD
		result, verr = readResult(req.HandoffDir, def, act.State)
	}
	if verr != "" {
		e.stepFailed(def, runID, st, act, "invalid result after resume: "+verr)
		return
	}
	if req.Spec.Worktree {
		runner.CleanupWorktree(req.Spec.WorkingFolder, filepath.Join(req.BaseDir, "worktree"))
	}
	e.stepCompleted(def, runID, st, result)
}

// readResult loads and checks handoff/result.json: valid JSON object whose
// "outcome" maps to a transition of the state. That IS the contract the
// engine routes on; field-level schemas ride in the prompt.
func readResult(handoffDir string, def *core.WorkflowDefinition, state string) (map[string]any, string) {
	raw, err := os.ReadFile(filepath.Join(handoffDir, "result.json"))
	if err != nil {
		return nil, "result.json not found in handoff dir"
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "result.json is not valid JSON: " + err.Error()
	}
	outcome, _ := m["outcome"].(string)
	if outcome == "" {
		return nil, `result.json is missing the "outcome" string field`
	}
	if _, ok := def.States[state].On[outcome]; !ok {
		return nil, fmt.Sprintf("outcome %q is not one of this step's transitions %v", outcome, keys(def.States[state].On))
	}
	return m, ""
}

func (e *Engine) stepCompleted(def *core.WorkflowDefinition, runID string, st *core.StepExecution, result map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	outcome := result["outcome"].(string)
	st.Outcome, st.Result, st.EndedAt = outcome, result, e.Clock.Now()
	e.Store.UpdateStep(st)
	e.event(runID, st.State, "step-finished", map[string]any{"step": st.ID, "outcome": outcome, "cost_usd": st.CostUSD, "session": st.SessionID}, def.Workspace)

	run, err := e.Store.GetRun(runID)
	if err != nil {
		e.Log.Printf("step done, run gone: %v", err)
		return
	}
	if run.State != st.State || run.Status != core.StatusRunning {
		e.event(runID, st.State, "step-result-stale", map[string]any{"step": st.ID}, def.Workspace)
		return // manual intervention moved the run; don't double-advance
	}
	d := core.Advance(def, *run, outcome, result)
	e.applyLocked(def, d)
}

// stepFailed applies the retry knob, then parks as needs-attention.
func (e *Engine) stepFailed(def *core.WorkflowDefinition, runID string, st *core.StepExecution, act core.ActionRunAgent, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st.Error, st.EndedAt = reason, e.Clock.Now()
	e.Store.UpdateStep(st)
	e.event(runID, st.State, "step-failed", map[string]any{"step": st.ID, "attempt": st.Attempt, "error": reason}, def.Workspace)

	run, err := e.Store.GetRun(runID)
	if err != nil || run.State != st.State || run.Status != core.StatusRunning {
		return
	}
	if st.Attempt <= act.Retry {
		e.event(runID, st.State, "step-retry", map[string]any{"attempt": st.Attempt + 1}, def.Workspace)
		e.startAgentStepLocked(def, *run, act)
		return
	}
	e.parkLocked(*run, fmt.Sprintf("step %s failed after %d attempt(s): %s", st.State, st.Attempt, reason))
}

// --- wait states, events, signals ---

func (e *Engine) armWaitLocked(def *core.WorkflowDefinition, run core.Run, act core.ActionArmWait) {
	// Buffered signals first: a matching one consumes immediately (U1 edge:
	// builder finished while the run was queued).
	sigs, _ := e.Store.PendingSignals(run.ID)
	for _, sig := range sigs {
		for _, arm := range act.Arms {
			if arm.Event != "" && arm.Event == sig.Event {
				e.Store.ConsumeSignal(sig.ID)
				e.event(run.ID, act.State, "signal-consumed", map[string]any{"event": sig.Event}, run.Workspace)
				d := core.Advance(def, run, sig.Event, sig.Payload)
				e.applyLocked(def, d)
				return
			}
		}
	}
	now := e.Clock.Now()
	for _, arm := range act.Arms {
		switch {
		case arm.Timeout > 0:
			e.Store.ArmTimer(&store.Timer{RunID: run.ID, State: act.State, Kind: "timeout", FireAt: now.Add(arm.Timeout.D())})
		case arm.Probe != nil:
			payload, _ := json.Marshal(arm.Probe)
			var p map[string]any
			json.Unmarshal(payload, &p)
			e.Store.ArmTimer(&store.Timer{RunID: run.ID, State: act.State, Kind: "probe", FireAt: now.Add(arm.Probe.Every.D()), Every: arm.Probe.Every.D(), Payload: p})
		}
	}
	e.event(run.ID, act.State, "wait-armed", map[string]any{"arms": len(act.Arms)}, run.Workspace)
}

// EmittedEvent is one observation from a watcher probe or an external emit.
type EmittedEvent struct {
	Event   string         `json:"event"`
	Key     string         `json:"key,omitempty"`
	RunID   string         `json:"run,omitempty"` // direct routing (krakoactl emit --run)
	Payload map[string]any `json:"payload,omitempty"`
}

// HandleEmit routes one event: direct run targeting, waiting-run correlation,
// mid-state buffering — and, exactly like a watcher observation, an event a
// spawn-mode watcher declares interest in spawns a run (dedupe included).
// Manual emit is the operator's replay/recovery tool; it must not be weaker
// than the watcher path. Returns what happened (for logs).
func (e *Engine) HandleEmit(wsName string, ev EmittedEvent) string {
	e.mu.Lock()
	out := e.routeEventLocked(wsName, ev)
	if out == "no matching run" {
		if spawned := e.spawnFromEventLocked(wsName, ev); spawned != "" {
			out = spawned
		}
	}
	e.mu.Unlock()
	e.drain()
	return out
}

// spawnFromEventLocked spawns a run for an event some spawn-mode watcher
// declares (dedupe against that watcher's keys). Empty string = no taker.
func (e *Engine) spawnFromEventLocked(wsName string, ev EmittedEvent) string {
	ws := e.Workspaces[wsName]
	if ws == nil {
		return ""
	}
	names := make([]string, 0, len(ws.Watchers))
	for n := range ws.Watchers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w := ws.Watchers[n]
		if w.Mode != "spawn" || !spawnable(w, ev.Event) {
			continue
		}
		if ev.Key != "" {
			if seen, _ := e.Store.DedupeSeen(n, ev.Key); seen {
				return "duplicate (already handled by watcher " + n + ")"
			}
		}
		run, err := e.startRunLocked(wsName, w.Workflow, ev.Payload, "")
		if err != nil {
			return "spawn failed: " + err.Error()
		}
		if ev.Key != "" {
			e.Store.DedupeMark(n, ev.Key, e.Clock.Now())
		}
		return "spawned " + run.ID
	}
	return ""
}

func (e *Engine) routeEventLocked(wsName string, ev EmittedEvent) string {
	e.event(ev.RunID, "", "emit", map[string]any{"event": ev.Event, "key": ev.Key}, wsName)

	if ev.RunID != "" {
		run, err := e.Store.GetRun(ev.RunID)
		if err != nil {
			return "unknown run"
		}
		return e.eventToRunLocked(run, ev)
	}

	// correlation scan over the workspace's active runs
	active, _ := e.Store.ListRuns(core.StatusRunning, core.StatusWaiting, core.StatusGated, core.StatusQueued, core.StatusNeedsAttention)
	for _, run := range active {
		if run.Workspace != wsName {
			continue
		}
		_, def, err := e.def(run.Workspace, run.Workflow)
		if err != nil {
			continue
		}
		if matchRun(def, run, ev) {
			return e.eventToRunLocked(run, ev)
		}
	}
	return "no matching run"
}

// matchRun reports whether any wait state in the run's definition has an arm
// for this event whose resolved correlation key matches.
func matchRun(def *core.WorkflowDefinition, run *core.Run, ev EmittedEvent) bool {
	for _, st := range def.States {
		for _, arm := range st.Arms {
			if arm.Event != ev.Event {
				continue
			}
			if arm.Correlate == "" {
				return true
			}
			v, err := core.Resolve(run, arm.Correlate)
			if err == nil && fmt.Sprintf("%v", v) == ev.Key {
				return true
			}
		}
	}
	return false
}

// eventToRunLocked advances a waiting run or buffers for later.
func (e *Engine) eventToRunLocked(run *core.Run, ev EmittedEvent) string {
	_, def, err := e.def(run.Workspace, run.Workflow)
	if err != nil {
		return err.Error()
	}
	if run.Status == core.StatusWaiting {
		if _, ok := def.States[run.State].On[ev.Event]; ok {
			e.Store.DisarmRunTimers(run.ID)
			d := core.Advance(def, *run, ev.Event, ev.Payload)
			e.applyLocked(def, d)
			return "advanced " + run.ID
		}
	}
	if run.Status.Terminal() {
		return "run finished; dropped"
	}
	e.Store.BufferSignal(run.ID, ev.Event, ev.Payload, e.Clock.Now())
	e.event(run.ID, run.State, "signal-buffered", map[string]any{"event": ev.Event}, run.Workspace)
	return "buffered for " + run.ID
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }
