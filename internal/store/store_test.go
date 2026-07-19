package store

import (
	"testing"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkRun(id string) *core.Run {
	now := time.Now()
	return &core.Run{
		ID: id, Workspace: "ws", Workflow: "wf", DefHash: "h1",
		State: "s1", Status: core.StatusRunning,
		Inputs:     map[string]any{"idea": "x"},
		Context:    map[string]any{},
		EdgeCounts: map[string]int{},
		CreatedAt:  now, UpdatedAt: now,
	}
}

func TestRunRoundtrip(t *testing.T) {
	s := open(t)
	r := mkRun("r1")
	if err := s.CreateRun(r); err != nil {
		t.Fatal(err)
	}
	r.State, r.Status = "s2", core.StatusWaiting
	r.Context["s1"] = map[string]any{"out": "v"}
	r.EdgeCounts["s1/ok"] = 2
	if err := s.SaveRun(r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun("r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "s2" || got.Status != core.StatusWaiting {
		t.Errorf("got %s/%s", got.State, got.Status)
	}
	if got.EdgeCounts["s1/ok"] != 2 {
		t.Errorf("edge counts lost: %+v", got.EdgeCounts)
	}
	if got.Inputs["idea"] != "x" {
		t.Errorf("inputs lost: %+v", got.Inputs)
	}
	if _, ok := got.Context["s1"].(map[string]any); !ok {
		t.Errorf("context lost: %+v", got.Context)
	}

	if err := s.SaveRun(mkRun("ghost")); err == nil {
		t.Error("saving unknown run should fail")
	}
}

func TestConcurrencyQueue(t *testing.T) {
	s := open(t)
	early := mkRun("q1")
	early.Status = core.StatusQueued
	early.CreatedAt = time.Now().Add(-time.Hour)
	late := mkRun("q2")
	late.Status = core.StatusQueued
	active := mkRun("a1")
	for _, r := range []*core.Run{late, early, active} {
		if err := s.CreateRun(r); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.CountActive("ws", "wf")
	if err != nil || n != 1 {
		t.Fatalf("active = %d, %v", n, err)
	}
	next, err := s.NextQueued("ws", "wf")
	if err != nil || next == nil || next.ID != "q1" {
		t.Fatalf("next = %+v, %v (FIFO broken?)", next, err)
	}
	none, err := s.NextQueued("ws", "other")
	if err != nil || none != nil {
		t.Fatalf("expected nil for other workflow, got %+v", none)
	}
}

func TestGateFirstResponseWins(t *testing.T) {
	s := open(t)
	g := &core.Gate{
		ID: "g1", Workspace: "ws", RunID: "r1", State: "asking",
		Kind: core.GateQuestion, Payload: "q?", Options: []string{"a", "b"},
		Status: core.GateOpen, CreatedAt: time.Now(),
	}
	if err := s.CreateGate(g); err != nil {
		t.Fatal(err)
	}
	won, err := s.ResolveGate("g1", "a", map[string]any{"q": "a"}, "slack", time.Now())
	if err != nil || !won {
		t.Fatalf("first response should win: %v %v", won, err)
	}
	won, err = s.ResolveGate("g1", "b", nil, "console", time.Now())
	if err != nil || won {
		t.Fatalf("second response should lose: %v %v", won, err)
	}
	got, _ := s.GetGate("g1")
	if got.Response != "a" || got.Responder != "slack" || got.Status != core.GateAnswered {
		t.Errorf("gate = %+v", got)
	}
	open, _ := s.OpenGates()
	if len(open) != 0 {
		t.Errorf("open gates = %+v", open)
	}
}

func TestDedupe(t *testing.T) {
	s := open(t)
	now := time.Now()
	seen, err := s.DedupeSeen("w", "repo!1!sha1")
	if err != nil || seen {
		t.Fatalf("unmarked key should be unseen: %v %v", seen, err)
	}
	if err := s.DedupeMark("w", "repo!1!sha1", now); err != nil {
		t.Fatal(err)
	}
	seen, err = s.DedupeSeen("w", "repo!1!sha1")
	if err != nil || !seen {
		t.Fatalf("marked key should be seen: %v %v", seen, err)
	}
	seen, _ = s.DedupeSeen("w", "repo!1!sha2")
	if seen {
		t.Fatal("new head SHA should be unseen")
	}
}

func TestTimers(t *testing.T) {
	s := open(t)
	now := time.Now()
	due := &Timer{RunID: "r1", State: "building", Kind: "timeout", FireAt: now.Add(-time.Minute)}
	future := &Timer{RunID: "r1", State: "building", Kind: "probe", FireAt: now.Add(time.Hour), Every: 3 * time.Minute}
	for _, tm := range []*Timer{due, future} {
		if err := s.ArmTimer(tm); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.DueTimers(now)
	if err != nil || len(got) != 1 || got[0].ID != due.ID {
		t.Fatalf("due = %+v, %v", got, err)
	}
	all, _ := s.ActiveTimers()
	if len(all) != 2 {
		t.Fatalf("active = %d", len(all))
	}
	if all[1].Every != 3*time.Minute {
		t.Errorf("cadence lost: %v", all[1].Every)
	}
	if err := s.DisarmRunTimers("r1"); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ActiveTimers()
	if len(all) != 0 {
		t.Fatalf("after disarm: %+v", all)
	}
}

func TestSignalBuffer(t *testing.T) {
	s := open(t)
	now := time.Now()
	if err := s.BufferSignal("r1", "mr-ready", map[string]any{"iid": float64(7)}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.BufferSignal("r1", "ticket-in-progress", nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	sigs, err := s.PendingSignals("r1")
	if err != nil || len(sigs) != 2 || sigs[0].Event != "mr-ready" {
		t.Fatalf("pending = %+v, %v", sigs, err)
	}
	if err := s.ConsumeSignal(sigs[0].ID); err != nil {
		t.Fatal(err)
	}
	sigs, _ = s.PendingSignals("r1")
	if len(sigs) != 1 || sigs[0].Event != "ticket-in-progress" {
		t.Fatalf("after consume = %+v", sigs)
	}
}

func TestEventsAndSteps(t *testing.T) {
	s := open(t)
	e := &core.Event{Workspace: "ws", RunID: "r1", State: "s1", Kind: "transition", Data: map[string]any{"to": "s2"}, At: time.Now()}
	if err := s.AppendEvent(e); err != nil || e.ID == 0 {
		t.Fatalf("append: %v id=%d", err, e.ID)
	}
	st := &core.StepExecution{RunID: "r1", State: "s1", Kind: core.StepAgent, Attempt: 1, StartedAt: time.Now()}
	if err := s.InsertStep(st); err != nil || st.ID == 0 {
		t.Fatalf("insert step: %v", err)
	}
	st.Outcome = "ok"
	st.SessionID = "sess-123"
	st.CostUSD = 0.42
	st.EndedAt = time.Now()
	if err := s.UpdateStep(st); err != nil {
		t.Fatal(err)
	}
	steps, err := s.StepsForRun("r1")
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps = %+v, %v", steps, err)
	}
	if steps[0].SessionID != "sess-123" || steps[0].Outcome != "ok" || steps[0].CostUSD != 0.42 {
		t.Errorf("step = %+v", steps[0])
	}
	evs, err := s.EventsForRun("r1")
	if err != nil || len(evs) != 1 || evs[0].Kind != "transition" {
		t.Fatalf("events = %+v, %v", evs, err)
	}
}
