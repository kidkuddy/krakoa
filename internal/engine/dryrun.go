package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
	"github.com/kidkuddy/krakoa/internal/runner"
	"github.com/kidkuddy/krakoa/internal/store"
	"github.com/kidkuddy/krakoa/internal/workspace"
)

// DryRun proves a workflow executes without touching the world: synthetic
// runner, in-memory store, auto-answered gates, auto-fired waits. It walks
// EVERY declared transition edge at least once — the happy path first, then
// one steered simulation per still-uncovered edge (sad paths, loops, and
// timeout arms included; a budget park hit while looping IS that edge
// working). Unwalkable edges fail the dry-run.
func DryRun(ws *workspace.Workspace, wfName string, out io.Writer) error {
	def, ok := ws.Workflows[wfName]
	if !ok {
		return fmt.Errorf("unknown workflow %q", wfName)
	}

	// declared edges
	var edges []string
	for name, st := range def.States {
		for o := range st.On {
			edges = append(edges, name+"/"+o)
		}
	}
	sort.Strings(edges)
	covered := map[string]bool{}

	// Every verb is another way in, and states only a verb can reach are only
	// walkable from it. The default entry first, then each named one.
	entries := append([]string{""}, def.EntryNames()...)
	for _, entry := range entries {
		fmt.Fprintf(out, "happy path (%s):\n", entryLabel(entry))
		if err := dryWalk(ws, def, entry, nil, covered, out); err != nil {
			return fmt.Errorf("entry %s: %w", entryLabel(entry), err)
		}
	}

	for _, edge := range edges {
		if covered[edge] {
			continue
		}
		parts := strings.SplitN(edge, "/", 2)
		var lastErr error
		for _, entry := range entries {
			fmt.Fprintf(out, "forcing edge %s (%s):\n", edge, entryLabel(entry))
			if err := dryWalk(ws, def, entry, &target{state: parts[0], outcome: parts[1]}, covered, out); err != nil {
				lastErr = fmt.Errorf("entry %s: %w", entryLabel(entry), err)
				continue
			}
			if covered[edge] {
				break
			}
		}
		if !covered[edge] {
			if lastErr != nil {
				return fmt.Errorf("edge %s: %w", edge, lastErr)
			}
			return fmt.Errorf("edge %s is unwalkable from any entry: the steered simulation never traversed it", edge)
		}
	}
	fmt.Fprintf(out, "dry-run OK: all %d transition edges walked across %d entr%s\n",
		len(edges), len(entries), map[bool]string{true: "y", false: "ies"}[len(entries) == 1])
	return nil
}

func entryLabel(entry string) string {
	if entry == "" {
		return "start"
	}
	return entry
}

// target steers one simulation: reach state, take outcome, then head home.
type target struct {
	state   string
	outcome string
	taken   bool
}

