package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
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

type gateNag struct {
	lastTry time.Time
	tries   int
	escalat bool
}

func (e *Engine) sweepDeliveries() {
	defer e.drain()
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Clock.Now()
	if now.Sub(e.lastDeliverySweep) < deliverySweepEvery {
		return
	}
	e.lastDeliverySweep = now
	if e.gateNags == nil {
		e.gateNags = map[string]*gateNag{}
	}

	gates, err := e.Store.OpenGates()
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, g := range gates {
		live[g.ID] = true
		n := e.gateNags[g.ID]
		if n == nil {
			n = &gateNag{}
			e.gateNags[g.ID] = n
		}
		switch {
		case undelivered(g):
			// Backoff on retries so a niffty outage doesn't become its own
			// flood: 10m, 20m, 40m… capped by the sweep cadence.
			if !n.lastTry.IsZero() && now.Sub(n.lastTry) < deliverySweepEvery*time.Duration(1<<min(n.tries, 4)) {
				continue
			}
			n.lastTry, n.tries = now, n.tries+1
			e.event(g.RunID, g.State, "gate-delivery-retry", map[string]any{"gate": g.ID, "attempt": n.tries}, g.Workspace)
			e.deliverLocked(g)
		case g.RunID == "":
			// engine-level (watcher) gates are informational: never nag
		case now.Sub(g.CreatedAt) > gateEscalateAfter && !n.escalat:
			n.escalat = true
			e.notifyLocked(&core.Notice{
				ID: "escalate-" + g.ID, Workspace: g.Workspace, RunID: g.RunID, State: g.State,
				Kind: core.NoticeStuck, Text: escalationText(g, now),
			})
		case now.Sub(g.CreatedAt) > gateNagAfter && now.Sub(n.lastTry) > gateNagAfter && inWorkHours(now):
			n.lastTry = now
			e.notifyLocked(&core.Notice{
				ID: fmt.Sprintf("nag-%s-%d", g.ID, now.Unix()), Workspace: g.Workspace, RunID: g.RunID, State: g.State,
				Kind: core.NoticeStuck,
				Text: fmt.Sprintf("still waiting on you: %s (%s, open %s)", g.Payload, g.ID, since(g.CreatedAt, now)),
			})
		}
	}
	for id := range e.gateNags {
		if !live[id] {
			delete(e.gateNags, id)
		}
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
