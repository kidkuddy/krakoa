package engine

// Hermetic seam tests: the PLAN.md U1 (task-lifecycle) and U2 (review-sweeper)
// definitions run end-to-end against a scripted runner, fake clock, and
// SQLite :memory:. These double as the dry-run mechanism.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidkuddy/krakoa/internal/contact"
	"github.com/kidkuddy/krakoa/internal/core"
	"github.com/kidkuddy/krakoa/internal/runner"
	"github.com/kidkuddy/krakoa/internal/store"
	"github.com/kidkuddy/krakoa/internal/workspace"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeStep is one scripted runner response: Result written to result.json
// (nil = write nothing), or Err returned as a runner failure.
type fakeStep struct {
	Result map[string]any
	Err    error
}

type fakeRunner struct {
	mu     sync.Mutex
	script map[string][]fakeStep // key: request State
	calls  []runner.Request
}

func newFakeRunner() *fakeRunner { return &fakeRunner{script: map[string][]fakeStep{}} }

func (f *fakeRunner) on(state string, steps ...fakeStep) {
	f.mu.Lock()
	f.script[state] = append(f.script[state], steps...)
	f.mu.Unlock()
}

func ok(fields ...any) fakeStep { // ok("outcome", "grounded", "k", "v")
	m := map[string]any{}
	for i := 0; i+1 < len(fields); i += 2 {
		m[fields[i].(string)] = fields[i+1]
	}
	return fakeStep{Result: m}
}

func (f *fakeRunner) Run(_ context.Context, req runner.Request) (*runner.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	q := f.script[req.State]
	if len(q) == 0 {
		f.mu.Unlock()
		return nil, fmt.Errorf("fakeRunner: no script for state %q", req.State)
	}
	step := q[0]
	f.script[req.State] = q[1:]
	n := len(f.calls)
	f.mu.Unlock()

	if step.Err != nil {
		return nil, step.Err
	}
	if step.Result != nil {
		os.MkdirAll(req.HandoffDir, 0o755)
		raw, _ := json.Marshal(step.Result)
		if err := os.WriteFile(filepath.Join(req.HandoffDir, "result.json"), raw, 0o644); err != nil {
			return nil, err
		}
	}
	return &runner.Result{SessionID: fmt.Sprintf("sess-%d", n), CostUSD: 0.01}, nil
}

func (f *fakeRunner) lastCall() runner.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

type env struct {
	eng   *Engine
	run   *fakeRunner
	clock *fakeClock
	st    *store.Store
	out   *bytes.Buffer
}

func setup(t *testing.T) *env {
	t.Helper()
	ws, errs := workspace.Load("testdata/demo")
	if len(errs) != 0 {
		t.Fatalf("fixture workspace invalid: %v", errs)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fr := newFakeRunner()
	clk := &fakeClock{t: time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)}
	eng := New(st, fr, clk, map[string]*workspace.Workspace{"demo": ws}, t.TempDir())
	eng.Spawn = func(f func()) { f() } // deterministic: jobs run inline
	out := &bytes.Buffer{}
	eng.Channels = []contact.Channel{&contact.Console{W: out}}
	eng.Log.SetOutput(out)
	return &env{eng: eng, run: fr, clock: clk, st: st, out: out}
}

