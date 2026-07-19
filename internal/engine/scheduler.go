package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
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
		Instruction: probe.Instruction,
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
			// unrecognized outcome = "not yet"; log and let the cadence retry
			e.event(run.ID, t.State, "probe-pending", map[string]any{"detail": verr, "session": res.SessionID}, run.Workspace)
			return
		}
		e.Store.DisarmRunTimers(run.ID)
		e.event(run.ID, t.State, "probe-outcome", map[string]any{"outcome": result["outcome"], "session": res.SessionID}, run.Workspace)
		d := core.Advance(def, *cur, result["outcome"].(string), result)
		e.applyLocked(def, d)
	})
}

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
	e.Store.Reschedule(t.ID, e.Clock.Now().Add(t.Every))
	spec := ws.Agents[w.Agent]
	skills := map[string]string{}
	for _, sk := range spec.Skills {
		skills[sk] = ws.Skills[sk]
	}
	base := filepath.Join(e.DataDir, "watchers", name, fmt.Sprintf("%d", e.Clock.Now().UnixNano()))
	req := runner.Request{
		RunID: "", State: "watcher:" + name, Spec: spec,
		Instruction: w.Instruction + watcherProtocol,
		Skills:      skills,
		BaseDir:     base, HandoffDir: filepath.Join(base, "handoff"),
	}
	e.mu.Unlock()

	e.Spawn(func() {
		res, err := e.Runner.Run(context.Background(), req)
		if err != nil {
			e.mu.Lock()
			e.event("", "watcher:"+name, "watcher-failed", map[string]any{"error": err.Error()}, wsName)
			e.mu.Unlock()
			return
		}
		events := readWatcherEvents(req.HandoffDir)
		e.mu.Lock()
		e.event("", "watcher:"+name, "watcher-swept", map[string]any{"observations": len(events), "session": res.SessionID}, wsName)
		e.mu.Unlock()
		e.HandleWatcherEvents(wsName, name, events)
	})
}

const watcherProtocol = `

Report observations by writing $KRAKOA_HANDOFF/result.json:
{"outcome":"ok","events":[{"event":"<name>","key":"<dedupe key>","payload":{...}}]}
No observations = empty events list. The key must change when the observed
object changes (e.g. include the head SHA).`

func readWatcherEvents(handoffDir string) []EmittedEvent {
	var parsed struct {
		Events []EmittedEvent `json:"events"`
	}
	raw, err := readFile(filepath.Join(handoffDir, "result.json"))
	if err != nil {
		return nil
	}
	json.Unmarshal(raw, &parsed)
	return parsed.Events
}

// HandleWatcherEvents dedupes and routes a watcher sweep's observations.
// Consumed events (advanced, buffered, spawned) are marked; dropped ones are
// not, so they retry next sweep (e.g. correlation target not filed yet).
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
		if ev.Key != "" {
			if seen, _ := e.Store.DedupeSeen(watcherName, ev.Key); seen {
				continue
			}
		}
		disposition := e.routeEventLocked(wsName, ev)
		if disposition == "no matching run" && w != nil && w.Mode == "spawn" && spawnable(w, ev.Event) {
			run, err := e.startRunLocked(wsName, w.Workflow, ev.Payload, "")
			if err != nil {
				e.Log.Printf("watcher %s spawn: %v", watcherName, err)
				continue
			}
			disposition = "spawned " + run.ID
		}
		e.event("", "watcher:"+watcherName, "watcher-observed", map[string]any{"event": ev.Event, "key": ev.Key, "disposition": disposition}, wsName)
		if ev.Key != "" && disposition != "no matching run" && disposition != "run finished; dropped" {
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
