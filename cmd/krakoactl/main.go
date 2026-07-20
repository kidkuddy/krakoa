// krakoactl is the CLI: start runs, list runs/gates, answer gates, emit
// events, render timelines, validate and dry-run workspaces.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/kidkuddy/krakoa/internal/engine"
	"github.com/kidkuddy/krakoa/internal/workspace"
)

const usage = `usage: krakoactl <command> [args]
(flags may appear before or after positional arguments)

  run <workflow> --workspace <ws> [--input k=v]...   start a run
  runs [--status s1,s2] [--thread key]               list runs
  threads                                            list threads (runs grouped by work served)
  gates                                              list open gates
  answer <gate-id> <response> [--answers k=v]...     answer a gate
  bind <run-id> --slack-ts <ts>                      bind a Slack thread to a run's thread
  harvest <gate-id>                                  answer a question gate from its canvas
  why <run-id>                                       render a run's timeline
  emit <event> --workspace <ws> [--key k] [--run id] [--payload json]
  workspace validate <path>                          load + validate a workspace dir
  workspace dry-run <path> <workflow>                simulate a workflow end to end
  doctor                                             check live-run prerequisites

env: KRAKOA_ADDR (default http://127.0.0.1:7770)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "runs":
		err = cmdRuns(os.Args[2:])
	case "threads":
		err = cmdThreads()
	case "gates":
		err = cmdGates()
	case "answer":
		err = cmdAnswer(os.Args[2:])
	case "bind":
		err = cmdBind(os.Args[2:])
	case "harvest":
		err = cmdHarvest(os.Args[2:])
	case "why":
		err = cmdWhy(os.Args[2:])
	case "emit":
		err = cmdEmit(os.Args[2:])
	case "workspace":
		err = cmdWorkspace(os.Args[2:])
	case "doctor":
		err = cmdDoctor()
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "krakoactl: %v\n", err)
		os.Exit(1)
	}
}

func addr() string {
	if v := os.Getenv("KRAKOA_ADDR"); v != "" {
		return v
	}
	return "http://127.0.0.1:7770"
}

func call(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, addr()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("daemon unreachable at %s (is krakoad running?): %w", addr(), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// parseAnywhere parses a flag set allowing flags before AND after
// positionals (stdlib flag stops at the first non-flag; that cost three
// failed attempts in a live session). Returns the positionals in order.
func parseAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// kvFlags collects repeated k=v flags into a map.
type kvFlags map[string]any

func (k kvFlags) String() string { return "" }
func (k kvFlags) Set(s string) error {
	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("expected k=v, got %q", s)
	}
	k[parts[0]] = parts[1]
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	ws := fs.String("workspace", "", "workspace name")
	inputs := kvFlags{}
	fs.Var(inputs, "input", "input k=v (repeatable)")
	pos, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	// the workflow is required — a silent default once started the wrong thing
	if len(pos) != 1 || *ws == "" {
		return fmt.Errorf("usage: run <workflow> --workspace <ws> [--input k=v] (flags may go anywhere; exactly one workflow name)")
	}
	var run map[string]any
	if err := call("POST", "/v1/runs", map[string]any{
		"workspace": *ws, "workflow": pos[0], "inputs": map[string]any(inputs),
	}, &run); err != nil {
		return err
	}
	fmt.Printf("run %v started (state %v, status %v)\n", run["ID"], run["State"], run["Status"])
	return nil
}

func cmdRuns(args []string) error {
	fs := flag.NewFlagSet("runs", flag.ExitOnError)
	status := fs.String("status", "", "comma-separated status filter")
	thread := fs.String("thread", "", "filter by thread key")
	fs.Parse(args)
	path := "/v1/runs"
	if *thread != "" {
		path += "?thread=" + *thread
	} else if *status != "" {
		path += "?status=" + *status
	}
	var runs []map[string]any
	if err := call("GET", path, nil, &runs); err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("no runs")
		return nil
	}
	fmt.Printf("%-28s %-10s %-18s %-16s %-10s %s\n", "ID", "WORKSPACE", "WORKFLOW", "STATE", "STATUS", "THREAD")
	for _, r := range runs {
		fmt.Printf("%-28v %-10v %-18v %-16v %-10v %v\n", r["ID"], r["Workspace"], r["Workflow"], r["State"], r["Status"], orDash(r["Thread"]))
	}
	return nil
}

func cmdThreads() error {
	var threads []map[string]any
	if err := call("GET", "/v1/threads", nil, &threads); err != nil {
		return err
	}
	if len(threads) == 0 {
		fmt.Println("no threads (runs get a thread key once their definition's thread template resolves)")
		return nil
	}
	fmt.Printf("%-14s %-5s %-30s %-9s %s\n", "THREAD", "RUNS", "STATUSES", "COST", "LAST ACTIVITY")
	for _, t := range threads {
		cost := ""
		if c, ok := t["CostUSD"].(float64); ok && c > 0 {
			cost = fmt.Sprintf("$%.2f", c)
		}
		last, _ := t["LastSeen"].(string)
		if ts, err := time.Parse(time.RFC3339Nano, last); err == nil {
			last = ts.Local().Format("01-02 15:04")
		}
		fmt.Printf("%-14v %-5v %-30v %-9s %s\n", t["Thread"], t["Runs"], t["Statuses"], cost, last)
	}
	return nil
}

func cmdGates() error {
	var gates []map[string]any
	if err := call("GET", "/v1/gates", nil, &gates); err != nil {
		return err
	}
	if len(gates) == 0 {
		fmt.Println("no open gates")
		return nil
	}
	for _, g := range gates {
		fmt.Printf("%v  (%v, run %v, state %v)\n  %v\n", g["ID"], g["Kind"], g["RunID"], g["State"], g["Payload"])
		if opts, ok := g["Options"].([]any); ok && len(opts) > 0 {
			fmt.Printf("  options: %v\n", opts)
		}
		if del, ok := g["Delivery"].(map[string]any); ok {
			for ch, res := range del {
				if res != "ok" {
					fmt.Printf("  ⚠ %s delivery FAILED — seen only here (%v)\n", ch, res)
				}
			}
		}
	}
	return nil
}

func cmdAnswer(args []string) error {
	fs := flag.NewFlagSet("answer", flag.ExitOnError)
	answers := kvFlags{}
	fs.Var(answers, "answers", "answer k=v for question gates (repeatable)")
	pos, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fmt.Errorf("usage: answer <gate-id> <response> [--answers k=v]")
	}
	return call("POST", "/v1/gates/"+pos[0]+"/answer", map[string]any{
		"response": pos[1], "answers": map[string]any(answers), "responder": "cli",
	}, nil)
}

func cmdEmit(args []string) error {
	fs := flag.NewFlagSet("emit", flag.ExitOnError)
	ws := fs.String("workspace", os.Getenv("KRAKOA_WORKSPACE"), "workspace name")
	key := fs.String("key", "", "dedupe/correlation key")
	// NEVER default --run from KRAKOA_RUN: agents carry that env var, so a
	// step-internal emit would silently target its own run and strand the
	// signal there (live run 2: the sweeper's mr-ready never reached the
	// waiting lifecycle). Correlation-key routing is the default; targeting
	// a specific run is an explicit operator action.
	run := fs.String("run", "", "target run id (explicit only)")
	payload := fs.String("payload", "", "JSON payload")
	pos, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: emit <event> --workspace <ws> [--key k] [--run id] [--payload json]")
	}
	var p map[string]any
	if *payload != "" {
		if err := json.Unmarshal([]byte(*payload), &p); err != nil {
			return fmt.Errorf("bad --payload: %w", err)
		}
	}
	var out map[string]string
	if err := call("POST", "/v1/emit", map[string]any{
		"workspace": *ws, "event": pos[0], "key": *key, "run": *run, "payload": p,
	}, &out); err != nil {
		return err
	}
	fmt.Println(out["disposition"])
	return nil
}

func cmdWhy(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: why <run-id>")
	}
	var detail struct {
		Run    map[string]any   `json:"run"`
		Steps  []map[string]any `json:"steps"`
		Events []map[string]any `json:"events"`
	}
	if err := call("GET", "/v1/runs/"+args[0], nil, &detail); err != nil {
		return err
	}
	r := detail.Run
	fmt.Printf("run %v  %v/%v\n  state=%v status=%v def=%v ws-git=%v\n",
		r["ID"], r["Workspace"], r["Workflow"], r["State"], r["Status"], r["DefHash"], r["WSVersion"])

	if len(detail.Steps) > 0 {
		fmt.Println("\nsteps:")
		for _, s := range detail.Steps {
			line := fmt.Sprintf("  #%v %v attempt %v -> %v", s["ID"], s["State"], s["Attempt"], orDash(s["Outcome"]))
			if v, ok := s["Error"].(string); ok && v != "" {
				line += " ERROR " + v
			}
			fmt.Println(line)
			if v, ok := s["SessionID"].(string); ok && v != "" {
				fmt.Printf("     session %v", v)
				if p, ok := s["SessionPath"].(string); ok && p != "" {
					fmt.Printf("  (%v)", p)
				}
				fmt.Println()
			}
			if v, ok := s["HandoffDir"].(string); ok && v != "" {
				fmt.Printf("     handoff %v\n", v)
			}
			if c, ok := s["CostUSD"].(float64); ok && c > 0 {
				fmt.Printf("     cost $%.4f\n", c)
			}
		}
	}

	fmt.Println("\ntimeline:")
	for _, e := range detail.Events {
		data := ""
		if d, ok := e["Data"].(map[string]any); ok && len(d) > 0 {
			parts := make([]string, 0, len(d))
			for k, v := range d {
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			}
			sort.Strings(parts)
			data = strings.Join(parts, " ")
			if len(data) > 110 {
				data = data[:110] + "…"
			}
		}
		at, _ := e["At"].(string)
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			at = t.Local().Format("01-02 15:04:05")
		}
		fmt.Printf("  %s  %-22v %-16v %s\n", at, e["Kind"], e["State"], data)
	}
	return nil
}

func orDash(v any) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return "-"
}

func cmdWorkspace(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: workspace validate <path> | workspace dry-run <path> <workflow>")
	}
	switch args[0] {
	case "validate":
		ws, errs := workspace.Load(args[1])
		if len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "  - %v\n", e)
			}
			return fmt.Errorf("%d validation error(s)", len(errs))
		}
		fmt.Printf("workspace %s OK: %d workflows, %d agents, %d watchers, %d skills (git %s)\n",
			ws.Name, len(ws.Workflows), len(ws.Agents), len(ws.Watchers), len(ws.Skills), ws.GitVersion)
		return nil
	case "dry-run":
		if len(args) < 3 {
			return fmt.Errorf("usage: workspace dry-run <path> <workflow>")
		}
		ws, errs := workspace.Load(args[1])
		if len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "  - %v\n", e)
			}
			return fmt.Errorf("workspace invalid; fix before dry-run")
		}
		return engine.DryRun(ws, args[2], os.Stdout)
	default:
		return fmt.Errorf("unknown workspace subcommand %q", args[0])
	}
}

// cmdBind stores a contact ref (e.g. Slack thread_ts) on a run's thread —
// the agent-mode Slack session calls this right after starting a run.
func cmdBind(args []string) error {
	fs := flag.NewFlagSet("bind", flag.ExitOnError)
	ts := fs.String("slack-ts", "", "Slack thread_ts to bind")
	kind := fs.String("kind", "slack_ts", "ref kind")
	pos, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *ts == "" {
		return fmt.Errorf("usage: bind <run-id> --slack-ts <ts> [--kind slack_ts]")
	}
	return call("POST", "/v1/runs/"+pos[0]+"/bind", map[string]string{"kind": *kind, "value": *ts}, nil)
}

// cmdHarvest reads a question gate's canvas back and answers the gate from
// its ANSWER: blocks — deterministic parsing, no LLM. The engine never
// learns niffty; this is a client-side bridge.
func cmdHarvest(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: harvest <gate-id>")
	}
	gateID := args[0]
	var gate struct {
		RunID     string
		Kind      string
		Questions []string
	}
	if err := call("GET", "/v1/gates/"+gateID, nil, &gate); err != nil {
		return err
	}
	var ref struct{ Value string }
	if err := call("GET", "/v1/runs/"+gate.RunID+"/refs?kind=canvas:"+gateID, nil, &ref); err != nil {
		return err
	}
	if ref.Value == "" {
		return fmt.Errorf("no canvas bound to gate %s", gateID)
	}
	bin := os.Getenv("KRAKOA_NIFFTY_BIN")
	if bin == "" {
		bin = "niffty"
	}
	out, err := exec.Command(bin, "canvas", "read", ref.Value).Output()
	if err != nil {
		return fmt.Errorf("canvas read: %w", err)
	}
	answers := parseAnswers(string(out), gate.Questions)
	if len(answers) == 0 {
		return fmt.Errorf("no filled ANSWER: blocks found in the canvas")
	}
	if err := call("POST", "/v1/gates/"+gateID+"/answer", map[string]any{
		"response": "answered", "answers": answers, "responder": "canvas",
	}, nil); err != nil {
		return err
	}
	fmt.Printf("harvested %d answer(s) from the canvas -> gate %s\n", len(answers), gateID)
	return nil
}

// parseAnswers pairs the Nth ANSWER: block with the Nth question. A block
// runs from "ANSWER:" to the next heading or ANSWER line; empty blocks are
// skipped (unanswered questions stay unanswered).
func parseAnswers(md string, questions []string) map[string]any {
	answers := map[string]any{}
	lines := strings.Split(md, "\n")
	idx := -1
	var cur []string
	flush := func() {
		if idx < 0 {
			return
		}
		text := strings.TrimSpace(strings.Join(cur, "\n"))
		if text == "" {
			return
		}
		key := fmt.Sprintf("answer_%d", idx+1)
		if idx < len(questions) {
			key = questions[idx]
		}
		answers[key] = text
	}
	n := 0
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if after, ok := strings.CutPrefix(trimmed, "ANSWER:"); ok {
			flush()
			idx = n
			n++
			cur = []string{strings.TrimSpace(after)}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			flush()
			idx = -1
			continue
		}
		if idx >= 0 {
			cur = append(cur, l)
		}
	}
	flush()
	return answers
}