func (v *env) mustRun(t *testing.T, id string) *core.Run {
	t.Helper()
	r, err := v.st.GetRun(id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return r
}

func (v *env) openGate(t *testing.T, runID string) *core.Gate {
	t.Helper()
	g, err := v.st.OpenGateForRun(runID)
	if err != nil || g == nil {
		t.Fatalf("no open gate for %s (err=%v)", runID, err)
	}
	return g
}

// scriptHappyTail scripts filing -> done for U1.
func scriptHappyTail(fr *fakeRunner) {
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-9", "ticket_url", "https://x/CAL-9"))
	fr.on("dispatching", ok("outcome", "ok"))
	fr.on("merging", ok("outcome", "merged", "mr_url", "https://x/mr/7"))
	fr.on("notifying", ok("outcome", "ok"))
	fr.on("closing", ok("outcome", "ok"))
}

func TestU1HappyPathWithRefineLoop(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "draft-1"))
	fr.on("grounding", ok("outcome", "ungrounded", "questions", []any{"which button?"}))
	fr.on("refining", ok("outcome", "ok", "ticket", "draft-2"))
	fr.on("grounding", ok("outcome", "grounded"))
	scriptHappyTail(fr)

	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "fix the button"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// default input applied
	if v.mustRun(t, run.ID).Inputs["repo"] != "acme/demo-app" {
		t.Errorf("repo default missing: %v", run.Inputs)
	}
	// parked on the question gate
	g := v.openGate(t, run.ID)
	if g.Kind != core.GateQuestion || g.State != "asking" {
		t.Fatalf("gate = %+v", g)
	}
	if err := v.eng.AnswerGate(g.ID, "answered", map[string]any{"which button?": "the blue one"}, "test"); err != nil {
		t.Fatal(err)
	}
	// now waiting on the builder
	r := v.mustRun(t, run.ID)
	if r.State != "building" || r.Status != core.StatusWaiting {
		t.Fatalf("got %s/%s", r.State, r.Status)
	}
	// builder events flow in, correlated by ticket id
	v.eng.HandleEmit("demo", EmittedEvent{Event: "ticket-in-review", Key: "CAL-9"})
	r = v.mustRun(t, run.ID)
	if r.State != "awaiting-ready" {
		t.Fatalf("after in-review: %s", r.State)
	}
	v.eng.HandleEmit("demo", EmittedEvent{Event: "mr-ready", Key: "CAL-9"})
	r = v.mustRun(t, run.ID)
	if r.State != "done" || r.Status != core.StatusDone {
		t.Fatalf("final: %s/%s", r.State, r.Status)
	}
	// notifier prompt interpolated from context
	var notifyReq runner.Request
	for _, c := range fr.calls {
		if c.State == "notifying" {
			notifyReq = c
		}
	}
	if want := "CAL-9 merged: https://x/mr/7"; !bytes.Contains([]byte(notifyReq.Instruction), []byte(want)) {
		t.Errorf("notify instruction %q missing %q", notifyReq.Instruction, want)
	}
	// timeline exists with session ids
	steps, _ := v.st.StepsForRun(run.ID)
	if len(steps) < 7 {
		t.Errorf("want >=7 steps, got %d", len(steps))
	}
	for _, s := range steps {
		if s.SessionID == "" {
			t.Errorf("step %s missing session id", s.State)
		}
	}
}

func TestU1CorrelationIsolatesRuns(t *testing.T) {
	v := setup(t)
	for _, ticket := range []string{"CAL-1", "CAL-2"} {
		v.run.on("refining", ok("outcome", "ok", "ticket", "d"))
		v.run.on("grounding", ok("outcome", "grounded"))
		v.run.on("filing", ok("outcome", "ok", "ticket_id", ticket))
		v.run.on("dispatching", ok("outcome", "ok"))
	}
	r1, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	v.eng.HandleEmit("demo", EmittedEvent{Event: "ticket-in-review", Key: "CAL-2"})
	if got := v.mustRun(t, r1.ID); got.State != "building" {
		t.Errorf("r1 moved: %s", got.State)
	}
	if got := v.mustRun(t, r2.ID); got.State != "awaiting-ready" {
		t.Errorf("r2 did not move: %s", got.State)
	}
}

// The builder can finish while the run is mid-state (e.g. still inside
// retriggering after a stall): the event must buffer and be consumed the
// moment the run re-enters its wait state.
func TestU1EventBuffersWhileMidState(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "grounded"))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-4"))
	fr.on("dispatching", ok("outcome", "ok"))
	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "y"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// stall out and choose rerun -> retriggering (agent step)
	v.clock.Advance(8*time.Hour + time.Minute)
	v.eng.Tick()
	g := v.openGate(t, run.ID)

	// pause agent jobs so the run sits mid-state in retriggering
	var jobs []func()
	v.eng.Spawn = func(f func()) { jobs = append(jobs, f) }
	fr.on("retriggering", ok("outcome", "ok"))
	if err := v.eng.AnswerGate(g.ID, "rerun", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if got := v.mustRun(t, run.ID); got.State != "retriggering" || got.Status != core.StatusRunning {
		t.Fatalf("expected mid-state retriggering/running, got %s/%s", got.State, got.Status)
	}
	// the builder finishes RIGHT NOW — event must buffer, not drop
	if out := v.eng.HandleEmit("demo", EmittedEvent{Event: "ticket-in-review", Key: "CAL-4"}); out == "no matching run" {
		t.Fatalf("emit dropped: %s", out)
	}
	// let retriggering finish; building must consume the buffered event
	v.eng.Spawn = func(f func()) { f() }
	for len(jobs) > 0 {
		j := jobs[0]
		jobs = jobs[1:]
		j()
	}
	got := v.mustRun(t, run.ID)
	if got.State != "awaiting-ready" {
		t.Fatalf("buffered event not consumed on wait entry: %s/%s", got.State, got.Status)
	}
}

