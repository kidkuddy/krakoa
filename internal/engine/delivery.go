package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
	"github.com/kidkuddy/krakoa/internal/store"
)

// A gate whose ping was dropped used to be recorded and forgotten — 30 gates
// hit that path, one of them stalling a ticket for 17 hours. Delivery now
// retries until it lands or the gate is answered, real decisions nag on a
// cadence, and a day-old gate escalates naming its recommended default.
// Watcher noise gets the opposite treatment: it never nags.

const (
	deliverySweepEvery = 10 * time.Minute
	gateNagAfter       = 2 * time.Hour
	gateEscalateAfter  = 24 * time.Hour
)

func (e *Engine) sweepDeliveries() {
	defer e.drain()
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Clock.Now()
	if now.Sub(e.lastDeliverySweep) < deliverySweepEvery {
		return
	}
	e.lastDeliverySweep = now

	gates, err := e.Store.OpenGates()
	if err != nil {
		return
	}
	nags, err := e.Store.GateNags()
	if err != nil {
		return
	}
	for _, g := range gates {
		n, known := nags[g.ID]
		if !known {
			// First sight of this gate in this table — either it just opened
			// (its opening ping already went out) or the daemon restarted and
			// the record predates the table. Either way: record the moment and
			// say nothing. Not knowing is not a reason to ping, and a restart
			// must never re-nag every open gate, which it did twice today.
			n = &store.GateNag{GateID: g.ID, LastTry: now, Tries: 1, Escalated: now.Sub(g.CreatedAt) > gateEscalateAfter}
			e.Store.SaveGateNag(n)
			continue
		}
		before := *n
		switch {
		case g.RunID == "":
			// Engine-level (watcher) gates are informational: never redelivered,
			// never nagged. They live in the UI's infra strip and in
			// `krakoactl gates`, which is exactly where noise belongs.
		case now.Sub(g.CreatedAt) > gateEscalateAfter && undelivered(g) && n.Tries == 0:
			// Older than the escalation window: a day-late delivery retry helps
			// nobody, the escalation below does.
			n.Tries = 1
		case undelivered(g):
			// Backoff on retries so a niffty outage doesn't become its own
			// flood: 10m, 20m, 40m… capped by the sweep cadence.
			if !n.LastTry.IsZero() && now.Sub(n.LastTry) < deliverySweepEvery*time.Duration(1<<min(n.Tries, 4)) {
				continue
			}
			n.LastTry, n.Tries = now, n.Tries+1
			e.event(g.RunID, g.State, "gate-delivery-retry", map[string]any{"gate": g.ID, "attempt": n.Tries}, g.Workspace)
			e.deliverLocked(g)
		case now.Sub(g.CreatedAt) > gateEscalateAfter && !n.Escalated:
			n.Escalated = true
			n.LastTry = now
			e.notifyLocked(&core.Notice{
				ID: "escalate-" + g.ID, Workspace: g.Workspace, RunID: g.RunID, State: g.State,
				Kind: core.NoticeStuck, Text: escalationText(g, now),
			})
		case now.Sub(g.CreatedAt) > gateNagAfter && now.Sub(n.LastTry) > gateNagAfter && inWorkHours(now):
			n.LastTry = now
			e.notifyLocked(&core.Notice{
				ID: fmt.Sprintf("nag-%s-%d", g.ID, now.Unix()), Workspace: g.Workspace, RunID: g.RunID, State: g.State,
				Kind: core.NoticeStuck,
				Text: fmt.Sprintf("still waiting on you: %s (%s, open %s)", g.Payload, g.ID, since(g.CreatedAt, now)),
			})
		}
		if *n != before {
			if err := e.Store.SaveGateNag(n); err != nil {
				e.Log.Printf("save gate nag %s: %v", g.ID, err)
			}
		}
	}
	if err := e.Store.PruneGateNags(); err != nil {
		e.Log.Printf("prune gate nags: %v", err)
	}
}

// undelivered reports whether any channel failed to carry this gate.
func undelivered(g *core.Gate) bool {
	if len(g.Delivery) == 0 {
		return true
	}
	for _, v := range g.Delivery {
		if v != "ok" {
			return true
		}
	}
	return false
}

func escalationText(g *core.Gate, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "open %s with no answer: %s", since(g.CreatedAt, now), g.Payload)
	if len(g.Options) > 0 {
		fmt.Fprintf(&b, "\nrecommended default: %s — `krakoactl answer %s %s`", g.Options[0], g.ID, g.Options[0])
	}
	return b.String()
}

func since(from, now time.Time) string {
	d := now.Sub(from)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
