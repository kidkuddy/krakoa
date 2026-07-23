package engine

import (
	"testing"

	"github.com/kidkuddy/krakoa/internal/core"
)

// `mr-ready CAL-657 → no matching run` printed every 60 seconds for an hour
// while two reviewed, ready MRs sat unmerged. An event nobody consumes must
// raise exactly one gate, and stay quiet once acknowledged.
func TestUnconsumedAlertRaisesOneGate(t *testing.T) {
	v := setup(t)
	v.eng.Workspaces["demo"].Alerts = []string{"mr-ready"}

	fire := func() {
		v.eng.mu.Lock()
		v.eng.raiseAlertLocked("demo", EmittedEvent{
			Event: "mr-ready", Key: "CAL-9",
			Payload: map[string]any{"message": "!103 is ready and no run owns it"},
		})
		v.eng.mu.Unlock()
		v.eng.drain()
	}

	openAlerts := func() []*core.Gate {
		var out []*core.Gate
		for _, g := range v.st.MustOpenGates() {
			if g.RunID == "" && len(g.State) > 6 && g.State[:6] == "alert:" {
				out = append(out, g)
			}
		}
		return out
	}

	fire()
	fire()
	fire()
	got := openAlerts()
	if len(got) != 1 {
		t.Fatalf("three identical unconsumed events must leave ONE gate, got %d", len(got))
	}
	if got[0].Payload != "!103 is ready and no run owns it" {
		t.Errorf("payload = %q; the emitter's own sentence should win", got[0].Payload)
	}

	// Acknowledging must silence that key for good, not for one sweep.
	if err := v.eng.AnswerGate(got[0].ID, "acknowledged", nil, "test"); err != nil {
		t.Fatal(err)
	}
	fire()
	if n := len(openAlerts()); n != 0 {
		t.Fatalf("acknowledged alert came back (%d open)", n)
	}

	// A different key is a different problem and must still speak up.
	v.eng.mu.Lock()
	v.eng.raiseAlertLocked("demo", EmittedEvent{Event: "mr-ready", Key: "CAL-10"})
	v.eng.mu.Unlock()
	v.eng.drain()
	if n := len(openAlerts()); n != 1 {
		t.Fatalf("a new key must raise its own gate, got %d", n)
	}
}

// An event the workspace did not declare stays a log line.
func TestUndeclaredEventDoesNotAlert(t *testing.T) {
	v := setup(t)
	v.eng.mu.Lock()
	v.eng.raiseAlertLocked("demo", EmittedEvent{Event: "chatter", Key: "x"})
	v.eng.mu.Unlock()
	v.eng.drain()
	if gates := v.st.MustOpenGates(); len(gates) != 0 {
		t.Fatalf("undeclared event raised a gate: %+v", gates[0])
	}
}
