package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
	"github.com/kidkuddy/krakoa/internal/workspace"
)

// Several failures in three days of live use were the same shape: the world
// wasn't ready and krakoa found out the expensive way — an expired Cloudflare
// token, expired AWS SSO, revoked Codex OAuth, a dead niffty daemon. None can
// self-heal and all are cheap to test first. A workflow (or one state) names
// the checks it `requires`; a failing one puts the run in `blocked`, which
// releases its slot and auto-resumes the moment the check passes.

// CheckProbeEvery is how often every declared check is re-probed.
const CheckProbeEvery = 5 * time.Minute

// CheckNagEvery is how often a still-failing check re-pings, in work hours.
const CheckNagEvery = 2 * time.Hour

type checkResult struct {
	ok          bool
	detail      string
	at          time.Time
	failedSince time.Time
	lastNag     time.Time
}

// sweepChecks re-probes every declared check on a cadence, then blocks or
// resumes runs on the verdict. Probing is I/O, so it never runs under the
// engine lock.
func (e *Engine) sweepChecks() {
	e.mu.Lock()
	now := e.Clock.Now()
	if now.Sub(e.lastCheckSweep) < CheckProbeEvery {
		e.mu.Unlock()
		return
	}
	e.lastCheckSweep = now
	type job struct {
		ws, name string
		dc       workspace.DoctorCheck
	}
	var jobs []job
	for _, ws := range e.Workspaces {
		for name, dc := range ws.Checks {
			jobs = append(jobs, job{ws.Name, name, dc})
		}
	}
	e.mu.Unlock()

	e.Spawn(func() {
		defer e.drain()
		for _, j := range jobs {
			ok, detail := workspace.RunCheck(j.dc)
			e.recordCheck(j.ws, j.name, ok, detail)
		}
	})
}

// recordCheck stores a verdict and acts on the transition: a check that went
// bad nags once per period with the run count; a check that came back resumes
// everything waiting on it with a single message.
func (e *Engine) recordCheck(wsName, name string, ok bool, detail string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := wsName + "/" + name
	prev := e.checks[key]
	now := e.Clock.Now()
	cur := &checkResult{ok: ok, detail: detail, at: now}
	if prev != nil {
		cur.failedSince, cur.lastNag = prev.failedSince, prev.lastNag
	}
	e.checks[key] = cur

	if ok {
		if prev != nil && !prev.ok {
			n := e.resumeBlockedLocked(wsName, name)
			// Only close a loop that was opened: if nothing was ever blocked
			// and nothing was ever announced, "X is back — 0 runs resumed" is
			// a notification about nothing.
			if n > 0 || !prev.lastNag.IsZero() {
				e.notifyLocked(&core.Notice{
					ID: fmt.Sprintf("check-ok-%s-%s-%d", wsName, name, now.Unix()), Workspace: wsName,
					Kind: core.NoticeUnblocked,
					Text: fmt.Sprintf("%s is back — %d run(s) resumed.", name, n),
				})
			}
		}
		cur.failedSince, cur.lastNag = time.Time{}, time.Time{}
		return
	}

	if cur.failedSince.IsZero() {
		cur.failedSince = now
	}
	blocked := len(e.runsBlockedOnLocked(wsName, name))
	if blocked == 0 {
		return // nothing is waiting on it; doctor and the board still show it
	}
	// One message per CHECK, never per run — the exact inverse of the watcher
	// spam. Nagging stays inside work hours; nobody re-auths at 03:00.
	if !cur.lastNag.IsZero() && (now.Sub(cur.lastNag) < CheckNagEvery || !inWorkHours(now)) {
		return
	}
	cur.lastNag = now
	fix := ""
	if ws := e.Workspaces[wsName]; ws != nil {
		fix = ws.Checks[name].Fix
	}
	text := fmt.Sprintf("%s is failing — %d run(s) paused (%s).", name, blocked, detail)
	if fix != "" {
		text += " Fix: " + fix
	}
	e.notifyLocked(&core.Notice{
		ID: fmt.Sprintf("check-fail-%s-%s-%d", wsName, name, now.Unix()), Workspace: wsName,
		Kind: core.NoticeBlocked, Text: text,
	})
}

// checkFailingLocked names the first required check known to be failing.
// A check never probed is not a reason to block: ignorance is not failure.
func (e *Engine) checkFailingLocked(wsName string, names []string) (string, string) {
	for _, n := range names {
		if r := e.checks[wsName+"/"+n]; r != nil && !r.ok {
			return n, r.detail
		}
	}
	return "", ""
}

// blockLocked parks a run on a failing check. Distinct from needs-attention
// (a decision) and from waiting (the world working): nothing here is yours to
// answer, and the slot goes back to the pool.
func (e *Engine) blockLocked(def *core.WorkflowDefinition, run core.Run, check, detail string) {
	run.Status = core.StatusBlocked
	if run.Context == nil {
		run.Context = map[string]any{}
	}
	run.Context["blocked"] = map[string]any{
		"check": check, "state": run.State, "detail": detail,
		"since": e.Clock.Now().Format(time.RFC3339),
	}
	run.UpdatedAt = e.Clock.Now()
	if err := e.Store.SaveRun(&run); err != nil {
		e.Log.Printf("block %s: %v", run.ID, err)
		return
	}
	e.Store.DisarmRunTimers(run.ID)
	e.event(run.ID, run.State, "run-blocked", map[string]any{"check": check, "detail": detail}, run.Workspace)
	e.admitNextLocked(def, run.Workspace, run.Workflow) // the slot is free
	e.projectBoardLocked(&run)
}

