package core

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// testDef is a miniature of U1's shape: agent loop with budget, question
// gate, wait with event+timeout arms, terminal states.
func testDef() *WorkflowDefinition {
	return &WorkflowDefinition{
		Name:    "mini",
		Trigger: Trigger{Kind: TriggerManual},
		Start:   "refining",
		States: map[string]State{
			"refining": {
				Step: StepAgent, Agent: "refiner", Instruction: "refine $input.idea",
				In: map[string]string{"idea": "$input.idea", "answers": "$asking"},
				On: map[string]string{"ok": "grounding"},
			},
			"grounding": {
				Step: StepAgent, Agent: "grounder",
				On: map[string]string{"grounded": "building", "ungrounded": "asking"},
			},
			"asking": {
				Step: StepGate, Gate: &GateSpec{Kind: GateQuestion, Payload: "questions: $grounding.questions"},
				On:      map[string]string{"answered": "refining"},
				Budgets: map[string]int{"answered": 2},
			},
			"building": {
				Step: StepWait,
				Arms: []WaitArm{
					{Event: "ticket-in-review"},
					{Timeout: Duration(8 * time.Hour)},
				},
				On: map[string]string{"ticket-in-review": "done", "timeout": "stalled"},
			},
			"stalled": {
				Step: StepGate, Gate: &GateSpec{Kind: GateChoice, Payload: "stuck", Options: []string{"rerun", "abandon"}},
				On: map[string]string{"rerun": "building", "abandon": "abandoned"},
			},
			"done":      {Terminal: true},
			"abandoned": {Terminal: true},
		},
	}
}

func newRun(state string) Run {
	return Run{
		ID: "r1", Workflow: "mini", State: state, Status: StatusRunning,
		Inputs:  map[string]any{"idea": "fix the button"},
		Context: map[string]any{},
	}
}

func TestEnter(t *testing.T) {
	def := testDef()
	cases := []struct {
		name       string
		state      string
		ctx        map[string]any
		wantStatus RunStatus
		wantAction any // reflect.TypeOf comparison
		check      func(t *testing.T, d Decision)
	}{
		{
			name: "agent state resolves inputs and instruction", state: "refining",
			ctx:        map[string]any{"asking": map[string]any{"a1": "yes"}},
			wantStatus: StatusRunning, wantAction: ActionRunAgent{},
			check: func(t *testing.T, d Decision) {
				a := d.Actions[0].(ActionRunAgent)
				if a.Agent != "refiner" || a.Instruction != "refine fix the button" {
					t.Errorf("bad action: %+v", a)
				}
				if a.Inputs["idea"] != "fix the button" {
					t.Errorf("input not resolved: %+v", a.Inputs)
				}
			},
		},
		{
			name: "gate state interpolates payload", state: "asking",
			ctx:        map[string]any{"grounding": map[string]any{"questions": []any{"q1"}}},
			wantStatus: StatusGated, wantAction: ActionOpenGate{},
			check: func(t *testing.T, d Decision) {
				g := d.Actions[0].(ActionOpenGate)
				if g.Kind != GateQuestion || g.Payload != "questions: [q1]" {
					t.Errorf("bad gate: %+v", g)
				}
			},
		},
		{
			name: "wait state arms", state: "building",
			wantStatus: StatusWaiting, wantAction: ActionArmWait{},
			check: func(t *testing.T, d Decision) {
				w := d.Actions[0].(ActionArmWait)
				if len(w.Arms) != 2 {
					t.Errorf("want 2 arms, got %+v", w.Arms)
				}
			},
		},
		{
			name: "terminal state finishes", state: "done",
			wantStatus: StatusDone, wantAction: ActionFinish{},
		},
		{
			name: "agent state with unresolvable input parks", state: "refining",
			ctx:        map[string]any{}, // $asking missing
			wantStatus: StatusNeedsAttention, wantAction: ActionPark{},
		},
		{
			name: "unknown state parks", state: "nope",
			wantStatus: StatusNeedsAttention, wantAction: ActionPark{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := newRun(tc.state)
			if tc.ctx != nil {
				run.Context = tc.ctx
			}
			d := Enter(def, run)
			if d.Run.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", d.Run.Status, tc.wantStatus)
			}
			if len(d.Actions) != 1 || reflect.TypeOf(d.Actions[0]) != reflect.TypeOf(tc.wantAction) {
				t.Fatalf("actions = %+v, want one %T", d.Actions, tc.wantAction)
			}
			if tc.check != nil {
				tc.check(t, d)
			}
		})
	}
}

