package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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
	e.sweepStalledSignals()
}

// StallGuardAfter is how long a buffered signal may sit unusable before the
// guard asks a human.
const StallGuardAfter = 30 * time.Minute

const stallSweepEvery = 5 * time.Minute

// stallStatePrefix marks a gate raised for a stuck buffered signal; the event
// name follows it.
const stallStatePrefix = "signal-stall:"

// StallOptions are the choices on a stalled-signal gate.
var StallOptions = []string{"advance", "discard"}

// sweepStalledSignals raises one gate per run holding a buffered signal that
// no state reachable from where it stands can ever consume. Buffered signals
// are only re-checked against the arms of the state a run enters NEXT, so one
// buffered in a state the machine has already left is invisible forever —
// three lifecycles stalled that way on mr-ready, and this kills the class.
func (e *Engine) sweepStalledSignals() {
	defer e.drain()
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Clock.Now()
	if now.Sub(e.lastStallSweep) < stallSweepEvery {
		return
	}
	e.lastStallSweep = now

	runs, err := e.Store.ListRuns(core.StatusWaiting, core.StatusGated)
	if err != nil {
		return
	}
	for _, run := range runs {
		// one open question per run at a time — this also dedupes our own gate
		if g, _ := e.Store.OpenGateForRun(run.ID); g != nil {
			continue
		}
		_, def, err := e.def(run.Workspace, run.Workflow)
		if err != nil {
			continue
		}
		sigs, _ := e.Store.PendingSignals(run.ID)
		for _, sig := range sigs {
			// The state the run stands in arms this event right now, yet the
			// signal is still buffered: it arrived before the state was
			// entered, or a workflow edit added the arm afterwards. Arming is
			// the only other place buffered signals drain, and an armed wait
			// never re-arms — so without this they sit forever.
			if run.Status == core.StatusWaiting && armsEvent(def.States[run.State], sig.Event) {
				e.Store.ConsumeSignal(sig.ID)
				e.Store.DisarmRunTimers(run.ID)
				e.event(run.ID, run.State, "signal-consumed", map[string]any{"event": sig.Event, "via": "stall-sweep"}, run.Workspace)
				d := core.Advance(def, *run, sig.Event, sig.Payload)
				e.applyLocked(def, d)
				break // the run moved; its remaining signals settle on the next sweep
			}
			if now.Sub(sig.At) < StallGuardAfter || consumableFrom(def, run.State, sig.Event) {
				continue
			}
			e.openGateLocked(*run, core.ActionOpenGate{
				State: stallStatePrefix + sig.Event,
				Kind:  core.GateChoice,
				Payload: fmt.Sprintf("%s has held a %q signal since %s and no state reachable from %q can consume it — the run cannot move on its own. advance = jump to the state that handles it and continue there; discard = drop the signal and keep waiting.",
					run.ID, sig.Event, sig.At.Format("Mon 15:04"), run.State),
				Options: StallOptions,
			})
			break
		}
	}
}

// armsEvent reports whether this state waits on that event.
func armsEvent(st core.State, event string) bool {
	if _, ok := st.On[event]; !ok {
		return false
	}
	for _, arm := range st.Arms {
		if arm.Event == event {
			return true
		}
	}
	return false
}

