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

	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
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

// A workflow edit lands on runs that are already in flight — the engine always
// interprets the CURRENT definition. A run started before `env` existed hit
// "resolve $env.trunk: env not found" and parked, three times live.
func TestEnvBackfilledOnRunsThatPredateIt(t *testing.T) {
	v := setup(t)
	ws := v.eng.Workspaces["demo"]
	ws.Envs = map[string]map[string]string{"dev": {"trunk": "development", "ns_suffix": "-dev"}}
	def := ws.Workflows["task-lifecycle"]
	def.Inputs["env"] = core.InputSpec{Type: "string", Default: "dev"}

	old := &core.Run{
		ID: "old-run", Workspace: "demo", Workflow: "task-lifecycle",
		State: def.Start, Status: core.StatusWaiting,
		Inputs: map[string]any{"idea": "x"}, Context: map[string]any{}, EdgeCounts: map[string]int{},
		CreatedAt: v.clock.Now(), UpdatedAt: v.clock.Now(),
	}
	if err := v.st.CreateRun(old); err != nil {
		t.Fatal(err)
	}

	v.eng.mu.Lock()
	if _, err := v.eng.runDef(old); err != nil {
		t.Fatal(err)
	}
	v.eng.mu.Unlock()

	env, _ := old.Context["env"].(map[string]any)
	if env["trunk"] != "development" {
		t.Fatalf("env not backfilled: %+v", old.Context)
	}
	// and it must be durable, not just in memory
	fresh := v.mustRun(t, "old-run")
	if e, _ := fresh.Context["env"].(map[string]any); e["trunk"] != "development" {
		t.Fatalf("backfill not persisted: %+v", fresh.Context)
	}
}

// `default: ""` is a default, not the absence of one: an optional input
// declared that way must still exist, or every $input.<name> that references
// it parks the run.
func TestEmptyStringDefaultIsApplied(t *testing.T) {
	v := setup(t)
	def := v.eng.Workspaces["demo"].Workflows["task-lifecycle"]
	def.Inputs["note"] = core.InputSpec{Type: "string", Default: ""}

	v.run.on("refining", ok("outcome", "ok", "ticket", "t"))
	run, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	got := v.mustRun(t, run.ID).Inputs
	if _, ok := got["note"]; !ok {
		t.Fatalf("optional input with an empty default is missing: %v", got)
	}
}