// dryWalk runs one simulation. A run that ends done, failed, or parked is a
// valid walk (parks exercise budgets and failure gates); only a stuck or
// non-terminating sim is an error.
func dryWalk(ws *workspace.Workspace, def *core.WorkflowDefinition, entryName string, tgt *target, covered map[string]bool, out io.Writer) error {
	st, err := store.Open(":memory:")
	if err != nil {
		return err
	}
	defer st.Close()

	// Homeward distances exclude timeout edges: stall-gates would otherwise
	// look like shortcuts and trap the walk in wait loops. Steering
	// distances keep every edge — reaching a sad state may need a timeout.
	distEnd := distanceTo(def, false, terminalStates(def)...)
	var distTgt map[string]int
	if tgt != nil {
		distTgt = distanceTo(def, true, tgt.state)
	}
	visited := map[string]bool{}
	choose := func(state string, on map[string]string) string {
		// agent states transition too fast for the walk loop to observe, so
		// the chooser is the only place that sees every state we pass through
		visited[state] = true
		if tgt != nil && !tgt.taken && state == tgt.state {
			tgt.taken = true
			return tgt.outcome
		}
		if tgt != nil && !tgt.taken {
			return bestByDist(def, on, distTgt, false, visited)
		}
		return bestByDist(def, on, distEnd, true, visited)
	}
	record := func(state, outcome string) { covered[state+"/"+outcome] = true }

	clk := &settableClock{t: time.Now()}
	fields := referencedFields(def)
	// forced carries the outcome the walker already chose for a state, for the
	// probe that is about to synthesize it. Asking the chooser a second time
	// there would consume the forced edge twice: the wait handler takes the
	// target, and the probe — arriving after tgt.taken — falls back to the
	// distance heuristic and fires a DIFFERENT outcome. Every state whose
	// target outcome comes from a probe was unwalkable that way, on both probe
	// kinds: command probes synthesize through Exec, agent probes through the
	// synthetic runner.
	forced := map[string]string{}
	takeForced := func(state string) (string, bool) {
		o, ok := forced[state]
		delete(forced, state)
		return o, ok
	}

	rn := &synthRunner{def: def, choose: choose, record: record, fields: fields, forced: takeForced}
	eng := New(st, rn, clk, map[string]*workspace.Workspace{ws.Name: ws}, os.TempDir())
	eng.Spawn = func(f func()) { f() }
	eng.Log = log.New(io.Discard, "", 0)
	// Command probes must never execute real workspace scripts in a dry
	// run — the synthetic exec picks the walk's outcome instead.
	probeState := ""
	eng.Exec = func(dir, command string) ([]byte, error) {
		outcome, ok := takeForced(probeState)
		if !ok {
			outcome = choose(probeState, def.States[probeState].On)
		}
		record(probeState, outcome)
		result := map[string]any{"outcome": outcome, "url": "dry-url", "status": "dry"}
		for _, f := range fields[probeState] {
			setPath(result, strings.Split(f, "."), "dry-"+f)
		}
		raw, _ := json.Marshal(result)
		return raw, nil
	}

	entry, err := def.EntryFor(entryName)
	if err != nil {
		return err
	}
	inputs := map[string]any{}
	for name, spec := range entry.Inputs {
		if spec.Default != "" {
			continue
		}
		// Inputs naming a workspace fact are checked at admission, so a
		// fabricated "dry-repo" would fail the walk before it started.
		switch {
		case name == "repo" && len(ws.Repos) > 0:
			inputs[name] = sortedKeys(ws.Repos)[0]
		case name == "env" && len(ws.Envs) > 0:
			inputs[name] = sortedKeys(ws.Envs)[0]
		default:
			inputs[name] = "dry-" + name
		}
	}
	run, err := eng.StartRun(ws.Name, def.Name, entryName, inputs, "")
	if err != nil {
		return err
	}
	// notify steps transition inside the engine, too fast for the walk loop to
	// see; their edges are covered from the run's own event trail
	defer func() {
		events, _ := st.EventsForRun(run.ID)
		for _, ev := range events {
			if ev.Kind == "notice" && def.States[ev.State].Step == core.StepNotify {
				record(ev.State, "ok")
			}
		}
	}()

	for i := 0; i < 200; i++ {
		cur, err := st.GetRun(run.ID)
		if err != nil {
			return err
		}
		visited[cur.State] = true
		switch cur.Status {
		case core.StatusDone:
			fmt.Fprintf(out, "  -> terminal %q\n", cur.State)
			return nil
		case core.StatusFailed, core.StatusCanceled:
			fmt.Fprintf(out, "  -> ended %s in %q\n", cur.Status, cur.State)
			return nil
		case core.StatusNeedsAttention:
			g, _ := st.OpenGateForRun(run.ID)
			reason := ""
			if g != nil {
				reason = g.Payload
			}
			fmt.Fprintf(out, "  -> parked in %q (%s)\n", cur.State, reason)
			// budget parks are legitimate walks; template failures are
			// definition bugs and must fail the dry-run
			if strings.Contains(reason, "resolve $") {
				return fmt.Errorf("template failure: %s", reason)
			}
			return nil
		case core.StatusGated:
			g, _ := st.OpenGateForRun(run.ID)
			if g == nil {
				return fmt.Errorf("gated but no open gate")
			}
			// A delivered payload still carrying a $ref means the template
			// is unresolvable on THIS incoming edge — the human (and the
			// Slack ping) would see raw template text. Definition bug.
			if m := payloadRefRe.FindString(g.Payload); m != "" {
				return fmt.Errorf("gate %s: payload ref %s is unresolvable on this incoming edge (bind per-edge, e.g. $last.<field>)", g.State, m)
			}
			resp, outcome, answers := gateChoice(def, cur, g, choose)
			record(cur.State, outcome)
			fmt.Fprintf(out, "  gate %s: answering %q\n", g.State, resp)
			if err := eng.AnswerGate(g.ID, resp, answers, "dry-run"); err != nil {
				return err
			}
		case core.StatusWaiting:
			state := def.States[cur.State]
			// Choose the outcome first, THEN pick the arm that can deliver
			// it: a state armed with both events and a probe (building waits
			// on the builder AND probes for its death) must be able to walk
			// its event edges, not only the probe's.
			outcome := choose(cur.State, state.On)
			if arm := armFor(state, outcome); arm == nil {
				if p := firstProbe(state); p != nil && outcome != "timeout" {
					probeState, forced[cur.State] = cur.State, outcome
					fmt.Fprintf(out, "  wait %s: firing probe\n", cur.State)
					clk.Advance(p.Every.D() + time.Second)
					eng.Tick()
					continue
				}
			}
			if arm := armFor(state, outcome); arm != nil {
				key := ""
				if arm.Correlate != "" {
					if v, err := core.Resolve(cur, arm.Correlate); err == nil {
						key = fmt.Sprintf("%v", v)
					}
				}
				record(cur.State, outcome)
				fmt.Fprintf(out, "  wait %s: firing event %q\n", cur.State, arm.Event)
				eng.HandleEmit(ws.Name, EmittedEvent{Event: arm.Event, Key: key})
			} else {
				record(cur.State, "timeout")
				fmt.Fprintf(out, "  wait %s: firing timeout\n", cur.State)
				// the state's own probe timer comes due in the same tick; it
				// must agree with the arm we are forcing, not race it
				probeState, forced[cur.State] = cur.State, "timeout"
				clk.Advance(maxTimeout(state) + time.Second)
				eng.Tick()
			}
		case core.StatusRunning, core.StatusQueued:
			return fmt.Errorf("stuck %s in state %s", cur.Status, cur.State)
		}
	}
	return fmt.Errorf("did not terminate within 200 iterations")
}

