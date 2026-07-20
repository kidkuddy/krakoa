package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
	"github.com/kidkuddy/krakoa/internal/runner"
	"github.com/kidkuddy/krakoa/internal/store"
)

// Tick fires every due timer. The daemon calls it on an interval; tests call
// it after advancing a fake clock. Everything it needs comes from the store,
// so a restart loses nothing.
func (e *Engine) Tick() {
	e.mu.Lock()
	due, err := e.Store.DueTimers(e.Clock.Now())
	e.mu.Unlock()
	if err != nil {
		e.Log.Printf("due timers: %v", err)
		return
	}
	for _, t := range due {
		switch t.Kind {
		case "timeout":
			e.fireTimeout(t)
		case "probe":
			e.fireProbe(t)
		case "watcher":
			e.fireWatcher(t)
		default:
			e.Log.Printf("unknown timer kind %q (id %d)", t.Kind, t.ID)
			e.mu.Lock()
			e.Store.DisarmTimer(t.ID)
			e.mu.Unlock()
		}
	}
}

func (e *Engine) fireTimeout(t *store.Timer) {
	defer e.drain()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Store.DisarmTimer(t.ID)
	run, err := e.Store.GetRun(t.RunID)
	if err != nil || run.Status != core.StatusWaiting || run.State != t.State {
		return // stale timer; the run moved on
	}
	_, def, err := e.def(run.Workspace, run.Workflow)
	if err != nil {
		return
	}
	e.Store.DisarmRunTimers(run.ID)
	e.event(run.ID, run.State, "timer-fired", map[string]any{"kind": "timeout"}, run.Workspace)
	d := core.Advance(def, *run, "timeout", nil)
	e.applyLocked(def, d)
}

// fireProbe runs the wait arm's probe agent; a recognized outcome advances
// the run, anything else just waits for the next cadence. The timeout arm is
// the backstop for a permanently confused probe.
func (e *Engine) fireProbe(t *store.Timer) {
	e.mu.Lock()
	run, err := e.Store.GetRun(t.RunID)
	if err != nil || run.Status != core.StatusWaiting || run.State != t.State {
		e.Store.DisarmTimer(t.ID)
		e.mu.Unlock()
		return
	}
	e.Store.Reschedule(t.ID, e.Clock.Now().Add(t.Every))
	_, def, err := e.def(run.Workspace, run.Workflow)
	if err != nil {
		e.mu.Unlock()
		return
	}
	var probe core.ProbeSpec
	raw, _ := json.Marshal(t.Payload)
	json.Unmarshal(raw, &probe)
	ws := e.Workspaces[run.Workspace]

	if probe.Command != "" {
		wsPath := ws.Path
		command := core.Interpolate(run, probe.Command)
		e.mu.Unlock()
		e.Spawn(func() { e.runCommandProbe(def, t, wsPath, command) })
		return
	}

	spec := ws.Agents[probe.Agent]
	if spec == nil {
		e.mu.Unlock()
		return
	}
	skills := map[string]string{}
	for _, sk := range spec.Skills {
		skills[sk] = ws.Skills[sk]
	}
	base := filepath.Join(e.DataDir, "runs", run.ID, fmt.Sprintf("probe-%s-%d", t.State, e.Clock.Now().UnixNano()))
	req := runner.Request{
		RunID: run.ID, State: t.State, Spec: spec,
		// probe instructions may reference run context ($merging.merge_sha)
		Instruction: probePreamble + core.Interpolate(run, probe.Instruction),
		Inputs:      map[string]any{}, Skills: skills,
		BaseDir: base, HandoffDir: filepath.Join(base, "handoff"),
	}
	e.mu.Unlock()

	e.Spawn(func() {
		defer e.drain()
		res, err := e.Runner.Run(context.Background(), req)
		if err != nil {
			e.mu.Lock()
			e.event(run.ID, t.State, "probe-failed", map[string]any{"error": err.Error()}, run.Workspace)
			e.mu.Unlock()
			return
		}
		result, verr := readResult(req.HandoffDir, def, t.State)
		e.mu.Lock()
		defer e.mu.Unlock()
		cur, err := e.Store.GetRun(t.RunID)
		if err != nil || cur.Status != core.StatusWaiting || cur.State != t.State {
			return // world moved while probing
		}
		if verr != "" {
			// "pending" is the probe's honest "not yet"; anything else
			// unparseable is a derailed probe — both wait for the cadence,
			// but they must be distinguishable in the event log.
			kind := "probe-failed"
			if raw, rerr := readFile(filepath.Join(req.HandoffDir, "result.json")); rerr == nil {
				var m map[string]any
				if json.Unmarshal(raw, &m) == nil {
					if o, _ := m["outcome"].(string); o == "pending" {
						kind = "probe-pending"
					}
				}
			}
			e.event(run.ID, t.State, kind, map[string]any{"detail": verr, "session": res.SessionID, "cost_usd": res.CostUSD}, run.Workspace)
			return
		}
		e.Store.DisarmRunTimers(run.ID)
		e.event(run.ID, t.State, "probe-outcome", map[string]any{"outcome": result["outcome"], "session": res.SessionID, "cost_usd": res.CostUSD}, run.Workspace)
		d := core.Advance(def, *cur, result["outcome"].(string), result)
		e.applyLocked(def, d)
	})
}