func TestU1TimeoutToStalledGate(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "grounded"))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-5"))
	fr.on("dispatching", ok("outcome", "ok"))
	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	v.clock.Advance(8*time.Hour + time.Minute)
	v.eng.Tick()
	r := v.mustRun(t, run.ID)
	if r.State != "stalled" || r.Status != core.StatusGated {
		t.Fatalf("got %s/%s", r.State, r.Status)
	}
	// choose to keep waiting -> back to building with a fresh timer
	g := v.openGate(t, run.ID)
	if err := v.eng.AnswerGate(g.ID, "wait", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if r = v.mustRun(t, run.ID); r.State != "building" || r.Status != core.StatusWaiting {
		t.Fatalf("after wait: %s/%s", r.State, r.Status)
	}
	v.clock.Advance(8*time.Hour + time.Minute)
	v.eng.Tick()
	g = v.openGate(t, run.ID)
	if err := v.eng.AnswerGate(g.ID, "abandon", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if r = v.mustRun(t, run.ID); r.State != "abandoned" || r.Status != core.StatusDone {
		t.Fatalf("after abandon: %s/%s", r.State, r.Status)
	}
}

func TestU1AskBudgetExhaustionParks(t *testing.T) {
	v := setup(t)
	fr := v.run
	for i := 0; i < 4; i++ {
		fr.on("refining", ok("outcome", "ok", "ticket", "d"))
		fr.on("grounding", ok("outcome", "ungrounded", "questions", []any{"?"}))
	}
	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		g := v.openGate(t, run.ID)
		if g.Kind != core.GateQuestion {
			t.Fatalf("loop %d: expected question gate, got %+v", i, g)
		}
		if err := v.eng.AnswerGate(g.ID, "answered", map[string]any{"?": "a"}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	// 4th answer would exceed budget 3
	g := v.openGate(t, run.ID)
	if err := v.eng.AnswerGate(g.ID, "answered", map[string]any{"?": "a"}, "test"); err != nil {
		t.Fatal(err)
	}
	r := v.mustRun(t, run.ID)
	if r.Status != core.StatusNeedsAttention {
		t.Fatalf("expected needs-attention, got %s/%s", r.State, r.Status)
	}
	att := v.openGate(t, run.ID)
	if err := v.eng.AnswerGate(att.ID, "abandon", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if r = v.mustRun(t, run.ID); r.Status != core.StatusFailed {
		t.Fatalf("after abandon: %s", r.Status)
	}
}

func TestSchemaViolationResumesOnceThenParks(t *testing.T) {
	v := setup(t)
	fr := v.run
	// first attempt writes garbage, resume writes a valid result
	fr.on("refining", fakeStep{Result: map[string]any{"nope": true}})
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "grounded"))
	scriptHappyTail(fr)
	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// second runner call must be a resume of the first session
	if len(fr.calls) < 2 || fr.calls[1].Resume != "sess-1" {
		t.Fatalf("expected resume of sess-1, calls: %d %+v", len(fr.calls), fr.calls[1].Resume)
	}
	if r := v.mustRun(t, run.ID); r.State != "building" {
		t.Fatalf("run should have proceeded, got %s", r.State)
	}

	// and a double violation parks
	fr.on("refining", fakeStep{Result: map[string]any{"nope": true}})
	fr.on("refining", fakeStep{Result: map[string]any{"still": "wrong"}})
	run2, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "y"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, run2.ID); r.Status != core.StatusNeedsAttention {
		t.Fatalf("expected park, got %s", r.Status)
	}
}

func TestRunnerFailureParksAndRetryRecovers(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", fakeStep{Err: fmt.Errorf("api meltdown")})
	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	r := v.mustRun(t, run.ID)
	if r.Status != core.StatusNeedsAttention {
		t.Fatalf("got %s", r.Status)
	}
	// human says retry; this time it works
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "grounded"))
	scriptHappyTail(fr)
	g := v.openGate(t, run.ID)
	if err := v.eng.AnswerGate(g.ID, "retry", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if r = v.mustRun(t, run.ID); r.State != "building" || r.Status != core.StatusWaiting {
		t.Fatalf("after retry: %s/%s", r.State, r.Status)
	}
}

