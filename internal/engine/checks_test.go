package engine

import (
	"testing"

	"github.com/kidkuddy/krakoa/internal/core"
)

// A failing check must park the run in `blocked` (not needs-attention, not a
// gate), free its slot, and resume it at the same state when the check comes
// back — with no human action beyond fixing the real thing.
func TestRequiresBlocksAndAutoResumes(t *testing.T) {
	v := setup(t)
	def := v.eng.Workspaces["demo"].Workflows["task-lifecycle"]
	st := def.States["filing"]
	st.Requires = []string{"cluster"}
	def.States["filing"] = st

	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "draft-1"))
	fr.on("grounding", ok("outcome", "grounded"))
	scriptHappyTail(fr)

	v.eng.recordCheck("demo", "cluster", false, "token expired")

	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	r := v.mustRun(t, run.ID)
	if r.Status != core.StatusBlocked || r.State != "filing" {
		t.Fatalf("want blocked at filing, got %s/%s", r.State, r.Status)
	}
	if blockedOn(r) != "cluster" {
		t.Errorf("blocked on %q", blockedOn(r))
	}
	if g, _ := v.st.OpenGateForRun(run.ID); g != nil {
		t.Errorf("blocked must not open a gate, got %s", g.ID)
	}
	// blocked releases the slot
	if n, _ := v.st.CountActive("demo", "task-lifecycle"); n != 0 {
		t.Errorf("blocked run still holds a slot (active=%d)", n)
	}

	v.eng.recordCheck("demo", "cluster", true, "ok")
	v.eng.drain()
	if r := v.mustRun(t, run.ID); r.State == "filing" && r.Status == core.StatusBlocked {
		t.Fatalf("run did not resume: %s/%s", r.State, r.Status)
	}
}
