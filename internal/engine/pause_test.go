package engine

import (
	"testing"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
	"github.com/kidkuddy/krakoa/internal/workspace"
)

func TestPauseHoldsNewRunsAndUnpauseReleasesThem(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "draft-1"))
	fr.on("grounding", ok("outcome", "grounded"))
	scriptHappyTail(fr)

	if _, err := v.eng.Pause("demo", "", "session limit"); err != nil {
		t.Fatal(err)
	}
	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "fix the button"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, run.ID); r.Status != core.StatusQueued {
		t.Fatalf("paused run admitted: %s/%s", r.State, r.Status)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("paused workspace spawned %d session(s)", len(fr.calls))
	}

	n, err := v.eng.Unpause("demo", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("resumed = %d, want 0 (the held run was queued, not blocked)", n)
	}
	r := v.mustRun(t, run.ID)
	if r.State != "building" || r.Status != core.StatusWaiting {
		t.Fatalf("after unpause: %s/%s, want building/waiting", r.State, r.Status)
	}
}

// The one that matters for the session limit: a run mid-lifecycle must stop
// before its NEXT step, not lose the step already in flight.
func TestPauseParksARunBeforeItsNextStep(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "draft-1"))
	fr.on("grounding", ok("outcome", "grounded"))
	scriptHappyTail(fr)

	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "fix the button"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, run.ID); r.State != "building" {
		t.Fatalf("setup: %s", r.State)
	}
	before := len(fr.calls)

	if _, err := v.eng.Pause("demo", "task-lifecycle", "session limit"); err != nil {
		t.Fatal(err)
	}
	// The builder answers while paused: the run may advance through waits and
	// gates, but the merging step is a session and must not start.
	v.eng.HandleEmit("demo", EmittedEvent{Event: "ticket-in-review", Key: "CAL-9"})
	v.eng.HandleEmit("demo", EmittedEvent{Event: "mr-ready", Key: "CAL-9"})

	r := v.mustRun(t, run.ID)
	if r.Status != core.StatusBlocked || r.State != "merging" {
		t.Fatalf("got %s/%s, want merging/blocked", r.State, r.Status)
	}
	if blockedOn(r) != PausedCheck {
		t.Errorf("blocked on %q, want %q", blockedOn(r), PausedCheck)
	}
	if len(fr.calls) != before {
		t.Fatalf("paused run spawned %d new session(s)", len(fr.calls)-before)
	}

	n, err := v.eng.Unpause("demo", "task-lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("resumed = %d, want 1", n)
	}
	if r := v.mustRun(t, run.ID); r.State != "done" || r.Status != core.StatusDone {
		t.Fatalf("after unpause: %s/%s, want done/done", r.State, r.Status)
	}
}

func TestPauseIsScopedToOneWorkflow(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("reviewing", ok("outcome", "clean"))
	fr.on("marking-ready", ok("outcome", "ok"))

	if _, err := v.eng.Pause("demo", "task-lifecycle", "holding the expensive one"); err != nil {
		t.Fatal(err)
	}
	held, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, held.ID); r.Status != core.StatusQueued {
		t.Fatalf("task-lifecycle not held: %s", r.Status)
	}
	free, err := v.eng.StartRun("demo", "review-sweeper", "", map[string]any{
		"repo": "acme/demo-app", "mr_iid": "7", "head_sha": "abc",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, free.ID); r.Status == core.StatusQueued {
		t.Fatalf("review-sweeper held by a task-lifecycle pause: %s/%s", r.State, r.Status)
	}
}

// A narrower pause must outlive the broader one it was nested under,
// otherwise "unpause the workspace" silently resumes a workflow someone
// stopped for its own reason.
func TestUnpausingAWorkspaceLeavesAWorkflowPauseStanding(t *testing.T) {
	v := setup(t)
	v.run.on("refining", ok("outcome", "ok", "ticket", "draft-1"))

	if _, err := v.eng.Pause("demo", "task-lifecycle", "its own reason"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.eng.Pause("demo", "", "everything"); err != nil {
		t.Fatal(err)
	}
	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.eng.Unpause("demo", ""); err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, run.ID); r.Status != core.StatusQueued {
		t.Fatalf("workflow pause did not survive: %s/%s", r.State, r.Status)
	}
	if len(v.run.calls) != 0 {
		t.Fatalf("spawned %d session(s) under a live workflow pause", len(v.run.calls))
	}
}

// The property the whole feature rests on: pause outlives the daemon. A
// restart that forgets it comes back up spawning what it was paused to stop.
func TestPauseSurvivesARestart(t *testing.T) {
	v := setup(t)
	if _, err := v.eng.Pause("demo", "", "session limit"); err != nil {
		t.Fatal(err)
	}
	ws, errs := workspace.Load("testdata/demo")
	if len(errs) != 0 {
		t.Fatalf("fixture: %v", errs)
	}
	restarted := New(v.st, v.run, v.clock, map[string]*workspace.Workspace{"demo": ws}, t.TempDir())
	restarted.Spawn = func(f func()) { f() }
	restarted.Log.SetOutput(v.out)

	if pauses := restarted.Pauses(); len(pauses) != 1 || pauses[0].Reason != "session limit" {
		t.Fatalf("pauses after restart = %+v", pauses)
	}
	run, err := restarted.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, run.ID); r.Status != core.StatusQueued {
		t.Fatalf("restarted daemon admitted a run under a live pause: %s", r.Status)
	}
}