func TestU2WatcherSpawnsDedupesAndReviews(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("reviewing", ok("outcome", "pass"))
	fr.on("marking-ready", ok("outcome", "ok"))

	obs := []EmittedEvent{{
		Event: "mr-draft-pending", Key: "dash!7!sha1",
		Payload: map[string]any{"repo": "dash", "mr_iid": "7", "head_sha": "sha1"},
	}}
	v.eng.HandleWatcherEvents("demo", "draft-mr-watch", obs)
	runs, _ := v.st.ListRuns()
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].Status != core.StatusDone {
		t.Fatalf("sweep run: %s/%s", runs[0].State, runs[0].Status)
	}

	// same key again: dedupe, no new run
	v.eng.HandleWatcherEvents("demo", "draft-mr-watch", obs)
	if runs, _ = v.st.ListRuns(); len(runs) != 1 {
		t.Fatalf("dedupe failed: %d runs", len(runs))
	}

	// new head SHA: fresh review, this time failing -> requesting-changes
	fr.on("reviewing", ok("outcome", "fail", "findings", "Blockers: x"))
	fr.on("requesting-changes", ok("outcome", "ok"))
	obs2 := []EmittedEvent{{
		Event: "mr-draft-pending", Key: "dash!7!sha2",
		Payload: map[string]any{"repo": "dash", "mr_iid": "7", "head_sha": "sha2"},
	}}
	v.eng.HandleWatcherEvents("demo", "draft-mr-watch", obs2)
	if runs, _ = v.st.ListRuns(); len(runs) != 2 {
		t.Fatalf("new head should spawn: %d runs", len(runs))
	}
	// gatekeeper reality check: the reviewing step ran as the reviewer agent
	for _, c := range fr.calls {
		if c.State == "reviewing" && c.Spec.Name != "reviewer" {
			t.Errorf("reviewing ran as %q", c.Spec.Name)
		}
	}
}

func TestConcurrencyFIFO(t *testing.T) {
	v := setup(t)
	fr := v.run
	// three sweeps; concurrency 2. Script only 2 reviews; the third runs
	// after a slot frees.
	fr.on("reviewing", ok("outcome", "noop"))
	fr.on("reviewing", ok("outcome", "noop"))
	var events []EmittedEvent
	for i := 1; i <= 3; i++ {
		events = append(events, EmittedEvent{
			Event: "mr-draft-pending", Key: fmt.Sprintf("dash!%d!s", i),
			Payload: map[string]any{"repo": "dash", "mr_iid": fmt.Sprintf("%d", i), "head_sha": "s"},
		})
	}
	// pause jobs so all three land before any review completes
	var jobs []func()
	v.eng.Spawn = func(f func()) { jobs = append(jobs, f) }
	v.eng.HandleWatcherEvents("demo", "draft-mr-watch", events)
	runs, _ := v.st.ListRuns(core.StatusQueued)
	if len(runs) != 1 {
		t.Fatalf("want 1 queued run, got %d", len(runs))
	}
	queuedID := runs[0].ID
	fr.on("reviewing", ok("outcome", "noop"))
	v.eng.Spawn = func(f func()) { f() }
	for len(jobs) > 0 {
		j := jobs[0]
		jobs = jobs[1:]
		j()
	}
	if r := v.mustRun(t, queuedID); r.Status != core.StatusDone {
		t.Fatalf("queued run not admitted+finished: %s/%s", r.State, r.Status)
	}
}

func TestRestartRecovery(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "grounded"))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-7"))
	fr.on("dispatching", ok("outcome", "ok"))
	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, run.ID); r.State != "building" || r.Status != core.StatusWaiting {
		t.Fatalf("precondition: %s/%s", r.State, r.Status)
	}

	// "restart": a brand-new engine over the same store
	ws, _ := workspace.Load("testdata/demo")
	fr2 := newFakeRunner()
	eng2 := New(v.st, fr2, v.clock, map[string]*workspace.Workspace{"demo": ws}, t.TempDir())
	eng2.Spawn = func(f func()) { f() }
	eng2.Log.SetOutput(v.out)
	eng2.Recover()

	// waiting run still advances on its event
	fr2.on("merging", ok("outcome", "merged", "mr_url", "u"))
	fr2.on("notifying", ok("outcome", "ok"))
	fr2.on("closing", ok("outcome", "ok"))
	eng2.HandleEmit("demo", EmittedEvent{Event: "ticket-in-review", Key: "CAL-7"})
	eng2.HandleEmit("demo", EmittedEvent{Event: "mr-ready", Key: "CAL-7"})
	if r := v.mustRun(t, run.ID); r.Status != core.StatusDone {
		t.Fatalf("after recovery: %s/%s", r.State, r.Status)
	}
}

