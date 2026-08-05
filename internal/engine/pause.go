package engine

import (
	"fmt"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
)

// Pausing is the brake that was missing: the only way to stop krakoa spending
// sessions was to cancel runs (terminal, throws the work away) or kill the
// daemon (which on restart re-enters every running run at once and re-runs the
// step that was in flight). A pause spawns nothing new and keeps everything.
//
// What it stops: admitting queued runs, starting an agent step, watcher
// sweeps, and scheduled runs. What it does NOT stop: an agent step already in
// flight. Killing one mid-session throws away spend already made and lands the
// run on a retry that spends it again, so a pause lets it finish and parks the
// run before the next step.
//
// A run parked by a pause is `blocked` — the same status a failing check
// produces, and for the same reason: it is waiting on the world, not on a
// decision. That releases its concurrency slot and makes unpausing the
// existing resume path rather than a second one.

// PausedCheck is the synthetic check name a pause parks runs on. Workspace
// check names are YAML keys, so the `$` guarantees it can never collide with
// a declared one.
const PausedCheck = "$paused"

// Run-level overrides, carried on the run so they survive a restart and show
// up in its timeline. The working mode they exist for is "hold everything,
// then let three things through": a scope pause plus an allowlist.
const (
	// runHeldKey holds this one run, whatever the workspace is doing.
	runHeldKey = "pause_held"
	// runAllowedKey lets this one run through a scope pause. Naming a run is
	// a decision about that run, and it outranks the blanket one.
	runAllowedKey = "pause_allowed"
)

func runFlag(run *core.Run, key string) (string, bool) {
	v, ok := run.Context[key]
	if !ok {
		return "", false
	}
	if m, ok := v.(map[string]any); ok {
		reason, _ := m["reason"].(string)
		return reason, true
	}
	return "", true
}

// heldLocked is the pause test every path routes through, in precedence
// order: a run held by name, then a run allowed by name, then the scope.
func (e *Engine) heldLocked(run *core.Run) (string, bool) {
	if reason, held := runFlag(run, runHeldKey); held {
		if reason == "" {
			reason = "run held"
		}
		return reason, true
	}
	if _, allowed := runFlag(run, runAllowedKey); allowed {
		return "", false
	}
	return e.pausedLocked(run.Workspace, run.Workflow)
}

// PauseRun holds one run by name. It takes effect before that run's next
// step: an agent already running is left to finish, exactly as a scope pause
// does, because killing it wastes the spend and then retries it.
func (e *Engine) PauseRun(runID, reason string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	run, err := e.Store.GetRun(runID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return fmt.Errorf("run %s is %s — there is nothing left to hold", runID, run.Status)
	}
	if run.Context == nil {
		run.Context = map[string]any{}
	}
	delete(run.Context, runAllowedKey)
	run.Context[runHeldKey] = map[string]any{"reason": reason, "since": e.Clock.Now().Format(time.RFC3339)}
	run.UpdatedAt = e.Clock.Now()
	if err := e.Store.SaveRun(run); err != nil {
		return err
	}
	e.event(run.ID, run.State, "run-paused", map[string]any{"reason": reason}, run.Workspace)
	return nil
}

// UnpauseRun releases one run: it clears any hold on it and, while a scope
// pause is standing, marks it allowed so it can walk to completion while
// everything else stays put. A run already parked on the pause re-enters
// where it stopped; a queued one is admitted if there is a slot.
func (e *Engine) UnpauseRun(runID string) error {
	e.mu.Lock()
	defer func() { e.mu.Unlock(); e.drain() }()
	run, err := e.Store.GetRun(runID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return fmt.Errorf("run %s is %s — nothing to release", runID, run.Status)
	}
	if run.Context == nil {
		run.Context = map[string]any{}
	}
	delete(run.Context, runHeldKey)
	// Only worth recording while something covers it; otherwise the run is
	// simply not paused, and a stale allowance would quietly exempt it from
	// the NEXT pause.
	if _, covered := e.pausedLocked(run.Workspace, run.Workflow); covered {
		run.Context[runAllowedKey] = map[string]any{"since": e.Clock.Now().Format(time.RFC3339)}
	}
	run.UpdatedAt = e.Clock.Now()
	if err := e.Store.SaveRun(run); err != nil {
		return err
	}
	e.event(run.ID, run.State, "run-unpaused", nil, run.Workspace)

	def, err := e.runDef(run)
	if err != nil {
		return err
	}
	switch {
	case run.Status == core.StatusBlocked && blockedOn(run) == PausedCheck:
		delete(run.Context, "blocked")
		e.applyLocked(def, core.Enter(def, *run))
	case run.Status == core.StatusQueued:
		e.admitNextLocked(def, run.Workspace, run.Workflow)
	}
	return nil
}

// reloadPauses re-reads the pause table into the in-memory mirror the hot
// paths consult.
func (e *Engine) reloadPauses() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reloadPausesLocked()
}

func (e *Engine) reloadPausesLocked() {
	list, err := e.Store.Pauses()
	if err != nil {
		e.Log.Printf("load pauses: %v", err)
		return
	}
	e.pauses = list
}

