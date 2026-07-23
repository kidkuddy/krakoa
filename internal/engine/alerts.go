package engine

import (
	"fmt"
	"strings"

	"github.com/kidkuddy/krakoa/internal/core"
)

// An emitted event with no consumer used to be computed, logged and dropped.
// `mr-ready CAL-657 → no matching run` printed every 60 seconds while two
// reviewed, ready MRs sat unmerged and nothing on earth said so. A workspace
// declares which events must never disappear that way; one of those with no
// taker raises a gate — one per (event, key), updated in place, and never
// re-raised for a key you already acknowledged.

const alertStatePrefix = "alert:"

// alertAckWatcher is the dedupe namespace for acknowledged alerts.
const alertAckWatcher = "alert-ack"

// AlertOptions are the choices on an alert gate. `acknowledged` silences this
// key for good; the alert returns if the same event fires with a new key.
var AlertOptions = []string{"acknowledged"}

func isAlert(ws []string, event string) bool {
	for _, a := range ws {
		if a == event {
			return true
		}
	}
	return false
}

// raiseAlertLocked opens or updates the standing gate for one unconsumed event.
func (e *Engine) raiseAlertLocked(wsName string, ev EmittedEvent) {
	ws := e.Workspaces[wsName]
	if ws == nil || !isAlert(ws.Alerts, ev.Event) {
		return
	}
	key := ev.Key
	if key == "" {
		key = ev.Event
	}
	state := alertStatePrefix + ev.Event + ":" + key
	if acked, _ := e.Store.DedupeSeen(alertAckWatcher, state); acked {
		return
	}
	payload := alertText(ev)
	for _, g := range e.Store.MustOpenGates() {
		if g.RunID == "" && g.Workspace == wsName && g.State == state {
			if g.Payload != payload {
				e.Store.SetGatePayload(g.ID, payload)
			}
			return
		}
	}
	e.openGateLocked(core.Run{ID: "", Workspace: wsName, State: state}, core.ActionOpenGate{
		State: state, Kind: core.GateChoice, Payload: payload, Options: AlertOptions,
	})
}

// alertText prefers the emitter's own sentence: a script knows why it is
// shouting better than the engine does.
func alertText(ev EmittedEvent) string {
	if m, ok := ev.Payload["message"].(string); ok && m != "" {
		return m
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s fired for %s and nothing consumed it", ev.Event, ev.Key)
	for _, k := range []string{"mr_url", "repo", "mr_iid", "ticket_id", "target_branch"} {
		if v, ok := ev.Payload[k]; ok {
			fmt.Fprintf(&b, "\n%s: %v", k, v)
		}
	}
	return b.String()
}

// closeStaleAlertsLocked retires alert gates whose condition a sweep no longer
// reports. Without this a transient truth is a permanent interruption: an MR
// flagged as orphaned 34 seconds after it was opened stayed on the page long
// after its ticket link landed.
//
// ponytail: assumes the sweeping watcher is the only source of this
// workspace's alerts, which holds while one command watcher emits all of them.
// Give a second watcher its own alert events and this needs scoping per
// watcher.
func (e *Engine) closeStaleAlertsLocked(wsName string, events []EmittedEvent) {
	ws := e.Workspaces[wsName]
	if ws == nil || len(ws.Alerts) == 0 {
		return
	}
	live := map[string]bool{}
	for _, ev := range events {
		key := ev.Key
		if key == "" {
			key = ev.Event
		}
		live[alertStatePrefix+ev.Event+":"+key] = true
	}
	for _, g := range e.Store.MustOpenGates() {
		if g.RunID != "" || g.Workspace != wsName || !strings.HasPrefix(g.State, alertStatePrefix) {
			continue
		}
		if live[g.State] {
			continue
		}
		e.Store.CancelGate(g.ID, e.Clock.Now())
		// forget the ack too, so a genuine recurrence can speak up again
		e.Store.ClearDedupe(alertAckWatcher, g.State)
		e.event("", g.State, "alert-cleared", map[string]any{"gate": g.ID}, wsName)
	}
}

// ackAlertLocked silences one alert key permanently.
func (e *Engine) ackAlertLocked(g *core.Gate) {
	e.Store.DedupeMark(alertAckWatcher, g.State, e.Clock.Now())
	e.event("", g.State, "alert-acknowledged", nil, g.Workspace)
}