func TestRestartMidAgentStepReattempts(t *testing.T) {
	v := setup(t)
	// crash simulation: a run sits in status=running with no live process
	now := v.clock.Now()
	run := &core.Run{
		ID: "task-lifecycle-dead", Workspace: "demo", Workflow: "task-lifecycle",
		DefHash: "h", State: "refining", Status: core.StatusRunning,
		Inputs: map[string]any{"idea": "x", "repo": "r"}, Context: map[string]any{}, EdgeCounts: map[string]int{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := v.st.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	fr2 := newFakeRunner()
	ws, _ := workspace.Load("testdata/demo")
	eng2 := New(v.st, fr2, v.clock, map[string]*workspace.Workspace{"demo": ws}, t.TempDir())
	eng2.Spawn = func(f func()) { f() }
	eng2.Log.SetOutput(v.out)
	fr2.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr2.on("grounding", ok("outcome", "grounded"))
	fr2.on("filing", ok("outcome", "ok", "ticket_id", "CAL-8"))
	fr2.on("dispatching", ok("outcome", "ok"))
	eng2.Recover()
	if r := v.mustRun(t, run.ID); r.State != "building" || r.Status != core.StatusWaiting {
		t.Fatalf("after recovery: %s/%s", r.State, r.Status)
	}
}

func TestRestartRearmsTimers(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "grounded"))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-10"))
	fr.on("dispatching", ok("outcome", "ok"))
	run, _ := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")

	ws, _ := workspace.Load("testdata/demo")
	eng2 := New(v.st, newFakeRunner(), v.clock, map[string]*workspace.Workspace{"demo": ws}, t.TempDir())
	eng2.Spawn = func(f func()) { f() }
	eng2.Log.SetOutput(v.out)
	eng2.Recover()
	v.clock.Advance(9 * time.Hour)
	eng2.Tick()
	if r := v.mustRun(t, run.ID); r.State != "stalled" || r.Status != core.StatusGated {
		t.Fatalf("timer not re-armed: %s/%s", r.State, r.Status)
	}
}

func TestWatcherTimerFiresProbeAgent(t *testing.T) {
	v := setup(t)
	fr := v.run
	// watcher probe emits one observation via its handoff result.json
	fr.on("watcher:draft-mr-watch", fakeStep{Result: map[string]any{
		"outcome": "ok",
		"events": []any{map[string]any{
			"event": "mr-draft-pending", "key": "dash!9!s",
			"payload": map[string]any{"repo": "dash", "mr_iid": "9", "head_sha": "s"},
		}},
	}})
	fr.on("reviewing", ok("outcome", "noop"))
	v.eng.Recover() // arms the watcher timer at now
	v.eng.Tick()    // fires it
	runs, _ := v.st.ListRuns()
	if len(runs) != 1 || runs[0].Status != core.StatusDone {
		t.Fatalf("watcher did not spawn+complete a run: %+v", runs)
	}
	// cadence: timer rescheduled, not gone
	timers, _ := v.st.ActiveTimers()
	found := false
	for _, tm := range timers {
		if tm.Kind == "watcher" {
			found = true
			if got := tm.FireAt; !got.After(v.clock.Now()) {
				t.Errorf("watcher timer not rescheduled: %v", got)
			}
		}
	}
	if !found {
		t.Error("watcher timer missing after fire")
	}
}

func TestEmitSpawnsWatcherModeRun(t *testing.T) {
	v := setup(t)
	v.run.on("reviewing", ok("outcome", "noop"))
	out := v.eng.HandleEmit("demo", EmittedEvent{
		Event: "mr-draft-pending", Key: "dash!42!s1",
		Payload: map[string]any{"repo": "dash", "mr_iid": "42", "head_sha": "s1"},
	})
	if len(out) < 7 || out[:7] != "spawned" {
		t.Fatalf("manual emit should spawn: %q", out)
	}
	runs, _ := v.st.ListRuns()
	if len(runs) != 1 || runs[0].Status != core.StatusDone {
		t.Fatalf("runs = %+v", runs)
	}
	// replaying the same key is a no-op (dedupe shared with the watcher)
	out = v.eng.HandleEmit("demo", EmittedEvent{Event: "mr-draft-pending", Key: "dash!42!s1"})
	if out != "duplicate (already handled by watcher draft-mr-watch)" {
		t.Fatalf("replay disposition = %q", out)
	}
	if runs, _ = v.st.ListRuns(); len(runs) != 1 {
		t.Fatalf("dedupe failed on manual emit")
	}
}