// consumableFrom reports whether any state reachable from `from` (itself
// included) arms this event.
func consumableFrom(def *core.WorkflowDefinition, from, event string) bool {
	seen := map[string]bool{from: true}
	for queue := []string{from}; len(queue) > 0; queue = queue[1:] {
		st, ok := def.States[queue[0]]
		if !ok {
			continue
		}
		for _, arm := range st.Arms {
			if arm.Event == event {
				return true
			}
		}
		for _, next := range st.On {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// stateArming names the state that handles this event, deterministically.
func stateArming(def *core.WorkflowDefinition, event string) string {
	names := make([]string, 0, len(def.States))
	for n := range def.States {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		st := def.States[n]
		if _, ok := st.On[event]; !ok {
			continue
		}
		for _, arm := range st.Arms {
			if arm.Event == event {
				return n
			}
		}
	}
	return ""
}

// resolveStallLocked acts on a stalled-signal gate: discard drops the signal;
// advance moves the run to the state that arms the event and delivers it
// there — the thing you were doing by hand.
func (e *Engine) resolveStallLocked(g *core.Gate, event, response string) {
	run, err := e.Store.GetRun(g.RunID)
	if err != nil {
		return
	}
	sigs, _ := e.Store.PendingSignals(run.ID)
	var target *store.Signal
	for _, s := range sigs {
		if s.Event == event {
			target = s
			break
		}
	}
	if target == nil {
		return // drained on its own while the gate was open
	}
	e.Store.ConsumeSignal(target.ID)
	if response != "advance" {
		e.event(run.ID, run.State, "signal-discarded", map[string]any{"event": event}, run.Workspace)
		return
	}
	_, def, err := e.def(run.Workspace, run.Workflow)
	if err != nil {
		return
	}
	state := stateArming(def, event)
	if state == "" {
		e.parkLocked(*run, fmt.Sprintf("no state of %s arms %q", def.Name, event))
		return
	}
	e.Store.DisarmRunTimers(run.ID)
	e.event(run.ID, run.State, "signal-advanced", map[string]any{"event": event, "via": state}, run.Workspace)
	run.State = state
	d := core.Advance(def, *run, event, target.Payload)
	e.applyLocked(def, d)
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
	attempt := func() ([]EmittedEvent, bool, string) {
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
			return nil, false, detail
		}
		e.event("", "watcher:"+name, "watcher-swept", map[string]any{
			"observations": len(parsed.Events), "command": command, "duration_ms": durMS, "cost_usd": 0}, wsName)
		return parsed.Events, true, ""
	}

	events, ok, lastErr := attempt()
	if !ok {
		events, ok, lastErr = attempt()
	}
	e.settleSweep(wsName, name, events, ok, 0, lastErr)
}

// runWatcherSweep executes one sweep with failure semantics: no schema-valid
// result.json is a FAILURE (a derailed agent), never an empty observation.
// One immediate retry; WatcherFailLimit consecutive failures raise a gate.
func (e *Engine) runWatcherSweep(wsName, name string, req runner.Request) {
	var cost float64
	attempt := func(r runner.Request) ([]EmittedEvent, bool, string) {
		res, err := e.Runner.Run(context.Background(), r)
		if err != nil {
			e.mu.Lock()
			e.event("", "watcher:"+name, "watcher-failed", map[string]any{"error": err.Error()}, wsName)
			e.mu.Unlock()
			return nil, false, err.Error()
		}
		cost += res.CostUSD
		events, ok := readWatcherEvents(r.HandoffDir)
		if !ok {
			const detail = "no schema-valid result.json (agent derailed?)"
			e.mu.Lock()
			e.event("", "watcher:"+name, "watcher-failed", map[string]any{
				"error": detail, "session": res.SessionID, "cost_usd": res.CostUSD}, wsName)
			e.mu.Unlock()
			return nil, false, detail
		}
		e.mu.Lock()
		e.event("", "watcher:"+name, "watcher-swept", map[string]any{
			"observations": len(events), "session": res.SessionID, "cost_usd": res.CostUSD}, wsName)
		e.mu.Unlock()
		return events, true, ""
	}

	events, ok, lastErr := attempt(req)
	if !ok {
		retry := req
		retry.BaseDir = req.BaseDir + "-retry"
		retry.HandoffDir = filepath.Join(retry.BaseDir, "handoff")
		events, ok, lastErr = attempt(retry)
	}
	e.settleSweep(wsName, name, events, ok, cost, lastErr)
}

// watcherState is one watcher's failure streak: how many, since when, and
// what the last failure actually said.
type watcherState struct {
	fails   int
	since   time.Time
	lastErr string
}

// settleSweep applies the shared post-sweep semantics: success routes the
// observations and clears the streak; failure counts a strike, backs the
// cadence off, and keeps ONE gate up to date.
func (e *Engine) settleSweep(wsName, name string, events []EmittedEvent, ok bool, cost float64, lastErr string) {
	e.mu.Lock()
	key := wsName + "/" + name
	if ok {
		delete(e.watcherHealth, key)
		e.closeWatcherGatesLocked(wsName, watcherStatePrefix+name)
		e.mu.Unlock()
		e.HandleWatcherEvents(wsName, name, events)
		return
	}
	st := e.watcherHealth[key]
	if st == nil {
		st = &watcherState{since: e.Clock.Now()}
		e.watcherHealth[key] = st
	}
	st.fails++
	st.lastErr = lastErr
	// A broken watcher must not keep sweeping at its healthy cadence: 60s of
	// re-failing an expired token is what produced a gate every ~3 minutes,
	// all night.
	e.rescheduleWatcherLocked(wsName, name, watcherBackoff(st.fails))
	if st.fails >= WatcherFailLimit {
		e.raiseWatcherGateLocked(wsName, name, st, cost)
	}
	e.mu.Unlock()
	e.drain()
}

// watcherBackoff is the failing cadence: 60s → 2m → 5m → 15m → 30m cap.
// The healthy cadence returns on the first success (fireWatcher reschedules
// on the spec's `every` before each sweep).
func watcherBackoff(fails int) time.Duration {
	steps := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute}
	if fails >= len(steps) {
		return steps[len(steps)-1]
	}
	if fails < 1 {
		return steps[0]
	}
	return steps[fails-1]
}

// raiseWatcherGateLocked keeps exactly one open gate per (workspace, watcher)
// and updates its payload in place. The old code minted a fresh gate id every
// WatcherFailLimit failures and reset the counter — 78% of all gate volume.
func (e *Engine) raiseWatcherGateLocked(wsName, name string, st *watcherState, cost float64) {
	state := watcherStatePrefix + name
	payload := watcherGatePayload(name, st, cost)
	if g := e.openWatcherGateLocked(wsName, state); g != nil {
		if err := e.Store.SetGatePayload(g.ID, payload); err != nil {
			e.Log.Printf("update watcher gate %s: %v", g.ID, err)
		}
		e.event("", state, "watcher-gate-updated", map[string]any{"gate": g.ID, "fails": st.fails}, wsName)
		return
	}
	e.openGateLocked(core.Run{ID: "", Workspace: wsName, State: state}, core.ActionOpenGate{
		State: state, Kind: core.GateChoice, Payload: payload, Options: WatcherGateOptions,
	})
}

// openWatcherGateLocked returns the watcher's standing gate (the oldest) and
// closes any duplicates — including the pile the old mint-a-new-one behaviour
// left behind.
func (e *Engine) openWatcherGateLocked(wsName, state string) *core.Gate {
	var keep *core.Gate
	for _, g := range e.watcherGatesLocked(wsName, state) {
		if keep == nil {
			keep = g
			continue
		}
		e.Store.CancelGate(g.ID, e.Clock.Now())
	}
	return keep
}

// closeWatcherGatesLocked retires a watcher's gates once it sweeps clean —
// the question they ask has answered itself.
func (e *Engine) closeWatcherGatesLocked(wsName, state string) {
	for _, g := range e.watcherGatesLocked(wsName, state) {
		e.Store.CancelGate(g.ID, e.Clock.Now())
		e.event("", state, "watcher-gate-closed", map[string]any{"gate": g.ID, "reason": "watcher recovered"}, wsName)
	}
}

// watcherGatesLocked lists a watcher's open gates, oldest first.
func (e *Engine) watcherGatesLocked(wsName, state string) []*core.Gate {
	gates, err := e.Store.OpenGates() // ordered by created_at ASC
	if err != nil {
		return nil
	}
	var out []*core.Gate
	for _, g := range gates {
		if g.RunID == "" && g.Workspace == wsName && g.State == state {
			out = append(out, g)
		}
	}
	return out
}

func watcherGatePayload(name string, st *watcherState, cost float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "watcher %s has failed %d consecutive sweeps since %s — nothing it drives has advanced since then.",
		name, st.fails, st.since.Format("Mon 15:04"))
	if st.lastErr != "" {
		fmt.Fprintf(&b, " last error: %s", st.lastErr)
	}
	// command watchers are exec'd directly: cost is structurally 0, and a
	// "$0.0000" line on every gate reads as a broken cost meter.
	if cost > 0 {
		fmt.Fprintf(&b, " sweep cost $%.4f.", cost)
	}
	b.WriteString(" retry = sweep again now; pause-24h = silence it for a day; disable = stop it until you retry this gate.")
	return b.String()
}

