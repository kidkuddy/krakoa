package engine

import (
	"encoding/json"
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
	// A gate that has said the same thing three times has said it. Nagging had
	// no cap at all, so one unanswered gate repeated itself fourteen times over
	// three days, and a week's worth of that is what made the DM unreadable.
	// Past the cap a gate is not forgotten — it moves into the morning digest,
	// which says the same thing once for all of them.
	gateNagLimit = 3
	// When the digest goes out. One notice, at the start of the working day,
	// naming every gate that has stopped nagging on its own.
	gateDigestHour = 9
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
		// Before spending a human's attention on this gate again, ask the world
		// whether the gate is still true. A gate is a claim, and claims expire.
		if e.recheckGateLocked(g) {
			continue // gate closed itself; the run moved on
		}
		before := *n
		switch {
		case g.RunID == "":
			// Engine-level (watcher) gates are informational: never nagged,
			// never escalated. They live in the UI's infra strip and in
			// `krakoactl gates`, which is exactly where noise belongs. They do
			// get ONE delivery, and deliverLocked holds it out of work hours —
			// this is where a gate raised overnight is carried out in the
			// morning instead of waking someone who cannot act on it.
			if !undelivered(g) || !inWorkHours(now) {
				continue
			}
			if !n.LastTry.IsZero() && now.Sub(n.LastTry) < deliverySweepEvery*time.Duration(1<<min(n.Tries, 4)) {
				continue
			}
			n.LastTry, n.Tries = now, n.Tries+1
			e.deliverLocked(g)
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
		case now.Sub(g.CreatedAt) > gateNagAfter && now.Sub(n.LastTry) > gateNagAfter && inWorkHours(now) && n.Tries <= gateNagLimit:
			// Tries counts deliveries and nags together — first sight records 1,
			// so this is three nags. A gate that burned the budget on delivery
			// retries nags less, which is the direction to err in.
			n.LastTry = now
			n.Tries++
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
	e.digestSilencedGatesLocked(gates, nags, now)
}

// digestSilencedGatesLocked says once, each working morning, what the nag cap
// has stopped saying. Capping the nag without this would quietly lose gates;
// the point is to stop repeating them, not to stop mentioning them.
func (e *Engine) digestSilencedGatesLocked(gates []*core.Gate, nags map[string]*store.GateNag, now time.Time) {
	if !inWorkHours(now) || now.Hour() != gateDigestHour {
		return
	}
	if !e.lastGateDigest.IsZero() && e.lastGateDigest.YearDay() == now.YearDay() && e.lastGateDigest.Year() == now.Year() {
		return
	}
	var silenced []*core.Gate
	byWorkspace := map[string]bool{}
	for _, g := range gates {
		if g.RunID == "" {
			continue // watcher gates never nagged, so nothing has gone quiet
		}
		if n, ok := nags[g.ID]; ok && n.Tries > gateNagLimit {
			silenced = append(silenced, g)
			byWorkspace[g.Workspace] = true
		}
	}
	if len(silenced) == 0 {
		return
	}
	e.lastGateDigest = now
	for ws := range byWorkspace {
		var b strings.Builder
		n := 0
		for _, g := range silenced {
			if g.Workspace != ws {
				continue
			}
			n++
			fmt.Fprintf(&b, "\n• %s (%s, open %s)", firstLine(g.Payload, 140), g.ID, since(g.CreatedAt, now))
		}
		e.notifyLocked(&core.Notice{
			ID: fmt.Sprintf("gate-digest-%s-%s", ws, now.Format("2006-01-02")), Workspace: ws,
			Kind: core.NoticeStuck,
			Text: fmt.Sprintf("%d gate(s) still open and no longer nagging:%s\n`krakoactl answer <id> <option>`", n, b.String()),
		})
	}
}

// firstLine trims a gate payload to something that reads in a list.
func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// recheckGateLocked re-evaluates the condition that raised a gate, if its state
// declares one. It reports whether the gate was resolved without the human.
//
// The recheck is read from the workflow definition rather than stored on the
// gate: the definition is the only place it can be kept honest across a
// workflow edit, and it costs one map lookup.
func (e *Engine) recheckGateLocked(g *core.Gate) bool {
	if g.RunID == "" {
		return false // engine-level alerts retire via closeStaleAlertsLocked
	}
	run, err := e.Store.GetRun(g.RunID)
	if err != nil {
		return false
	}
	// A gate whose run is already terminal has nothing to answer. finishLocked
	// retires these, but a run that ended before that shipped still carries one.
	if run.Status.Terminal() {
		e.retireGatesLocked(*run, "run terminal")
		return true
	}
	def, err := e.runDef(run)
	if err != nil {
		return false
	}
	st := def.States[g.State]
	if st.Gate == nil || st.Gate.Recheck == nil || st.Gate.Recheck.Command == "" {
		return false
	}
	ws := e.Workspaces[run.Workspace]
	if ws == nil {
		return false
	}
	// Queue it rather than run it here. A recheck shells out to GitLab, and
	// doing that under the engine lock would stall every other run for the
	// length of a network call — and deadlock outright wherever Spawn is
	// synchronous. The pending queue drains after the lock is released, so the
	// gate keeps its turn in this sweep and the outcome lands before the next.
	gateID, wsPath := g.ID, ws.Path
	command := core.Interpolate(run, st.Gate.Recheck.Command)
	e.pending = append(e.pending, func() { e.runGateRecheck(gateID, wsPath, command) })
	return false
}

// runGateRecheck executes a gate's recheck command and, if it decides
// something the gate's state can act on, closes the gate and advances the run.
// A recheck that errors, says "pending", or names an outcome with no edge stays
// silent: it may not speak for the human, only relieve them.
func (e *Engine) runGateRecheck(gateID, wsPath, command string) {
	defer e.drain()
	out, execErr := e.Exec(wsPath, command)
	var result map[string]any
	if execErr == nil {
		execErr = json.Unmarshal(out, &result)
	}
	outcome, _ := result["outcome"].(string)
	if execErr != nil || outcome == "" || outcome == "pending" {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	g, err := e.Store.GetGate(gateID)
	if err != nil || g.Status != core.GateOpen {
		return // answered by the human while we were probing — they win
	}
	run, err := e.Store.GetRun(g.RunID)
	if err != nil || run.Status.Terminal() {
		return
	}
	def, err := e.runDef(run)
	if err != nil {
		return
	}
	if _, ok := def.States[g.State].On[outcome]; !ok {
		return
	}
	e.event(run.ID, g.State, "gate-recheck", map[string]any{"gate": g.ID, "outcome": outcome, "command": command}, run.Workspace)
	e.Store.CancelGate(g.ID, e.Clock.Now())
	e.applyLocked(def, core.Advance(def, *run, outcome, result))
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