func TestWatcherDerailmentRetriesThenGates(t *testing.T) {
	v := setup(t)
	// every sweep attempt writes NO result.json (a derailed agent);
	// 3 sweeps x (attempt + retry) = 6 scripted failures -> gate
	for i := 0; i < 6; i++ {
		v.run.on("watcher:draft-mr-watch", fakeStep{Result: nil})
	}
	v.eng.Recover() // arms the watcher timer at now
	for i := 0; i < 3; i++ {
		v.eng.Tick()
		v.clock.Advance(10 * time.Minute)
	}
	if len(v.run.calls) != 6 {
		t.Fatalf("want 6 attempts (retry once per sweep), got %d", len(v.run.calls))
	}
	gates, _ := v.st.OpenGates()
	if len(gates) != 1 || gates[0].RunID != "" {
		t.Fatalf("want one engine-level gate, got %+v", gates)
	}
	// the gate is answerable even with no run behind it
	if err := v.eng.AnswerGate(gates[0].ID, "acknowledged", nil, "test"); err != nil {
		t.Fatal(err)
	}
	// and a later healthy sweep resets the world
	v.run.on("watcher:draft-mr-watch", fakeStep{Result: map[string]any{"outcome": "ok", "events": []any{}}})
	v.eng.Tick()
	if gates, _ = v.st.OpenGates(); len(gates) != 0 {
		t.Fatalf("gates should stay closed after healthy sweep: %+v", gates)
	}
}

func TestWatcherPromptIsImperativeAndIsolated(t *testing.T) {
	v := setup(t)
	v.run.on("watcher:draft-mr-watch", fakeStep{Result: map[string]any{"outcome": "ok", "events": []any{}}})
	v.eng.Recover()
	v.eng.Tick()
	req := v.run.lastCall()
	if !bytes.Contains([]byte(req.Instruction), []byte("Do not ask questions")) {
		t.Errorf("watcher prompt not imperative: %.120s", req.Instruction)
	}
}

func setupCmd(t *testing.T) *env {
	t.Helper()
	ws, errs := workspace.Load("testdata/cmddemo")
	if len(errs) != 0 {
		t.Fatalf("cmddemo invalid: %v", errs)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fr := newFakeRunner()
	clk := &fakeClock{t: time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)}
	eng := New(st, fr, clk, map[string]*workspace.Workspace{"cmddemo": ws}, t.TempDir())
	eng.Spawn = func(f func()) { f() }
	out := &bytes.Buffer{}
	eng.Channels = []contact.Channel{&contact.Console{W: out}}
	eng.Log.SetOutput(out)
	return &env{eng: eng, run: fr, clock: clk, st: st, out: out}
}

func TestCommandWatcherSweepsAndCommandProbeAdvances(t *testing.T) {
	v := setupCmd(t)
	v.eng.Recover() // arms both command watchers
	v.eng.Tick()    // cmd-watch spawns a run; bad-watch strikes once

	runs, _ := v.st.ListRuns()
	if len(runs) != 1 {
		t.Fatalf("command watcher should spawn 1 run, got %d", len(runs))
	}
	r := runs[0]
	if r.State != "waiting" || r.Status != core.StatusWaiting {
		t.Fatalf("run = %s/%s", r.State, r.Status)
	}
	// zero LLM calls so far: both watcher and (pending) probe are commands
	if len(v.run.calls) != 0 {
		t.Fatalf("no runner calls expected, got %d", len(v.run.calls))
	}

	// the probe command fires on its cadence and advances the run
	v.clock.Advance(61 * time.Second)
	v.eng.Tick()
	if got, _ := v.st.GetRun(r.ID); got.Status != core.StatusDone {
		t.Fatalf("after probe: %s/%s", got.State, got.Status)
	}
	// probe interpolated its $input.key argument and recorded $0 cost
	evs, _ := v.st.EventsForRun(r.ID)
	found := false
	for _, e := range evs {
		if e.Kind == "probe-outcome" {
			found = true
			if c, ok := e.Data["command"].(string); !ok || !strings.Contains(c, "check.sh K") {
				t.Errorf("command not interpolated: %v", e.Data)
			}
			if e.Data["cost_usd"] != float64(0) {
				t.Errorf("command probe cost should be 0: %v", e.Data)
			}
		}
	}
	if !found {
		t.Error("no probe-outcome event")
	}

	// bad-watch: two more ticks -> 3 consecutive failures -> engine gate
	v.clock.Advance(5 * time.Minute)
	v.eng.Tick()
	v.clock.Advance(5 * time.Minute)
	v.eng.Tick()
	gates, _ := v.st.OpenGates()
	if len(gates) != 1 || !strings.Contains(gates[0].Payload, "bad-watch") {
		t.Fatalf("expected bad-watch strike gate, got %+v", gates)
	}
}

