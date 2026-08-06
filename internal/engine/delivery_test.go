package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
)

// A daemon restart must not re-nag every open gate. The nag state used to live
// in memory, so two restarts in one morning sent two rounds of "still waiting
// on you" for gates that had already been nagged.
func TestNaggingSurvivesRestart(t *testing.T) {
	v := setup(t)
	// 09:00 on a Wednesday: inside work hours, so nagging is allowed.
	v.clock.t = time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)

	g := &core.Gate{
		ID: "g-old", Workspace: "demo", RunID: "run-1", State: "stalled",
		Kind: core.GateChoice, Payload: "builder silent", Options: []string{"rerun", "abandon"},
		Status: core.GateOpen, CreatedAt: v.clock.t.Add(-5 * time.Hour), // long past the nag threshold
		Delivery: map[string]string{"console": "ok"},
	}
	if err := v.st.CreateGate(g); err != nil {
		t.Fatal(err)
	}

	nags := func() int { return strings.Count(v.out.String(), "still waiting on you") }

	// First sweep ever: the gate is unknown to the bookkeeping. Record, stay
	// silent — not knowing is not a reason to ping.
	v.eng.sweepDeliveries()
	if nags() != 0 {
		t.Fatalf("first sight of a gate must be silent, got %d nag(s)", nags())
	}

	// A restart drops every in-memory field; the store must carry the state.
	restarted := &Engine{
		Store: v.st, Runner: v.eng.Runner, Clock: v.clock, Channels: v.eng.Channels,
		Workspaces: v.eng.Workspaces, DataDir: t.TempDir(),
		Spawn: func(f func()) { f() }, watcherHealth: map[string]*watcherState{},
		checks: map[string]*checkResult{}, Exec: v.eng.Exec, Log: v.eng.Log,
	}
	v.clock.Advance(time.Minute)
	restarted.sweepDeliveries()
	if nags() != 0 {
		t.Fatalf("restart re-nagged an already-known gate (%d nag(s))", nags())
	}

	// Once the interval genuinely passes, it does nag — silence is not the goal.
	v.clock.Advance(gateNagAfter + time.Minute)
	restarted.sweepDeliveries()
	if nags() != 1 {
		t.Fatalf("want exactly 1 nag after the interval, got %d", nags())
	}
}

// Nagging used to have no cap: one unanswered gate repeated itself fourteen
// times over three days. Three nags, then the morning digest carries it.
func TestNagCapsThenDigests(t *testing.T) {
	v := setup(t)
	v.clock.t = time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC) // Wednesday

	g := &core.Gate{
		ID: "g-loud", Workspace: "demo", RunID: "run-1", State: "stalled",
		Kind: core.GateChoice, Payload: "builder silent on \"thing\"", Options: []string{"rerun", "abandon"},
		Status: core.GateOpen, CreatedAt: v.clock.t.Add(-5 * time.Hour),
		Delivery: map[string]string{"console": "ok"},
	}
	if err := v.st.CreateGate(g); err != nil {
		t.Fatal(err)
	}
	nags := func() int { return strings.Count(v.out.String(), "still waiting on you") }

	v.eng.sweepDeliveries() // first sight: silent
	for i := 0; i < 6; i++ {
		v.clock.Advance(gateNagAfter + time.Minute)
		v.eng.sweepDeliveries()
	}
	if n := nags(); n != gateNagLimit {
		t.Fatalf("want %d nags then silence, got %d", gateNagLimit, n)
	}

	// The next working morning it is named once, not nagged again.
	v.clock.t = time.Date(2026, 7, 23, gateDigestHour, 0, 0, 0, time.UTC)
	v.eng.lastDeliverySweep = time.Time{}
	v.eng.sweepDeliveries()
	if !strings.Contains(v.out.String(), "no longer nagging") {
		t.Fatal("a silenced gate must still appear in the morning digest")
	}
	if n := nags(); n != gateNagLimit {
		t.Fatalf("digest must not nag again, got %d nags", n)
	}

	// And only once that day.
	before := strings.Count(v.out.String(), "no longer nagging")
	v.clock.Advance(deliverySweepEvery + time.Minute)
	v.eng.sweepDeliveries()
	if got := strings.Count(v.out.String(), "no longer nagging"); got != before {
		t.Fatalf("digest fired twice in one day (%d -> %d)", before, got)
	}
}

// A watcher breaking at 02:00 cannot be fixed before the morning. The gate
// opens either way; the DM waits.
func TestWatcherGateHeldOutOfHours(t *testing.T) {
	v := setup(t)
	v.clock.t = time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC) // 02:00 Wednesday

	g := &core.Gate{
		ID: "g-night", Workspace: "demo", RunID: "", State: watcherStatePrefix + "draft-mr-watch",
		Kind: core.GateChoice, Payload: "watcher draft-mr-watch has failed 3 consecutive sweeps",
		Options: WatcherGateOptions, Status: core.GateOpen, CreatedAt: v.clock.t,
	}
	if err := v.st.CreateGate(g); err != nil {
		t.Fatal(err)
	}
	v.eng.deliverLocked(g)
	v.eng.drain()
	if strings.Contains(v.out.String(), "draft-mr-watch") {
		t.Fatal("a run-less gate must not be delivered at 02:00")
	}

	v.eng.sweepDeliveries() // first sight, still night
	v.clock.t = time.Date(2026, 7, 22, 9, 30, 0, 0, time.UTC)
	v.eng.lastDeliverySweep = time.Time{}
	v.eng.sweepDeliveries()
	v.eng.drain()
	if !strings.Contains(v.out.String(), "draft-mr-watch") {
		t.Fatal("the held gate must be delivered once the working day starts")
	}
}