func TestPausedScheduleSkipsItsOccurrence(t *testing.T) {
	v := setup(t)
	v.run.on("doing", ok("outcome", "ok"))
	v.eng.Recover() // arms the schedule timers

	if _, err := v.eng.Pause("demo", "daily", "session limit"); err != nil {
		t.Fatal(err)
	}
	v.clock.Advance(24 * time.Hour)
	v.eng.Tick()

	runs, err := v.st.ListRuns()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Workflow == "daily" {
			t.Fatalf("paused schedule banked an occurrence: %s (%s)", r.ID, r.Status)
		}
	}
}

// The working mode the per-run override exists for: hold everything, then
// let exactly one thing through.
func TestAllowOneRunThroughAStandingPause(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "draft-1"))
	fr.on("grounding", ok("outcome", "grounded"))
	scriptHappyTail(fr)

	if _, err := v.eng.Pause("demo", "", "session limit"); err != nil {
		t.Fatal(err)
	}
	wanted, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "the one I want"}, "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "not this one"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.eng.UnpauseRun(wanted.ID); err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, wanted.ID); r.State != "building" || r.Status != core.StatusWaiting {
		t.Fatalf("allowed run did not go: %s/%s", r.State, r.Status)
	}
	if r := v.mustRun(t, other.ID); r.Status != core.StatusQueued {
		t.Fatalf("the pause leaked: %s is %s", other.ID, r.Status)
	}
	// and it keeps going for its whole life, not just one step
	v.eng.HandleEmit("demo", EmittedEvent{Event: "ticket-in-review", Key: "CAL-9"})
	v.eng.HandleEmit("demo", EmittedEvent{Event: "mr-ready", Key: "CAL-9"})
	if r := v.mustRun(t, wanted.ID); r.State != "done" {
		t.Fatalf("allowed run re-blocked at %s/%s", r.State, r.Status)
	}
}

// An allowed run behind a held one must still be admitted: stopping at the
// head of the queue would strand it forever.
func TestAllowedRunIsAdmittedFromBehindHeldOnes(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "draft-1"))
	fr.on("grounding", ok("outcome", "grounded"))
	scriptHappyTail(fr)

	if _, err := v.eng.Pause("demo", "", "session limit"); err != nil {
		t.Fatal(err)
	}
	first, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "ahead in the queue"}, "")
	if err != nil {
		t.Fatal(err)
	}
	behind, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "the one I want"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.eng.UnpauseRun(behind.ID); err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, behind.ID); r.Status == core.StatusQueued {
		t.Fatalf("allowed run stranded behind a held one: %s/%s", r.State, r.Status)
	}
	if r := v.mustRun(t, first.ID); r.Status != core.StatusQueued {
		t.Fatalf("held run went anyway: %s/%s", r.State, r.Status)
	}
}

func TestPauseOneRunWhileNothingElseIsPaused(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "draft-1"))
	fr.on("grounding", ok("outcome", "grounded"))
	scriptHappyTail(fr)

	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.eng.PauseRun(run.ID, "stop this one"); err != nil {
		t.Fatal(err)
	}
	v.eng.HandleEmit("demo", EmittedEvent{Event: "ticket-in-review", Key: "CAL-9"})
	v.eng.HandleEmit("demo", EmittedEvent{Event: "mr-ready", Key: "CAL-9"})
	r := v.mustRun(t, run.ID)
	if r.Status != core.StatusBlocked || r.State != "merging" {
		t.Fatalf("got %s/%s, want merging/blocked", r.State, r.Status)
	}
	if err := v.eng.UnpauseRun(run.ID); err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, run.ID); r.State != "done" {
		t.Fatalf("after release: %s/%s", r.State, r.Status)
	}
}

// An allowance must not outlive the pause it was an exception to, or the
// next pause silently lets it through.
func TestLiftingThePauseClearsAllowances(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "draft-1"))
	fr.on("grounding", ok("outcome", "grounded"))
	scriptHappyTail(fr)

	if _, err := v.eng.Pause("demo", "", "first pause"); err != nil {
		t.Fatal(err)
	}
	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.eng.UnpauseRun(run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := v.eng.Unpause("demo", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := v.eng.Pause("demo", "", "second pause"); err != nil {
		t.Fatal(err)
	}
	r := v.mustRun(t, run.ID)
	if _, allowed := runFlag(r, runAllowedKey); allowed {
		t.Fatalf("stale allowance survived the pause it belonged to")
	}
}

func TestUnpauseWithNothingPausedIsAnError(t *testing.T) {
	v := setup(t)
	if _, err := v.eng.Unpause("demo", ""); err == nil {
		t.Fatal("unpausing an unpaused workspace should say so")
	}
}