// Answers accumulate across gate rounds: round 3 must still carry round 1's
// decisions, or the verifier re-asks settled questions.
func TestQuestionGateAnswersAccumulate(t *testing.T) {
	v := setup(t)
	fr := v.run
	for i := 0; i < 3; i++ {
		fr.on("refining", ok("outcome", "ok", "ticket", "d"))
		fr.on("grounding", ok("outcome", "ungrounded", "questions", []any{"q"}))
	}
	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	g := v.openGate(t, run.ID)
	if err := v.eng.AnswerGate(g.ID, "answered", map[string]any{"direction": "inbound only"}, "test"); err != nil {
		t.Fatal(err)
	}
	g = v.openGate(t, run.ID)
	if err := v.eng.AnswerGate(g.ID, "answered", map[string]any{"revision": "pin latest"}, "test"); err != nil {
		t.Fatal(err)
	}
	r := v.mustRun(t, run.ID)
	asking, _ := r.Context["asking"].(map[string]any)
	answers, _ := asking["answers"].(map[string]any)
	if answers["direction"] != "inbound only" || answers["revision"] != "pin latest" {
		t.Fatalf("answers did not accumulate: %+v", answers)
	}
}

// Correlation events are NOT spawn-deduped: a ticket goes ready on every
// review round, and each recurrence must route (found live: one consumed
// dedupe mark swallowed all later mr-ready events for the ticket).
func TestCorrelationEventsRecurAcrossReviewRounds(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "grounded"))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-9"))
	fr.on("dispatching", ok("outcome", "ok"))
	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}

	sweep := []EmittedEvent{
		{Event: "ticket-in-review", Key: "CAL-9", Payload: map[string]any{"ticket_id": "CAL-9"}},
		{Event: "mr-ready", Key: "CAL-9", Payload: map[string]any{"ticket_id": "CAL-9"}},
	}
	// round 1: review found problems -> conflict -> retrigger -> building
	fr.on("merging", ok("outcome", "conflict"))
	fr.on("retriggering", ok("outcome", "ok"))
	v.eng.HandleWatcherEvents("demo", "draft-mr-watch", sweep)
	if r := v.mustRun(t, run.ID); r.State != "building" {
		t.Fatalf("after round 1: %s/%s", r.State, r.Status)
	}
	// round 2: the SAME keys recur — must route again, not dedupe-swallow
	fr.on("merging", ok("outcome", "merged", "mr_url", "u"))
	fr.on("notifying", ok("outcome", "ok"))
	fr.on("closing", ok("outcome", "ok"))
	v.eng.HandleWatcherEvents("demo", "draft-mr-watch", sweep)
	if r := v.mustRun(t, run.ID); r.Status != core.StatusDone {
		t.Fatalf("round-2 mr-ready swallowed: %s/%s", r.State, r.Status)
	}
}

// Re-observed world state buffers at most one pending copy per (run, event).
func TestBufferedSignalsDeduplicate(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "grounded"))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-9"))
	var jobs []func()
	v.eng.Spawn = func(f func()) { jobs = append(jobs, f) }
	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// run the pipeline up to dispatching, then hold it mid-state there
	for len(jobs) > 3 {
		t.Fatal("unexpected")
	}
	for i := 0; i < 3; i++ {
		j := jobs[0]
		jobs = jobs[1:]
		j()
	}
	if r := v.mustRun(t, run.ID); r.State != "dispatching" || r.Status != core.StatusRunning {
		t.Fatalf("precondition: %s/%s", r.State, r.Status)
	}
	// three sweeps observe the same unchanged world
	for i := 0; i < 3; i++ {
		v.eng.HandleWatcherEvents("demo", "draft-mr-watch", []EmittedEvent{
			{Event: "ticket-in-review", Key: "CAL-9", Payload: map[string]any{"ticket_id": "CAL-9"}},
		})
	}
	sigs, _ := v.st.PendingSignals(run.ID)
	if len(sigs) != 1 {
		t.Fatalf("want exactly 1 pending signal, got %d", len(sigs))
	}
	// finish dispatching; building consumes the single buffered signal
	fr.on("dispatching", ok("outcome", "ok"))
	v.eng.Spawn = func(f func()) { f() }
	j := jobs[0]
	j()
	if r := v.mustRun(t, run.ID); r.State != "awaiting-ready" {
		t.Fatalf("buffered signal not consumed: %s/%s", r.State, r.Status)
	}
}