type settableClock struct{ t time.Time }

func (c *settableClock) Now() time.Time          { return c.t }
func (c *settableClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// synthRunner picks outcomes via the walk's chooser and synthesizes every
// $state.field the definition references.
type synthRunner struct {
	def    *core.WorkflowDefinition
	choose func(state string, on map[string]string) string
	record func(state, outcome string)
	fields map[string][]string
	// forced returns the outcome the walker already picked for this state
	// (agent probes), so the chooser is not consulted — and consumed — twice.
	forced func(state string) (string, bool)
}

func (r *synthRunner) Run(_ context.Context, req runner.Request) (*runner.Result, error) {
	st := r.def.States[req.State]
	outcome, ok := r.forced(req.State)
	if !ok {
		outcome = r.choose(req.State, st.On)
	}
	r.record(req.State, outcome)
	result := map[string]any{"outcome": outcome}
	for _, f := range r.fields[req.State] {
		setPath(result, strings.Split(f, "."), "dry-"+f)
	}
	if err := os.MkdirAll(req.HandoffDir, 0o755); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(result)
	if err := os.WriteFile(filepath.Join(req.HandoffDir, "result.json"), raw, 0o644); err != nil {
		return nil, err
	}
	return &runner.Result{SessionID: "dry-run"}, nil
}

// bestByDist picks the outcome whose target minimizes dist. With avoidGates,
// gate targets only win when nothing else leads anywhere (failure gates on
// short paths must not seduce the homeward walk off the main line).
// visited breaks distance ties away from states the walk has already been
// through: two edges equally far from a terminal, one of them a loop back, is
// exactly the shape that made a happy path look like a budget overrun.
func bestByDist(def *core.WorkflowDefinition, on map[string]string, dist map[string]int, avoidGates bool, visited map[string]bool) string {
	outcomes := make([]string, 0, len(on))
	for o := range on {
		outcomes = append(outcomes, o)
	}
	sort.Strings(outcomes)
	pick := func(allowGate bool) string {
		best, bestD := "", 1<<30
		for _, o := range outcomes {
			t := on[o]
			if !allowGate && def.States[t].Step == core.StepGate {
				continue
			}
			d, ok := dist[t]
			if !ok {
				continue
			}
			if d < bestD || (d == bestD && visited[on[best]] && !visited[t]) {
				best, bestD = o, d
			}
		}
		return best
	}
	if avoidGates {
		if b := pick(false); b != "" {
			return b
		}
	}
	if b := pick(true); b != "" {
		return b
	}
	if len(outcomes) == 0 {
		return ""
	}
	return outcomes[0]
}

// gateChoice maps the walk's chosen outcome back to a gate response.
func gateChoice(def *core.WorkflowDefinition, run *core.Run, g *core.Gate, choose func(string, map[string]string) string) (resp, outcome string, answers map[string]any) {
	st := def.States[run.State]
	outcome = choose(run.State, st.On)
	switch g.Kind {
	case core.GateQuestion:
		return "answered", "answered", map[string]any{"dry-run": "synthetic answer"}
	case core.GateApproval:
		if outcome == "rejected" {
			return "rejected", "rejected", nil
		}
		return "approved", "approved", nil
	default:
		return outcome, outcome, nil
	}
}

func firstProbe(st core.State) *core.ProbeSpec {
	for _, arm := range st.Arms {
		if arm.Probe != nil {
			return arm.Probe
		}
	}
	return nil
}

func armFor(st core.State, outcome string) *core.WaitArm {
	for i := range st.Arms {
		if st.Arms[i].Event != "" && st.Arms[i].Event == outcome {
			return &st.Arms[i]
		}
	}
	return nil
}

func maxTimeout(st core.State) time.Duration {
	var max time.Duration
	for _, arm := range st.Arms {
		if arm.Timeout.D() > max {
			max = arm.Timeout.D()
		}
	}
	return max
}

func terminalStates(def *core.WorkflowDefinition) []string {
	var out []string
	for name, st := range def.States {
		if st.Terminal {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// distanceTo BFSes reverse edges from the given states.
func distanceTo(def *core.WorkflowDefinition, includeTimeout bool, targets ...string) map[string]int {
	rev := map[string][]string{}
	for name, st := range def.States {
		for outcome, t := range st.On {
			if !includeTimeout && outcome == "timeout" {
				continue
			}
			rev[t] = append(rev[t], name)
		}
	}
	dist := map[string]int{}
	frontier := []string{}
	for _, t := range targets {
		dist[t] = 0
		frontier = append(frontier, t)
	}
	for len(frontier) > 0 {
		cur := frontier[0]
		frontier = frontier[1:]
		for _, pred := range rev[cur] {
			if _, seen := dist[pred]; !seen {
				dist[pred] = dist[cur] + 1
				frontier = append(frontier, pred)
			}
		}
	}
	return dist
}

// payloadRefRe flags any $ref left in human-facing text (bare or dotted).
var payloadRefRe = regexp.MustCompile(`\$[a-zA-Z_][a-zA-Z0-9_-]*`)

var dryRefRe = regexp.MustCompile(`\$([a-zA-Z0-9_-]+)((?:\.[a-zA-Z0-9_-]+)+)`)

// referencedFields collects, per state, the $state.field paths the rest of
// the definition dereferences — the synthetic results must contain them.
func referencedFields(def *core.WorkflowDefinition) map[string][]string {
	out := map[string][]string{}
	var lastFields []string
	add := func(tmpl string) {
		for _, m := range dryRefRe.FindAllStringSubmatch(tmpl, -1) {
			state, path := m[1], strings.TrimPrefix(m[2], ".")
			if state == "input" {
				continue
			}
			if state == "last" {
				// "$last" binds to whichever result caused the transition —
				// any state's synthetic result must be able to carry it
				lastFields = append(lastFields, path)
				continue
			}
			if _, ok := def.States[state]; !ok {
				continue
			}
			out[state] = append(out[state], path)
		}
	}
	defer func() {
		for name := range def.States {
			out[name] = append(out[name], lastFields...)
		}
	}()
	for _, st := range def.States {
		add(st.Instruction)
		for _, v := range st.In {
			add(v)
		}
		add(st.Message)
		if st.Gate != nil {
			add(st.Gate.Payload)
		}
		for _, arm := range st.Arms {
			add(arm.Correlate)
		}
	}
	return out
}

func setPath(m map[string]any, path []string, val any) {
	for i := 0; i < len(path)-1; i++ {
		next, ok := m[path[i]].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[path[i]] = next
		}
		m = next
	}
	m[path[len(path)-1]] = val
}
