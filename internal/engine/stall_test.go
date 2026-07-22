package engine

import (
	"testing"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
)

// the shape that stalled live: building arms mr-ready itself, and awaiting-ready
// is reachable from it; merging is downstream of both and arms nothing.
func lifecycleDef() *core.WorkflowDefinition {
	return &core.WorkflowDefinition{
		Name:  "task-lifecycle",
		Start: "building",
		States: map[string]core.State{
			"building": {Step: core.StepWait,
				Arms: []core.WaitArm{{Event: "ticket-in-review"}, {Event: "mr-ready"}},
				On:   map[string]string{"ticket-in-review": "awaiting-ready", "mr-ready": "merging"}},
			"awaiting-ready": {Step: core.StepWait,
				Arms: []core.WaitArm{{Event: "mr-ready"}},
				On:   map[string]string{"mr-ready": "merging"}},
			"merging": {Step: core.StepAgent, On: map[string]string{"merged": "done"}},
			"done":    {Terminal: true},
		},
	}
}

func TestConsumableFrom(t *testing.T) {
	def := lifecycleDef()
	cases := []struct {
		from, event string
		want        bool
	}{
		{"building", "mr-ready", true},         // armed here
		{"building", "ticket-in-review", true}, // armed here
		{"merging", "mr-ready", false},         // past it: nothing downstream arms it
		{"awaiting-ready", "mr-ready", true},   // armed here
		{"building", "never-emitted", false},   // not in the machine at all
	}
	for _, c := range cases {
		if got := consumableFrom(def, c.from, c.event); got != c.want {
			t.Errorf("consumableFrom(%s, %s) = %v, want %v", c.from, c.event, got, c.want)
		}
	}
}

func TestStateArming(t *testing.T) {
	def := lifecycleDef()
	if got := stateArming(def, "mr-ready"); got != "awaiting-ready" {
		t.Errorf("stateArming(mr-ready) = %q, want awaiting-ready (first by name)", got)
	}
	if got := stateArming(def, "never-emitted"); got != "" {
		t.Errorf("stateArming(never-emitted) = %q, want empty", got)
	}
}

func TestWatcherBackoff(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, w := range want {
		if got := watcherBackoff(i + 1); got != w {
			t.Errorf("watcherBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}