// Threads: the lifecycle stamps its thread once filing resolves; a watcher
// spawn carrying the same ticket joins the same thread; aggregation sees one
// piece of work.
func TestThreadStampingAndPropagation(t *testing.T) {
	v := setup(t)
	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "grounded"))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-9"))
	fr.on("dispatching", ok("outcome", "ok"))
	life, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := v.mustRun(t, life.ID); r.Thread != "CAL-9" {
		t.Fatalf("lifecycle thread = %q (want CAL-9)", r.Thread)
	}

	fr.on("reviewing", ok("outcome", "noop"))
	v.eng.HandleWatcherEvents("demo", "draft-mr-watch", []EmittedEvent{{
		Event: "mr-draft-pending", Key: "dash!7!s1",
		Payload: map[string]any{"repo": "dash", "mr_iid": "7", "head_sha": "s1", "ticket_id": "CAL-9"},
	}})
	runs, _ := v.st.RunsByThread("CAL-9")
	if len(runs) != 2 {
		t.Fatalf("thread CAL-9 should hold 2 runs, got %d", len(runs))
	}

	threads, err := v.st.Threads()
	if err != nil || len(threads) != 1 {
		t.Fatalf("threads = %+v, %v", threads, err)
	}
	if threads[0].Thread != "CAL-9" || threads[0].Runs != 2 || threads[0].CostUSD <= 0 {
		t.Fatalf("summary = %+v", threads[0])
	}
}

// The refiner's working folder follows the repo input through the
// workspace repos: map — never a hardcoded clone.
func TestWorkingFolderFollowsRepoInput(t *testing.T) {
	v := setup(t)
	v.run.on("refining", ok("outcome", "ok", "ticket", "d"))
	v.run.on("grounding", ok("outcome", "ungrounded", "questions", []any{"q"}))
	if _, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "x"}, ""); err != nil {
		t.Fatal(err)
	}
	var refine runner.Request
	for _, c := range v.run.calls {
		if c.State == "refining" {
			refine = c
		}
	}
	if refine.Spec.WorkingFolder != "/tmp/demo-app-clone" {
		t.Fatalf("working folder = %q (want repos-mapped path)", refine.Spec.WorkingFolder)
	}
}

// Round 4: question gates carry structured questions; the board projector
// tracks thread phase; slack refs bind pre-stamp and migrate on stamp.
func TestQuestionsBoardAndBindings(t *testing.T) {
	v := setup(t)
	var board [][3]string
	v.eng.Board = func(thread, title, lane string) { board = append(board, [3]string{thread, title, lane}) }

	fr := v.run
	fr.on("refining", ok("outcome", "ok", "ticket", "d"))
	fr.on("grounding", ok("outcome", "ungrounded", "questions", []any{"which direction?", "pin it?"}))
	run, err := v.eng.StartRun("demo", "task-lifecycle", map[string]any{"idea": "wizard task"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// bind a slack thread while the run id is still the effective thread
	if err := v.eng.Bind(run.ID, "slack_ts", "1700000.123"); err != nil {
		t.Fatal(err)
	}
	g := v.openGate(t, run.ID)
	if len(g.Questions) != 2 || g.Questions[0] != "which direction?" {
		t.Fatalf("structured questions missing: %+v", g.Questions)
	}
	if len(board) == 0 || board[len(board)-1][2] != "needs-answers" {
		t.Fatalf("board should show needs-answers, got %v", board)
	}

	// answer; proceed to filing (thread stamps) — refs must migrate
	fr.on("refining", ok("outcome", "ok", "ticket", "d2"))
	fr.on("grounding", ok("outcome", "grounded"))
	fr.on("filing", ok("outcome", "ok", "ticket_id", "CAL-77"))
	fr.on("dispatching", ok("outcome", "ok"))
	if err := v.eng.AnswerGate(g.ID, "answered", map[string]any{"which direction?": "inbound"}, "canvas"); err != nil {
		t.Fatal(err)
	}
	if got := v.eng.ThreadRefForRun(run.ID, "slack_ts"); got != "1700000.123" {
		t.Fatalf("slack_ts lost after thread stamp: %q", got)
	}
	if ts, _ := v.st.ThreadRef("CAL-77", "slack_ts"); ts != "1700000.123" {
		t.Fatalf("ref not migrated to thread key: %q", ts)
	}
	if board[len(board)-1][2] != "in-progress" {
		t.Fatalf("board should show in-progress while waiting, got %v", board[len(board)-1])
	}
}
