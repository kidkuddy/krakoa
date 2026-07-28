package engine

// A verb re-enters a workflow the human already ran once. The seam these
// tests hold: everything downstream of the entry state — the thread key, the
// correlation on the wait arms, `$filing.ticket_id` in every later `in:` —
// resolves from the seed, because the earlier run really did file the ticket.

import (
	"testing"

	"github.com/kidkuddy/krakoa/internal/core"
)

func TestFollowupEntryResumesTheLifecycleMidFlow(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("following-up", ok("outcome", "ok"))
	fr.on("merging", ok("outcome", "merged", "mr_url", "https://x/mr/12"))
	fr.on("notifying", ok("outcome", "ok"))
	fr.on("closing", ok("outcome", "ok"))

	run, err := v.eng.StartRun("demo", "task-lifecycle", "followup", map[string]any{
		"ticket_id": "CAL-9",
		"feedback":  "also gate it behind the flag",
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	// The thread resolves at creation, not after a filing step that never
	// runs: the followup lands in the SAME conversation as the original.
	if got := v.mustRun(t, run.ID).Thread; got != "CAL-9" {
		t.Fatalf("thread = %q, want CAL-9 (seeded $filing.ticket_id)", got)
	}
	// No refining/grounding/filing agent was spawned — the ticket exists.
	for _, call := range fr.calls {
		if call.State == "refining" || call.State == "filing" {
			t.Fatalf("followup re-ran %s; it should re-enter below it", call.State)
		}
	}
	// The verb commented and handed off to the same build wait as mid-lifecycle.
	r := v.mustRun(t, run.ID)
	if r.State != "building" || r.Status != core.StatusWaiting {
		t.Fatalf("got %s/%s, want building/waiting", r.State, r.Status)
	}

	// The point of the whole feature: the sweeper reviews the new MR and
	// emits mr-ready correlated on the ticket. Without a run waiting on it
	// that event has no consumer and becomes a stray alert gate.
	v.eng.HandleEmit("demo", EmittedEvent{Event: "ticket-in-review", Key: "CAL-9"})
	v.eng.HandleEmit("demo", EmittedEvent{Event: "mr-ready", Key: "CAL-9"})
	r = v.mustRun(t, run.ID)
	if r.State != "done" || r.Status != core.StatusDone {
		t.Fatalf("final: %s/%s, want done", r.State, r.Status)
	}
}

func TestEntryInputsOverrideTheSharedOnes(t *testing.T) {
	v := setup(t)
	v.run.on("following-up", ok("outcome", "ok"))

	// `idea` is required on the default entry and redeclared optional on
	// followup; the shared `repo` default still applies.
	run, err := v.eng.StartRun("demo", "task-lifecycle", "followup", map[string]any{
		"ticket_id": "CAL-9", "feedback": "f",
	}, "")
	if err != nil {
		t.Fatalf("followup should not demand `idea`: %v", err)
	}
	if got := v.mustRun(t, run.ID).Inputs["repo"]; got != "acme/demo-app" {
		t.Errorf("shared repo default = %v, want acme/demo-app", got)
	}

	// The default entry still demands it.
	if _, err := v.eng.StartRun("demo", "task-lifecycle", "", map[string]any{}, ""); err == nil {
		t.Error("default entry accepted a run with no idea")
	}
}

func TestUnknownEntryIsRefusedNotDefaulted(t *testing.T) {
	v := setup(t)
	// A typo must never silently start the workflow from the top — that is a
	// full refine+file cycle and a duplicate ticket.
	if _, err := v.eng.StartRun("demo", "task-lifecycle", "folowup", map[string]any{"idea": "x"}, ""); err == nil {
		t.Fatal("unknown entry started a run")
	}
}
