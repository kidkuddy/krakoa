package engine

import (
	"testing"

	"github.com/kidkuddy/krakoa/internal/contact"
	"github.com/kidkuddy/krakoa/internal/core"
)

// recorder is a scoped channel that records what it was asked to carry.
type recorder struct {
	name string
	only []string
	got  []string
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Serves(ws string) bool {
	if len(r.only) == 0 {
		return true
	}
	for _, w := range r.only {
		if w == ws {
			return true
		}
	}
	return false
}

func (r *recorder) Deliver(g *core.Gate) error  { r.got = append(r.got, g.ID); return nil }
func (r *recorder) Notify(n *core.Notice) error { r.got = append(r.got, n.ID); return nil }

// A gate for personal work must never reach the work Slack, and vice versa.
// Before scoping, every channel carried every workspace's messages — which
// would have put inbox triage into a colleague-visible relay.
func TestChannelsAreScopedToTheirWorkspace(t *testing.T) {
	slack := &recorder{name: "niffty", only: []string{"callab"}}
	matrix := &recorder{name: "momo", only: []string{"personal"}}
	console := &recorder{name: "console"} // no scope: carries everything

	v := setup(t)
	v.eng.Channels = []contact.Channel{console, slack, matrix}

	v.eng.mu.Lock()
	v.eng.deliverLocked(&core.Gate{ID: "g-work", Workspace: "callab", RunID: "r1"})
	v.eng.deliverLocked(&core.Gate{ID: "g-home", Workspace: "personal", RunID: "r2"})
	v.eng.notifyLocked(&core.Notice{ID: "n-home", Workspace: "personal", RunID: "r2", Text: "inbox time"})
	v.eng.mu.Unlock()
	v.eng.drain()

	want := map[string][]string{
		"niffty":  {"g-work"},
		"momo":    {"g-home", "n-home"},
		"console": {"g-work", "g-home", "n-home"},
	}
	for _, ch := range []*recorder{slack, matrix, console} {
		got := ch.got
		exp := want[ch.name]
		if len(got) != len(exp) {
			t.Fatalf("%s carried %v, want %v", ch.name, got, exp)
		}
		for i := range exp {
			if got[i] != exp[i] {
				t.Fatalf("%s carried %v, want %v", ch.name, got, exp)
			}
		}
	}
}