// runCommandProbe executes a deterministic wait-arm probe: direct exec,
// structurally $0. Outcomes route like agent probes ("pending" = not yet);
// non-zero exit or invalid JSON is a probe failure the cadence retries and
// the timeout arm backstops.
func (e *Engine) runCommandProbe(def *core.WorkflowDefinition, t *store.Timer, wsPath, command string) {
	defer e.drain()
	start := time.Now()
	out, err := e.Exec(wsPath, command)
	durMS := time.Since(start).Milliseconds()
	var result map[string]any
	if err == nil {
		err = json.Unmarshal(out, &result)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	cur, gerr := e.Store.GetRun(t.RunID)
	if gerr != nil || cur.Status != core.StatusWaiting || cur.State != t.State {
		return // world moved while probing
	}
	outcome, _ := result["outcome"].(string)
	base := map[string]any{"command": command, "duration_ms": durMS, "cost_usd": 0}
	switch {
	case err != nil || outcome == "":
		detail := snippet(out)
		if err != nil {
			detail = err.Error() + " " + detail
		}
		base["error"] = detail
		e.event(t.RunID, t.State, "probe-failed", base, cur.Workspace)
	case outcome == "pending":
		e.event(t.RunID, t.State, "probe-pending", base, cur.Workspace)
	default:
		if _, ok := def.States[t.State].On[outcome]; !ok {
			base["error"] = fmt.Sprintf("outcome %q not in transitions", outcome)
			e.event(t.RunID, t.State, "probe-failed", base, cur.Workspace)
			return
		}
		e.Store.DisarmRunTimers(t.RunID)
		base["outcome"] = outcome
		e.event(t.RunID, t.State, "probe-outcome", base, cur.Workspace)
		d := core.Advance(def, *cur, outcome, result)
		e.applyLocked(def, d)
	}
}

const probePreamble = `Execute ONE probe check now, exactly as instructed
below. Do not ask questions — there is no interlocutor. If the condition is
not yet decided, write {"outcome":"pending"} to $KRAKOA_HANDOFF/result.json.
Run the commands, write the result, and stop.

`

// fireWatcher runs the watcher's probe agent and routes its emitted events.
func (e *Engine) fireWatcher(t *store.Timer) {
	name, _ := t.Payload["watcher"].(string)
	wsName, _ := t.Payload["workspace"].(string)
	e.mu.Lock()
	ws, ok := e.Workspaces[wsName]
	if !ok || ws.Watchers[name] == nil {
		e.Store.DisarmTimer(t.ID)
		e.mu.Unlock()
		return
	}
	w := ws.Watchers[name]
	// reschedule on the CURRENT spec cadence, not the one stored when the
	// timer was first armed — workspace edits to `every` apply on reload
	e.Store.Reschedule(t.ID, e.Clock.Now().Add(w.Every.D()))
	if w.Command != "" {
		wsPath := ws.Path
		command := w.Command
		e.mu.Unlock()
		e.Spawn(func() { e.runCommandSweep(wsName, name, wsPath, command) })
		return
	}
	spec := ws.Agents[w.Agent]
	skills := map[string]string{}
	for _, sk := range spec.Skills {
		skills[sk] = ws.Skills[sk]
	}
	base := filepath.Join(e.DataDir, "watchers", name, fmt.Sprintf("%d", e.Clock.Now().UnixNano()))
	req := runner.Request{
		RunID: "", State: "watcher:" + name, Spec: spec,
		Instruction: watcherPreamble + w.Instruction + watcherProtocol,
		Skills:      skills,
		BaseDir:     base, HandoffDir: filepath.Join(base, "handoff"),
	}
	e.mu.Unlock()

	e.Spawn(func() { e.runWatcherSweep(wsName, name, req) })
}

// runCommandSweep executes a deterministic command watcher: direct exec, no
// session, structurally $0. Non-zero exit or schema-invalid stdout is an
// unambiguous failure — same retry-once + strike-gate semantics as agents.
func (e *Engine) runCommandSweep(wsName, name, wsPath, command string) {
	attempt := func() ([]EmittedEvent, bool) {
		start := time.Now()
		out, err := e.Exec(wsPath, command)
		durMS := time.Since(start).Milliseconds()
		var parsed struct {
			Outcome string         `json:"outcome"`
			Events  []EmittedEvent `json:"events"`
		}
		if err == nil {
			err = json.Unmarshal(out, &parsed)
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if err != nil || parsed.Outcome == "" {
			detail := snippet(out)
			if err != nil {
				detail = err.Error() + " " + detail
			}
			e.event("", "watcher:"+name, "watcher-failed", map[string]any{
				"error": detail, "command": command, "duration_ms": durMS}, wsName)
			return nil, false
		}
		e.event("", "watcher:"+name, "watcher-swept", map[string]any{
			"observations": len(parsed.Events), "command": command, "duration_ms": durMS, "cost_usd": 0}, wsName)
		return parsed.Events, true
	}

	events, ok := attempt()
	if !ok {
		events, ok = attempt()
	}
	e.settleSweep(wsName, name, events, ok, 0)
}

// runWatcherSweep executes one sweep with failure semantics: no schema-valid
// result.json is a FAILURE (a derailed agent), never an empty observation.
// One immediate retry; WatcherFailLimit consecutive failures raise a gate.
func (e *Engine) runWatcherSweep(wsName, name string, req runner.Request) {
	var cost float64
	attempt := func(r runner.Request) ([]EmittedEvent, bool) {
		res, err := e.Runner.Run(context.Background(), r)
		if err != nil {
			e.mu.Lock()
			e.event("", "watcher:"+name, "watcher-failed", map[string]any{"error": err.Error()}, wsName)
			e.mu.Unlock()
			return nil, false
		}
		cost += res.CostUSD
		events, ok := readWatcherEvents(r.HandoffDir)
		if !ok {
			e.mu.Lock()
			e.event("", "watcher:"+name, "watcher-failed", map[string]any{
				"error": "no schema-valid result.json (agent derailed?)", "session": res.SessionID, "cost_usd": res.CostUSD}, wsName)
			e.mu.Unlock()
			return nil, false
		}
		e.mu.Lock()
		e.event("", "watcher:"+name, "watcher-swept", map[string]any{
			"observations": len(events), "session": res.SessionID, "cost_usd": res.CostUSD}, wsName)
		e.mu.Unlock()
		return events, true
	}

	events, ok := attempt(req)
	if !ok {
		retry := req
		retry.BaseDir = req.BaseDir + "-retry"
		retry.HandoffDir = filepath.Join(retry.BaseDir, "handoff")
		events, ok = attempt(retry)
	}
	e.settleSweep(wsName, name, events, ok, cost)
}

// settleSweep applies the shared post-sweep semantics: success routes the
// observations and resets the strike count; failure counts a strike and
// raises an engine-level gate at the limit.
func (e *Engine) settleSweep(wsName, name string, events []EmittedEvent, ok bool, cost float64) {
	e.mu.Lock()
	key := wsName + "/" + name
	if ok {
		e.watcherFails[key] = 0
		e.mu.Unlock()
		e.HandleWatcherEvents(wsName, name, events)
		return
	}
	e.watcherFails[key]++
	fails := e.watcherFails[key]
	if fails >= WatcherFailLimit {
		e.watcherFails[key] = 0
		e.openGateLocked(core.Run{ID: "", Workspace: wsName, State: "watcher:" + name}, core.ActionOpenGate{
			State: "watcher:" + name, Kind: core.GateChoice,
			Payload: fmt.Sprintf("watcher %s failed %d consecutive sweeps — investigate via the watcher-failed events (sweep cost so far $%.4f)", name, fails, cost),
			Options: []string{"acknowledged"},
		})
	}
	e.mu.Unlock()
	e.drain()
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// WatcherFailLimit is how many consecutive derailed sweeps raise a gate.
const WatcherFailLimit = 3

const watcherPreamble = `Execute ONE sweep now, exactly as your skill
prescribes. Do not ask questions — there is no interlocutor and nobody will
ever reply. Do not introduce yourself. Run the commands, write
$KRAKOA_HANDOFF/result.json, and stop.

`

const watcherProtocol = `

Report observations by writing $KRAKOA_HANDOFF/result.json:
{"outcome":"ok","events":[{"event":"<name>","key":"<dedupe key>","payload":{...}}]}
No observations = empty events list. The key must change when the observed
object changes (e.g. include the head SHA).`

// readWatcherEvents returns (events, ok): ok=false means the sweep produced
// no parseable result at all — a failure, distinct from zero observations.
func readWatcherEvents(handoffDir string) ([]EmittedEvent, bool) {
	var parsed struct {
		Outcome string         `json:"outcome"`
		Events  []EmittedEvent `json:"events"`
	}
	raw, err := readFile(filepath.Join(handoffDir, "result.json"))
	if err != nil {
		return nil, false
	}
	if json.Unmarshal(raw, &parsed) != nil || parsed.Outcome == "" {
		return nil, false
	}
	return parsed.Events, true
}

// HandleWatcherEvents routes a watcher sweep's observations. watch_dedupe is
// SPAWN protection only: a spawnable event's key is checked and marked
// around the spawn, so one observation spawns one run. Correlation (resume)
// events never touch dedupe — a ticket goes ready on EVERY review round, and
// a consumed mark would swallow all but the first (found live, CAL-653);
// their idempotence comes from run-state routing + the duplicate-buffer
// guard.
func (e *Engine) HandleWatcherEvents(wsName, watcherName string, events []EmittedEvent) {
	defer e.drain()
	e.mu.Lock()
	defer e.mu.Unlock()
	ws := e.Workspaces[wsName]
	if ws == nil {
		return
	}
	w := ws.Watchers[watcherName]
	for _, ev := range events {
		spawnEvent := w != nil && w.Mode == "spawn" && spawnable(w, ev.Event)
		if spawnEvent && ev.Key != "" {
			if seen, _ := e.Store.DedupeSeen(watcherName, ev.Key); seen {
				continue
			}
		}
		disposition := e.routeEventLocked(wsName, ev)
		if disposition == "no matching run" && spawnEvent {
			run, err := e.startRunLocked(wsName, w.Workflow, ev.Payload, "")
			if err != nil {
				e.Log.Printf("watcher %s spawn: %v", watcherName, err)
				continue
			}
			disposition = "spawned " + run.ID
		}
		e.event("", "watcher:"+watcherName, "watcher-observed", map[string]any{"event": ev.Event, "key": ev.Key, "disposition": disposition}, wsName)
		if spawnEvent && ev.Key != "" && disposition != "no matching run" && disposition != "run finished; dropped" {
			e.Store.DedupeMark(watcherName, ev.Key, e.Clock.Now())
		}
	}
}

func spawnable(w *core.WatcherSpec, event string) bool {
	if len(w.SpawnOn) == 0 {
		return true
	}
	for _, s := range w.SpawnOn {
		if s == event {
			return true
		}
	}
	return false
}

// --- startup ---

// Recover re-arms the world from the store after a restart: watcher timers
// exist, crashed agent steps re-enter their state (check-first skills make
// the re-attempt safe), queued runs fill free slots. Wait-state timers are
// already durable rows, picked up by the next Tick.
func (e *Engine) Recover() {
	defer e.drain()
	e.mu.Lock()
	defer e.mu.Unlock()

	e.armWatchersLocked()

	runs, err := e.Store.ListRuns(core.StatusRunning)
	if err != nil {
		e.Log.Printf("recover: %v", err)
		return
	}
	for _, run := range runs {
		_, def, err := e.def(run.Workspace, run.Workflow)
		if err != nil {
			e.Log.Printf("recover %s: %v (workspace gone or invalid — parking)", run.ID, err)
			e.parkLocked(*run, "recovery: "+err.Error())
			continue
		}
		e.event(run.ID, run.State, "recovered", map[string]any{"action": "re-enter state"}, run.Workspace)
		d := core.Enter(def, *run)
		e.applyLocked(def, d)
	}

	// fill freed slots per workflow
	for _, ws := range e.Workspaces {
		for _, def := range ws.Workflows {
			e.admitNextLocked(def, ws.Name, def.Name)
		}
	}
}

// armWatchersLocked ensures every workspace watcher has one active timer.
func (e *Engine) armWatchersLocked() {
	timers, err := e.Store.ActiveTimers()
	if err != nil {
		e.Log.Printf("arm watchers: %v", err)
		return
	}
	have := map[string]bool{}
	for _, t := range timers {
		if t.Kind == "watcher" {
			name, _ := t.Payload["watcher"].(string)
			wsName, _ := t.Payload["workspace"].(string)
			have[wsName+"/"+name] = true
		}
	}
	for _, ws := range e.Workspaces {
		for _, w := range ws.Watchers {
			if have[ws.Name+"/"+w.Name] {
				continue
			}
			e.Store.ArmTimer(&store.Timer{
				Kind: "watcher", FireAt: e.Clock.Now(), Every: w.Every.D(),
				Payload: map[string]any{"watcher": w.Name, "workspace": ws.Name},
			})
		}
	}
}

// RunForever is the daemon loop: tick on an interval until ctx ends.
func (e *Engine) RunForever(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.Tick()
		}
	}
}