// applyWatcherChoiceLocked makes the options act — answering a watcher gate
// used to be a literal no-op (answerGateLocked returned early on run-less
// gates).
func (e *Engine) applyWatcherChoiceLocked(g *core.Gate, response string) {
	name := strings.TrimPrefix(g.State, watcherStatePrefix)
	delete(e.watcherHealth, g.Workspace+"/"+name)
	var in time.Duration
	switch response {
	case "pause-24h":
		in = 24 * time.Hour
	case "disable":
		in = watcherDisabledFor
	default: // retry
		in = 0
	}
	e.rescheduleWatcherLocked(g.Workspace, name, in)
	e.event("", g.State, "watcher-"+response, map[string]any{"next_sweep_in": in.String()}, g.Workspace)
}

// rescheduleWatcherLocked moves a watcher's next sweep. Pausing is a far-out
// fire_at rather than a disarm: the timer row stays active, so armWatchers
// won't resurrect it on the next daemon restart.
func (e *Engine) rescheduleWatcherLocked(wsName, name string, in time.Duration) {
	timers, err := e.Store.ActiveTimers()
	if err != nil {
		return
	}
	for _, t := range timers {
		if t.Kind != "watcher" {
			continue
		}
		if n, _ := t.Payload["watcher"].(string); n != name {
			continue
		}
		if w, _ := t.Payload["workspace"].(string); w != wsName {
			continue
		}
		e.Store.Reschedule(t.ID, e.Clock.Now().Add(in))
		return
	}
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

// watcherStatePrefix marks engine-level gates that belong to a watcher.
const watcherStatePrefix = "watcher:"

// watcherDisabledFor parks a disabled watcher's timer past any plausible
// uptime — "off" without a schema column. Answering the gate `retry` re-arms.
const watcherDisabledFor = 100 * 365 * 24 * time.Hour

// WatcherGateOptions are the choices on a broken-watcher gate. They act.
var WatcherGateOptions = []string{"retry", "pause-24h", "disable"}

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
			run, err := e.startRunLocked(wsName, w.Workflow, ev.Payload, watcherStatePrefix+watcherName)
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
