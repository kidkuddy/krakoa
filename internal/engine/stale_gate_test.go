package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
)

// A gate outlived its run six times in one week. One opened two minutes before
// its run finished, then nagged for sixteen hours about a decision that had
// already made itself. A terminal run must take its questions with it.
func TestTerminalRunRetiresItsGates(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "ungrounded", "questions", []any{"which button?"}))

	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	g := v.openGate(t, run.ID) // parked on the refine question
	if r := v.mustRun(t, run.ID); r.Status != core.StatusGated {
		t.Fatalf("precondition: run is %s, want gated", r.Status)
	}

	// The run ends by another route entirely — cancelled, or a terminal reached
	// while the gate was open. Either way the question is moot.
	if err := v.eng.CancelRun(run.ID, "world moved"); err != nil {
		t.Fatal(err)
	}
	if got, _ := v.st.GetGate(g.ID); got.Status == core.GateOpen {
		t.Fatalf("gate %s still open after its run ended", g.ID)
	}
}

// Every stall gate raised in the week of Jul 27 was answered "discard", and all
// of them were ticket-status echoes. An event that reports a fact and asks for
// nothing must not become a human question.
func TestAdvisorySignalIsDroppedNotGated(t *testing.T) {
	v, run := stalledRun(t, "k1")
	v.eng.Workspaces["cmddemo"].Advisory = []string{"echo"}

	v.clock.Advance(StallGuardAfter + time.Minute)
	v.eng.sweepStalledSignals()

	if g, _ := v.st.OpenGateForRun(run.ID); g != nil {
		t.Fatalf("advisory signal raised a gate: %q", g.Payload)
	}
	if sigs, _ := v.st.PendingSignals(run.ID); len(sigs) != 0 {
		t.Fatalf("advisory signal was neither dropped nor gated (%d left)", len(sigs))
	}
}

// A non-advisory signal in the same position still asks, because dropping an
// mr-ready silently is the bug the stall guard was built for.
func TestNonAdvisorySignalStillGates(t *testing.T) {
	v, run := stalledRun(t, "k2") // no Advisory declared

	v.clock.Advance(StallGuardAfter + time.Minute)
	v.eng.sweepStalledSignals()

	if g, _ := v.st.OpenGateForRun(run.ID); g == nil {
		t.Fatal("a signal that is not advisory must still raise the stall gate")
	}
}

// stalledRun parks a run in `late` holding one buffered `echo` — armed only in
// `early`, so no state reachable from here can ever consume it.
func stalledRun(t *testing.T, key string) (*env, *core.Run) {
	t.Helper()
	v := setupCmd(t)
	run, err := v.eng.StartRun("cmddemo", "stallflow", "", map[string]any{"key": key}, "")
	if err != nil {
		t.Fatal(err)
	}
	v.eng.HandleWatcherEvents("cmddemo", "cmd-watch", []EmittedEvent{{Event: "go", Key: key}})
	if r := v.mustRun(t, run.ID); r.State != "late" {
		t.Fatalf("precondition: run is at %s, want late", r.State)
	}
	v.eng.HandleWatcherEvents("cmddemo", "cmd-watch", []EmittedEvent{{Event: "echo", Key: key}})
	if sigs, _ := v.st.PendingSignals(run.ID); len(sigs) != 1 {
		t.Fatalf("precondition: want the echo buffered, got %d signals", len(sigs))
	}
	return v, run
}

// A gate is a claim about the world, and the world moves. One gate nagged for
// twenty hours about a pipeline that had gone green three minutes later, and
// the operator had no way to tell it so.
func TestGateRecheckClosesItselfWhenThePremiseExpires(t *testing.T) {
	v := setupCmd(t)
	marker := filepath.Join(t.TempDir(), "green")

	run, err := v.eng.StartRun("cmddemo", "gateflow", "", map[string]any{"marker": marker}, "")
	if err != nil {
		t.Fatal(err)
	}
	g := v.openGate(t, run.ID)

	// The world still agrees with the gate: recheck says red, nothing moves.
	v.clock.Advance(deliverySweepEvery + time.Minute)
	v.eng.sweepDeliveries()
	if got, _ := v.st.GetGate(g.ID); got.Status != core.GateOpen {
		t.Fatal("recheck closed a gate whose premise still holds")
	}

	// The pipeline recovers.
	if err := writeFile(marker); err != nil {
		t.Fatal(err)
	}
	v.clock.Advance(deliverySweepEvery + time.Minute)
	v.eng.sweepDeliveries()

	if got, _ := v.st.GetGate(g.ID); got.Status == core.GateOpen {
		t.Fatal("gate stayed open after its premise expired")
	}
	if r := v.mustRun(t, run.ID); r.State != "recovered" {
		t.Fatalf("run at %s; a recovering recheck must advance it to recovered", r.State)
	}
}

// The manual override for everything the recheck cannot decide: a gated run
// resumes, re-asking its question against the world as it is now. Refusing
// left the operator choosing between a false answer and killing the run.
func TestResumeWorksOnAGatedRun(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "ungrounded", "questions", []any{"which button?"}))

	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	g := v.openGate(t, run.ID)

	fr.on("refining", ok("outcome", "ok", "ticket", "d2"))
	fr.on("grounding", ok("outcome", "grounded"))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-9"))
	fr.on("dispatching", ok("outcome", "ok"))
	if err := v.eng.ResumeRun(run.ID); err != nil {
		t.Fatalf("resume on a gated run: %v", err)
	}
	if got, _ := v.st.GetGate(g.ID); got.Status == core.GateOpen {
		t.Fatal("resume left the stale gate open")
	}
	// Re-entering the state re-asks the question against the world as it is
	// now, which is the point — the old gate is retired, a fresh one stands in
	// its place, and the recheck sweep gets a shot at that one too.
	again, _ := v.st.OpenGateForRun(run.ID)
	if again == nil {
		t.Fatal("resume neither advanced the run nor re-asked its question")
	}
	if again.ID == g.ID {
		t.Fatal("resume reused the stale gate instead of re-asking")
	}
}

func writeFile(path string) error { return os.WriteFile(path, []byte("x"), 0o644) }

// A run told mid-flight that it is working the wrong repo could not act on it:
// `repo` is an input, gate answers cannot reach one, and the value stayed at
// the workflow default. That filed a second ticket building a UI another live
// run was already building.
func TestStepCanRebindADeclaredInput(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	// The grounder reads the ticket and finds it is not about the repo the run
	// was launched against.
	fr.on("grounding", ok("outcome", "grounded", "rebind", map[string]any{"repo": "acme/other-app"}))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-9"))
	fr.on("dispatching", ok("outcome", "ok"))

	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := v.mustRun(t, run.ID).Inputs["repo"]; got != "acme/other-app" {
		t.Fatalf("repo = %v; the grounder's correction was ignored", got)
	}
}

// A step may correct the run, not invent it: only declared inputs rebind.
func TestRebindRejectsUndeclaredInputs(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "grounded", "rebind", map[string]any{"smuggled": "value"}))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-9"))
	fr.on("dispatching", ok("outcome", "ok"))

	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := v.mustRun(t, run.ID).Inputs["smuggled"]; present {
		t.Fatal("an undeclared input was written into the run")
	}
}