// pausedLocked reports whether work for this workflow is suspended, and why.
// Passing an empty workflow asks only about a workspace-wide pause, which is
// what the workspace-scoped machinery (watchers) needs to know.
func (e *Engine) pausedLocked(ws, wf string) (string, bool) {
	for _, p := range e.pauses {
		if p.Covers(ws, wf) {
			return p.Reason, true
		}
	}
	return "", false
}

// Pause suspends a workspace, or one workflow inside it. An empty workspace
// pauses every loaded one — the "stop everything, now" case.
func (e *Engine) Pause(wsName, wfName, reason string) ([]core.Pause, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	targets := []string{wsName}
	if wsName == "" {
		targets = nil
		for name := range e.Workspaces {
			targets = append(targets, name)
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("no workspaces loaded")
		}
	}
	var out []core.Pause
	for _, target := range targets {
		ws, ok := e.Workspaces[target]
		if !ok {
			return nil, fmt.Errorf("unknown workspace %q", target)
		}
		if wfName != "" && ws.Workflows[wfName] == nil {
			return nil, fmt.Errorf("workspace %s has no workflow %q", target, wfName)
		}
		p := core.Pause{Workspace: target, Workflow: wfName, Reason: reason, Since: e.Clock.Now()}
		if err := e.Store.SetPause(&p); err != nil {
			return nil, err
		}
		e.event("", "", "paused", map[string]any{"workflow": wfName, "reason": reason}, target)
		out = append(out, p)
	}
	e.reloadPausesLocked()
	return out, nil
}

// Unpause lifts a pause and lets the work it held go again: every run parked
// on it re-enters where it stopped, and the queue drains into the freed slots.
// It returns how many runs moved.
func (e *Engine) Unpause(wsName, wfName string) (int, error) {
	e.mu.Lock()
	defer func() { e.mu.Unlock(); e.drain() }()

	targets := []string{wsName}
	if wsName == "" {
		targets = nil
		for _, p := range e.pauses {
			targets = append(targets, p.Workspace)
		}
	}
	lifted := false
	for _, target := range targets {
		ok, err := e.Store.ClearPause(target, wfName)
		if err != nil {
			return 0, err
		}
		if ok {
			lifted = true
			e.event("", "", "unpaused", map[string]any{"workflow": wfName}, target)
		}
	}
	if !lifted {
		return 0, fmt.Errorf("nothing was paused there")
	}
	// The mirror must be current before anything resumes: resumeBlockedLocked
	// skips runs a narrower pause still covers, and it reads it.
	e.reloadPausesLocked()

	n := 0
	for _, target := range targets {
		e.clearAllowancesLocked(target)
		n += e.resumeBlockedLocked(target, PausedCheck)
		ws, ok := e.Workspaces[target]
		if !ok {
			continue
		}
		for _, def := range ws.Workflows {
			e.admitNextLocked(def, target, def.Name)
		}
	}
	return n, nil
}

// clearAllowancesLocked drops the per-run allowances a lifted pause made
// meaningless. Left behind, they are invisible exemptions from the next
// pause — the operator holds everything and three runs keep going.
func (e *Engine) clearAllowancesLocked(wsName string) {
	runs, err := e.Store.ListRuns()
	if err != nil {
		return
	}
	for _, run := range runs {
		if run.Workspace != wsName || run.Status.Terminal() {
			continue
		}
		if _, ok := runFlag(run, runAllowedKey); !ok {
			continue
		}
		if _, still := e.pausedLocked(run.Workspace, run.Workflow); still {
			continue // a narrower pause still covers it; the allowance still means something
		}
		delete(run.Context, runAllowedKey)
		run.UpdatedAt = e.Clock.Now()
		if err := e.Store.SaveRun(run); err != nil {
			e.Log.Printf("clear allowance %s: %v", run.ID, err)
		}
	}
}

// Pauses lists what is currently suspended.
func (e *Engine) Pauses() []core.Pause {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]core.Pause(nil), e.pauses...)
}

// RunPause is one run singled out by name, in either direction.
type RunPause struct {
	RunID     string
	Workspace string
	Workflow  string
	State     string
	Status    string
	Reason    string
}

// PauseBoard is the whole picture: the scopes that are suspended, the runs
// held individually, and the runs allowed through a standing pause. The last
// list is the one that answers "so what is actually still moving".
type PauseBoard struct {
	Scopes  []core.Pause
	Held    []RunPause
	Allowed []RunPause
}

func (e *Engine) PauseBoard() PauseBoard {
	e.mu.Lock()
	defer e.mu.Unlock()
	board := PauseBoard{Scopes: append([]core.Pause(nil), e.pauses...)}
	runs, err := e.Store.ListRuns()
	if err != nil {
		return board
	}
	for _, run := range runs {
		if run.Status.Terminal() {
			continue
		}
		rp := RunPause{
			RunID: run.ID, Workspace: run.Workspace, Workflow: run.Workflow,
			State: run.State, Status: string(run.Status),
		}
		if reason, held := runFlag(run, runHeldKey); held {
			rp.Reason = reason
			board.Held = append(board.Held, rp)
			continue
		}
		if _, allowed := runFlag(run, runAllowedKey); allowed {
			board.Allowed = append(board.Allowed, rp)
		}
	}
	return board
}