func TestAdvance(t *testing.T) {
	def := testDef()

	t.Run("outcome transitions and saves result", func(t *testing.T) {
		run := newRun("refining")
		d := Advance(def, run, "ok", map[string]any{"ticket": "body"})
		if d.Run.State != "grounding" || d.Run.Status != StatusRunning {
			t.Fatalf("got %s/%s", d.Run.State, d.Run.Status)
		}
		saved, _ := d.Run.Context["refining"].(map[string]any)
		if saved["ticket"] != "body" {
			t.Fatalf("result not saved: %+v", d.Run.Context)
		}
	})

	t.Run("unknown outcome parks", func(t *testing.T) {
		d := Advance(def, newRun("refining"), "wat", nil)
		if d.Run.Status != StatusNeedsAttention {
			t.Fatalf("status = %s", d.Run.Status)
		}
	})

	t.Run("budget allows up to N then parks", func(t *testing.T) {
		run := newRun("asking")
		run.Context["grounding"] = map[string]any{"questions": []any{"q"}}
		for i := 1; i <= 2; i++ {
			d := Advance(def, run, "answered", map[string]any{"a1": "yes"})
			if d.Run.State != "refining" || d.Run.Status != StatusRunning {
				t.Fatalf("traversal %d: %s/%s", i, d.Run.State, d.Run.Status)
			}
			run = d.Run
			run.State = "asking" // loop back for the next traversal
		}
		d := Advance(def, run, "answered", nil)
		if d.Run.Status != StatusNeedsAttention {
			t.Fatalf("3rd traversal should park, got %s/%s", d.Run.State, d.Run.Status)
		}
	})

	t.Run("wait event arm advances to terminal", func(t *testing.T) {
		run := newRun("building")
		run.Status = StatusWaiting
		d := Advance(def, run, "ticket-in-review", nil)
		if d.Run.State != "done" || d.Run.Status != StatusDone {
			t.Fatalf("got %s/%s", d.Run.State, d.Run.Status)
		}
	})

	t.Run("wait timeout arm routes to gate", func(t *testing.T) {
		d := Advance(def, newRun("building"), "timeout", nil)
		if d.Run.State != "stalled" || d.Run.Status != StatusGated {
			t.Fatalf("got %s/%s", d.Run.State, d.Run.Status)
		}
	})

	t.Run("choice gate outcome is the option", func(t *testing.T) {
		d := Advance(def, newRun("stalled"), "abandon", nil)
		if d.Run.State != "abandoned" || d.Run.Status != StatusDone {
			t.Fatalf("got %s/%s", d.Run.State, d.Run.Status)
		}
	})
}

func TestResolveAttention(t *testing.T) {
	def := testDef()

	t.Run("retry re-enters current state", func(t *testing.T) {
		run := newRun("grounding")
		run.Status = StatusNeedsAttention
		d := ResolveAttention(def, run, "retry")
		if d.Run.Status != StatusRunning || len(d.Actions) != 1 {
			t.Fatalf("got %s %+v", d.Run.Status, d.Actions)
		}
	})

	t.Run("abandon fails the run", func(t *testing.T) {
		run := newRun("grounding")
		run.Status = StatusNeedsAttention
		d := ResolveAttention(def, run, "abandon")
		if d.Run.Status != StatusFailed {
			t.Fatalf("got %s", d.Run.Status)
		}
	})
}

func TestResolve(t *testing.T) {
	run := newRun("x")
	run.Context["grounding"] = map[string]any{"verdict": "grounded", "nested": map[string]any{"k": "v"}}
	cases := []struct {
		expr    string
		want    any
		wantErr bool
	}{
		{"literal", "literal", false},
		{"$input.idea", "fix the button", false},
		{"$grounding.verdict", "grounded", false},
		{"$grounding.nested.k", "v", false},
		{"$grounding.missing", nil, true},
		{"$grounding.missing?", nil, false},
		{"$asking?", nil, false},
		{"$grounding.verdict?", "grounded", false},
		{"$nope", nil, true},
		{"$input.nope", nil, true},
	}
	for _, tc := range cases {
		got, err := Resolve(&run, tc.expr)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.expr, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestAdvanceBindsLast(t *testing.T) {
	def := testDef()
	run := newRun("grounding")
	d := Advance(def, run, "ungrounded", map[string]any{"questions": []any{"q1"}})
	if d.Run.State != "asking" {
		t.Fatalf("state = %s", d.Run.State)
	}
	last, _ := d.Run.Context["last"].(map[string]any)
	if last == nil || last["questions"] == nil {
		t.Fatalf("$last not bound: %+v", d.Run.Context)
	}
	g := d.Actions[0].(ActionOpenGate)
	if g.Payload != "questions: [q1]" {
		t.Fatalf("payload = %q", g.Payload)
	}
}

func TestValidateReservedStateNames(t *testing.T) {
	def := testDef()
	def.States["last"] = State{Terminal: true}
	found := false
	for _, err := range Validate(def) {
		if strings.Contains(err.Error(), "reserved name") {
			found = true
		}
	}
	if !found {
		t.Fatal("state named 'last' should be rejected")
	}
}
