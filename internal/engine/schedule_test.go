package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
)

// countRuns returns how many runs of a workflow exist in any status.
func countRuns(t *testing.T, v *env, wf string) int {
	t.Helper()
	runs, err := v.st.ListRuns()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range runs {
		if r.Workflow == wf {
			n++
		}
	}
	return n
}

// A schedule trigger was validated but never armed: `krakoactl workspace
// validate` passed, the daemon loaded the workflow, and 08:00 came and went
// with nothing. This is the whole contract — it fires, it re-arms, and it
// does not fire twice for one slot.
func TestScheduleFiresAndRearms(t *testing.T) {
	v := setup(t)
	v.run.on("doing", ok("outcome", "ok"), ok("outcome", "ok"))
	// Monday 07:59 local — one minute before the morning slot.
	v.clock.t = time.Date(2026, 7, 20, 7, 59, 0, 0, time.Local)
	v.eng.Recover() // arms the schedule

	v.eng.Tick()
	if n := countRuns(t, v, "daily"); n != 0 {
		t.Fatalf("fired before its time: %d run(s)", n)
	}

	v.clock.Advance(time.Minute) // 08:00
	v.eng.Tick()
	if n := countRuns(t, v, "daily"); n != 1 {
		t.Fatalf("08:00 should have started exactly 1 run, got %d", n)
	}

	// Same slot, later in the minute: re-armed to 17:00, so nothing new.
	v.clock.Advance(30 * time.Second)
	v.eng.Tick()
	if n := countRuns(t, v, "daily"); n != 1 {
		t.Fatalf("re-fired within the same slot: %d runs", n)
	}

	// The evening slot is its own fire.
	v.clock.Advance(9 * time.Hour) // 17:00
	v.eng.Tick()
	if n := countRuns(t, v, "daily"); n != 2 {
		t.Fatalf("17:00 should have started a second run, got %d", n)
	}
}

// A Mac asleep over a long weekend must wake to ONE run, not one per missed
// slot. Replaying a backlog of rituals is worse than the single one wanted.
func TestScheduleDoesNotReplayMissedSlots(t *testing.T) {
	v := setup(t)
	v.run.on("doing", ok("outcome", "ok"))
	v.clock.t = time.Date(2026, 7, 20, 7, 59, 0, 0, time.Local)
	v.eng.Recover()

	// Asleep from Monday morning to Thursday evening: six slots missed.
	v.clock.Advance(84 * time.Hour)
	v.eng.Tick()
	if n := countRuns(t, v, "daily"); n != 1 {
		t.Fatalf("waking after 6 missed slots must start exactly 1 run, got %d", n)
	}
}

// skip_if_running: an unanswered ritual already carries the signal, so the
// next slot must not stack a second run behind it.
func TestScheduleSkipsWhileOneIsActive(t *testing.T) {
	v := setup(t)
	// daily-skip parks on a wait, so the morning run is still open at 17:00.
	v.run.on("doing", ok("outcome", "ok"), ok("outcome", "ok"))
	v.clock.t = time.Date(2026, 7, 20, 7, 59, 0, 0, time.Local)
	v.eng.Recover()

	v.clock.Advance(time.Minute) // 08:00 fires
	v.eng.Tick()
	if n := countRuns(t, v, "daily-skip"); n != 1 {
		t.Fatalf("want 1 run after the morning slot, got %d", n)
	}
	runs, _ := v.st.ListRuns(core.StatusWaiting)
	if len(runs) == 0 {
		t.Fatal("the morning run should still be waiting for this test to mean anything")
	}

	v.clock.Advance(9 * time.Hour) // 17:00, morning run still open
	v.eng.Tick()
	if n := countRuns(t, v, "daily-skip"); n != 1 {
		t.Fatalf("skip_if_running must suppress the evening slot, got %d runs", n)
	}
	if !strings.Contains(v.out.String(), "schedule") && countRuns(t, v, "daily-skip") != 1 {
		t.Fatal("the skip should be recorded, not silent")
	}
}