// blockedOn reports which check a blocked run is waiting for.
func blockedOn(run *core.Run) string {
	b, _ := run.Context["blocked"].(map[string]any)
	s, _ := b["check"].(string)
	return s
}

func (e *Engine) runsBlockedOnLocked(wsName, check string) []*core.Run {
	runs, err := e.Store.ListRuns(core.StatusBlocked)
	if err != nil {
		return nil
	}
	var out []*core.Run
	for _, r := range runs {
		if r.Workspace == wsName && blockedOn(r) == check {
			out = append(out, r)
		}
	}
	return out
}

// resumeBlockedLocked re-enters every run blocked on this check, at the state
// it stopped in. Over-capacity runs queue rather than overshoot concurrency.
func (e *Engine) resumeBlockedLocked(wsName, check string) int {
	n := 0
	for _, run := range e.runsBlockedOnLocked(wsName, check) {
		_, def, err := e.def(run.Workspace, run.Workflow)
		if err != nil {
			continue
		}
		delete(run.Context, "blocked")
		active, _ := e.Store.CountActive(run.Workspace, run.Workflow)
		if def.Concurrency > 0 && active >= def.Concurrency {
			run.Status = core.StatusQueued
			run.UpdatedAt = e.Clock.Now()
			e.Store.SaveRun(run)
			e.event(run.ID, run.State, "run-requeued", map[string]any{"check": check}, run.Workspace)
			n++
			continue
		}
		e.event(run.ID, run.State, "run-resumed", map[string]any{"check": check}, run.Workspace)
		e.applyLocked(def, core.Enter(def, *run))
		n++
	}
	return n
}

// ResumeRun is the manual override on the same machinery: it un-blocks a run
// (or re-enters a parked one) from wherever it stopped.
func (e *Engine) ResumeRun(runID string) error {
	e.mu.Lock()
	defer func() { e.mu.Unlock(); e.drain() }()
	run, err := e.Store.GetRun(runID)
	if err != nil {
		return err
	}
	switch run.Status {
	case core.StatusBlocked, core.StatusNeedsAttention:
	default:
		return fmt.Errorf("run %s is %s — only blocked or needs-attention runs resume", runID, run.Status)
	}
	if g, _ := e.Store.OpenGateForRun(runID); g != nil {
		e.Store.CancelGate(g.ID, e.Clock.Now())
	}
	delete(run.Context, "blocked")
	_, def, err := e.def(run.Workspace, run.Workflow)
	if err != nil {
		return err
	}
	e.event(run.ID, run.State, "run-resumed", map[string]any{"by": "operator"}, run.Workspace)
	e.applyLocked(def, core.Enter(def, *run))
	return nil
}

// CheckBoard is one row of `krakoactl checks`.
type CheckBoard struct {
	Workspace   string    `json:"workspace"`
	Name        string    `json:"name"`
	OK          bool      `json:"ok"`
	Probed      bool      `json:"probed"`
	Detail      string    `json:"detail"`
	Fix         string    `json:"fix,omitempty"`
	At          time.Time `json:"at,omitempty"`
	FailedSince time.Time `json:"failed_since,omitempty"`
	Blocked     int       `json:"blocked_runs"`
}

func (e *Engine) ChecksBoard() []CheckBoard {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []CheckBoard
	for _, wsName := range sortedWorkspaces(e.Workspaces) {
		ws := e.Workspaces[wsName]
		names := make([]string, 0, len(ws.Checks))
		for n := range ws.Checks {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			row := CheckBoard{Workspace: wsName, Name: n, Fix: ws.Checks[n].Fix, OK: true,
				Blocked: len(e.runsBlockedOnLocked(wsName, n))}
			if r := e.checks[wsName+"/"+n]; r != nil {
				row.OK, row.Detail, row.At, row.FailedSince, row.Probed = r.ok, r.detail, r.at, r.failedSince, true
			}
			out = append(out, row)
		}
	}
	return out
}

func sortedWorkspaces(m map[string]*workspace.Workspace) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// notifyLocked queues a one-way message; delivery happens off the lock.
func (e *Engine) notifyLocked(n *core.Notice) {
	if n.At.IsZero() {
		n.At = e.Clock.Now()
	}
	e.event(n.RunID, n.State, "notice", map[string]any{"kind": string(n.Kind), "id": n.ID, "text": n.Text}, n.Workspace)
	channels := e.Channels
	e.pending = append(e.pending, func() {
		for _, ch := range channels {
			if err := ch.Notify(n); err != nil {
				e.Log.Printf("notify %s via %s: %v", n.ID, ch.Name(), err)
				e.mu.Lock()
				e.event(n.RunID, n.State, "notice-failed", map[string]any{"id": n.ID, "channel": ch.Name(), "error": err.Error()}, n.Workspace)
				e.mu.Unlock()
			}
		}
	})
}

// inWorkHours keeps nagging to hours when the fix is actually possible.
func inWorkHours(t time.Time) bool {
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	return t.Hour() >= 9 && t.Hour() < 19
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
