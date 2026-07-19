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

// DryRun executes one simulated run of a workflow against a synthetic runner
// and an in-memory store: outcomes are chosen by shortest path to a terminal
// state, agent results synthesize every $state.field the definition
// references, gates auto-answer, waits fire their best arm. It proves the
// definition actually executes (templates resolve, transitions route,
// budgets hold) without touching the world.
func DryRun(ws *workspace.Workspace, wfName string, out io.Writer) error {
	def, ok := ws.Workflows[wfName]
	if !ok {
		return fmt.Errorf("unknown workflow %q", wfName)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		return err
	}
	defer st.Close()

	dist := distanceToTerminal(def)
	fields := referencedFields(def)
	clk := &settableClock{t: time.Now()}
	rn := &synthRunner{def: def, dist: dist, fields: fields}
	eng := New(st, rn, clk, map[string]*workspace.Workspace{ws.Name: ws}, os.TempDir())
	eng.Spawn = func(f func()) { f() }
	eng.Log = log.New(io.Discard, "", 0)
	rn.eng = eng

	inputs := map[string]any{}
	for name, spec := range def.Inputs {
		if spec.Default == "" {
			inputs[name] = "dry-" + name
		}
	}
	run, err := eng.StartRun(ws.Name, wfName, inputs, "")
	if err != nil {
		return err
	}

	visited := map[string]bool{}
	for i := 0; i < 200; i++ {
		cur, err := st.GetRun(run.ID)
		if err != nil {
			return err
		}
		visited[cur.State] = true
		switch cur.Status {
		case core.StatusDone:
			fmt.Fprintf(out, "dry-run OK: reached terminal state %q\n", cur.State)
			printTrace(out, st, run.ID)
			return nil
		case core.StatusFailed, core.StatusCanceled:
			printTrace(out, st, run.ID)
			return fmt.Errorf("dry-run ended %s in state %s", cur.Status, cur.State)
		case core.StatusNeedsAttention:
			g, _ := st.OpenGateForRun(run.ID)
			reason := ""
			if g != nil {
				reason = g.Payload
			}
			printTrace(out, st, run.ID)
			return fmt.Errorf("dry-run parked in %s: %s", cur.State, reason)
		case core.StatusGated:
			g, _ := st.OpenGateForRun(run.ID)
			if g == nil {
				return fmt.Errorf("gated but no open gate")
			}
			resp, answers := autoAnswer(def, cur, g, dist)
			fmt.Fprintf(out, "  gate %s (%s): %q -> answering %q\n", g.State, g.Kind, g.Payload, resp)
			if err := eng.AnswerGate(g.ID, resp, answers, "dry-run"); err != nil {
				return err
			}
		case core.StatusWaiting:
			state := def.States[cur.State]
			arm, timeout := bestArm(def, state, dist, visited)
			if arm != nil {
				key := ""
				if arm.Correlate != "" {
					if v, err := core.Resolve(cur, arm.Correlate); err == nil {
						key = fmt.Sprintf("%v", v)
					}
				}
				fmt.Fprintf(out, "  wait %s: firing event %q (key %q)\n", cur.State, arm.Event, key)
				eng.HandleEmit(ws.Name, EmittedEvent{Event: arm.Event, Key: key})
			} else {
				fmt.Fprintf(out, "  wait %s: no event arm, firing timeout after %s\n", cur.State, timeout)
				clk.Advance(timeout + time.Second)
				eng.Tick()
			}
		case core.StatusRunning, core.StatusQueued:
			return fmt.Errorf("dry-run stuck %s in state %s", cur.Status, cur.State)
		}
	}
	return fmt.Errorf("dry-run did not terminate within 200 iterations")
}

type settableClock struct{ t time.Time }

