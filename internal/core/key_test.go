package core

import "testing"

func keyRun() *Run {
	return &Run{
		ID:     "callab-scrum-263dc005",
		Inputs: map[string]any{"ceremony": "planning"},
		Context: map[string]any{
			"filing": map[string]any{"ticket_id": "CAL-648"},
		},
	}
}

// The bug this exists for: a composite key went through Resolve, which returns
// anything not starting with "$" as a literal, so every ceremony of every day
// stamped the template itself and collided on one thread.
func TestResolveKeyInterpolatesCompositeKeys(t *testing.T) {
	got, ok := ResolveKey(keyRun(), "scrum-$input.ceremony")
	if !ok || got != "scrum-planning" {
		t.Fatalf("ResolveKey = %q, %v; want \"scrum-planning\", true", got, ok)
	}
}

func TestResolveKeyWholeStringRefStillWorks(t *testing.T) {
	got, ok := ResolveKey(keyRun(), "$filing.ticket_id")
	if !ok || got != "CAL-648" {
		t.Fatalf("ResolveKey = %q, %v; want \"CAL-648\", true", got, ok)
	}
}

// A key naming a state that has not run yet must WAIT, not stamp the ref as the
// key — task-lifecycle depends on this to key its thread by ticket id.
func TestResolveKeyWaitsForUnresolvedRefs(t *testing.T) {
	run := &Run{ID: "r1", Inputs: map[string]any{}, Context: map[string]any{}}
	for _, tmpl := range []string{"$filing.ticket_id", "scrum-$input.ceremony", "a-$input.x-b"} {
		if got, ok := ResolveKey(run, tmpl); ok {
			t.Errorf("ResolveKey(%q) = %q, true; want not-ready", tmpl, got)
		}
	}
}

// An optional ref resolving to nil is not a key.
func TestResolveKeyRejectsNilAndEmpty(t *testing.T) {
	run := &Run{ID: "r1", Inputs: map[string]any{}, Context: map[string]any{}}
	if _, ok := ResolveKey(run, "$asking?"); ok {
		t.Error("an unanswered optional ref produced a key")
	}
	if _, ok := ResolveKey(run, ""); ok {
		t.Error("an empty template produced a key")
	}
}

// A literal key is a key: not every workflow threads on a ref.
func TestResolveKeyPassesLiteralsThrough(t *testing.T) {
	got, ok := ResolveKey(keyRun(), "nightly-sweep")
	if !ok || got != "nightly-sweep" {
		t.Fatalf("ResolveKey = %q, %v; want \"nightly-sweep\", true", got, ok)
	}
}

// A resolved value containing a "$" must not read as an unresolved ref — the
// count comes from the substitution, never from scanning the result.
func TestResolveKeyToleratesDollarsInValues(t *testing.T) {
	run := &Run{ID: "r1", Inputs: map[string]any{"label": "cost-$5"}, Context: map[string]any{}}
	got, ok := ResolveKey(run, "run-$input.label")
	if !ok || got != "run-cost-$5" {
		t.Fatalf("ResolveKey = %q, %v; want \"run-cost-$5\", true", got, ok)
	}
}