func (c *settableClock) Now() time.Time          { return c.t }
func (c *settableClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// synthRunner picks the outcome whose target is closest to a terminal and
// synthesizes every field later templates reference on this state.
type synthRunner struct {
	def    *core.WorkflowDefinition
	dist   map[string]int
	fields map[string][]string
	eng    *Engine
}

func (r *synthRunner) Run(_ context.Context, req runner.Request) (*runner.Result, error) {
	st := r.def.States[req.State]
	outcome := bestOutcome(r.def, st.On, r.dist)
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

// bestOutcome prefers the shortest path to a terminal through NON-gate
// targets — failure/question gates on short paths must not seduce the walk
// off the main line (a dispatch-failed -> gate -> abandoned exit is 2 hops;
// the happy tail is 5). Gate targets are the fallback when nothing else
// leads anywhere.
func bestOutcome(def *core.WorkflowDefinition, on map[string]string, dist map[string]int) string {
	outcomes := make([]string, 0, len(on))
	for o := range on {
		outcomes = append(outcomes, o)
	}
	sort.Strings(outcomes)
	pick := func(allowGate bool) string {
		best, bestD := "", 1<<30
		for _, o := range outcomes {
			target := on[o]
			if !allowGate && def.States[target].Step == core.StepGate {
				continue
			}
			if d, ok := dist[target]; ok && d < bestD {
				best, bestD = o, d
			}
		}
		return best
	}
	if best := pick(false); best != "" {
		return best
	}
	if best := pick(true); best != "" {
		return best
	}
	return outcomes[0]
}

func autoAnswer(def *core.WorkflowDefinition, run *core.Run, g *core.Gate, dist map[string]int) (string, map[string]any) {
	switch g.Kind {
	case core.GateQuestion:
		return "answered", map[string]any{"dry-run": "synthetic answer"}
	case core.GateApproval:
		return "approved", nil
	default: // choice: option leading closest to a terminal
		st := def.States[run.State]
		best, bestD := g.Options[0], 1<<30
		for _, opt := range g.Options {
			if target, ok := st.On[opt]; ok {
				if d, ok := dist[target]; ok && d < bestD {
					best, bestD = opt, d
				}
			}
		}
		return best, nil
	}
}

// bestArm picks the event arm whose outcome leads closest to a terminal
// (already-visited targets penalized to break loops); returns
// (nil, maxTimeout) when only timers exist.
func bestArm(def *core.WorkflowDefinition, st core.State, dist map[string]int, visited map[string]bool) (*core.WaitArm, time.Duration) {
	var best *core.WaitArm
	bestD := 1 << 30
	var maxTimeout time.Duration
	for i := range st.Arms {
		arm := &st.Arms[i]
		if arm.Timeout > 0 && arm.Timeout.D() > maxTimeout {
			maxTimeout = arm.Timeout.D()
		}
		if arm.Event == "" {
			continue
		}
		target := st.On[arm.Event]
		if d, ok := dist[target]; ok {
			if visited[target] {
				d += 1000
			}
			if def.States[target].Step == core.StepGate {
				d += 500 // prefer non-gate arms; see bestOutcome
			}
			if d < bestD {
				best, bestD = arm, d
			}
		}
	}
	return best, maxTimeout
}

// distanceToTerminal BFSes reverse edges from terminal states. Timeout
// edges are excluded — they are exceptional paths and counting them makes
// stall-gates look like shortcuts, trapping the walk in wait loops.
func distanceToTerminal(def *core.WorkflowDefinition) map[string]int {
	rev := map[string][]string{}
	dist := map[string]int{}
	var frontier []string
	for name, st := range def.States {
		if st.Terminal {
			dist[name] = 0
			frontier = append(frontier, name)
		}
		for outcome, target := range st.On {
			if outcome == "timeout" {
				continue
			}
			rev[target] = append(rev[target], name)
		}
	}
	sort.Strings(frontier)
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

var dryRefRe = regexp.MustCompile(`\$([a-zA-Z0-9_-]+)((?:\.[a-zA-Z0-9_-]+)+)`)

// referencedFields collects, per state, the $state.field paths the rest of
// the definition dereferences — the synthetic results must contain them.
func referencedFields(def *core.WorkflowDefinition) map[string][]string {
	out := map[string][]string{}
	add := func(tmpl string) {
		for _, m := range dryRefRe.FindAllStringSubmatch(tmpl, -1) {
			state, path := m[1], strings.TrimPrefix(m[2], ".")
			if state == "input" {
				continue
			}
			if _, ok := def.States[state]; !ok {
				continue
			}
			out[state] = append(out[state], path)
		}
	}
	for _, st := range def.States {
		add(st.Instruction)
		for _, v := range st.In {
			add(v)
		}
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

func printTrace(out io.Writer, st *store.Store, runID string) {
	events, _ := st.EventsForRun(runID)
	fmt.Fprintln(out, "trace:")
	for _, e := range events {
		fmt.Fprintf(out, "  %-22s %-16s %v\n", e.Kind, e.State, compact(e.Data))
	}
}

func compact(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(parts)
	s := strings.Join(parts, " ")
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}
