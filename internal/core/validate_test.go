package core

import (
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	valid := testDef // from interp_test.go

	cases := []struct {
		name    string
		mutate  func(d *WorkflowDefinition)
		wantErr string // substring; "" = valid
	}{
		{"the test definition is valid", func(d *WorkflowDefinition) {}, ""},
		{"missing name", func(d *WorkflowDefinition) { d.Name = "" }, "name is required"},
		{"missing start", func(d *WorkflowDefinition) { d.Start = "" }, "start state is required"},
		{"unknown start", func(d *WorkflowDefinition) { d.Start = "zzz" }, "does not exist"},
		{"schedule without cron", func(d *WorkflowDefinition) { d.Trigger = Trigger{Kind: TriggerSchedule} }, "requires cron"},
		{"watcher without name", func(d *WorkflowDefinition) { d.Trigger = Trigger{Kind: TriggerWatcher} }, "requires watcher name"},
		{"unknown trigger", func(d *WorkflowDefinition) { d.Trigger = Trigger{Kind: "vibes"} }, "unknown trigger"},
		{
			"transition to unknown state",
			func(d *WorkflowDefinition) {
				s := d.States["grounding"]
				s.On = map[string]string{"grounded": "zzz"}
				d.States["grounding"] = s
			},
			"targets unknown state",
		},
		{
			"non-terminal without transitions",
			func(d *WorkflowDefinition) {
				s := d.States["grounding"]
				s.On = nil
				d.States["grounding"] = s
			},
			"needs at least one transition",
		},
		{
			"budget on missing edge",
			func(d *WorkflowDefinition) {
				s := d.States["asking"]
				s.Budgets = map[string]int{"nope": 1}
				d.States["asking"] = s
			},
			"no such transition",
		},
		{
			"agent without agent name",
			func(d *WorkflowDefinition) {
				s := d.States["refining"]
				s.Agent = ""
				d.States["refining"] = s
			},
			"requires agent",
		},
		{
			"choice gate without options",
			func(d *WorkflowDefinition) {
				s := d.States["stalled"]
				s.Gate = &GateSpec{Kind: GateChoice}
				d.States["stalled"] = s
			},
			"requires options",
		},
		{
			"wait without timeout arm",
			func(d *WorkflowDefinition) {
				s := d.States["building"]
				s.Arms = []WaitArm{{Event: "ticket-in-review"}}
				d.States["building"] = s
			},
			"must include a timeout arm",
		},
		{
			"wait arm with event and timeout both set",
			func(d *WorkflowDefinition) {
				s := d.States["building"]
				s.Arms = []WaitArm{{Event: "ticket-in-review", Timeout: Duration(time.Hour)}}
				d.States["building"] = s
			},
			"exactly one of",
		},
		{
			"event arm without matching transition",
			func(d *WorkflowDefinition) {
				s := d.States["building"]
				s.Arms = append(s.Arms, WaitArm{Event: "mystery"})
				d.States["building"] = s
			},
			"has no transition",
		},
		{
			"spawn rejected",
			func(d *WorkflowDefinition) {
				d.States["spawny"] = State{Step: StepSpawn, On: map[string]string{"ok": "done"}}
				s := d.States["refining"]
				s.On = map[string]string{"ok": "spawny"}
				d.States["refining"] = s
			},
			"not supported yet",
		},
		{
			"terminal with step",
			func(d *WorkflowDefinition) {
				d.States["done"] = State{Terminal: true, Step: StepAgent, Agent: "x"}
			},
			"carry no step",
		},
		{
			"no terminal state",
			func(d *WorkflowDefinition) {
				d.States["done"] = State{Step: StepAgent, Agent: "x", On: map[string]string{"ok": "abandoned"}}
				d.States["abandoned"] = State{Step: StepAgent, Agent: "x", On: map[string]string{"ok": "done"}}
			},
			"at least one terminal",
		},
		{
			"unreachable state",
			func(d *WorkflowDefinition) {
				d.States["island"] = State{Step: StepAgent, Agent: "x", On: map[string]string{"ok": "done"}}
			},
			"unreachable",
		},
		{
			// A verb's own entry state is unreachable from `start` by design;
			// declaring the entry is what makes it reachable.
			"entry makes its start state reachable",
			func(d *WorkflowDefinition) {
				d.States["island"] = State{Step: StepAgent, Agent: "x", On: map[string]string{"ok": "done"}}
				d.Entries = map[string]Entry{"followup": {Start: "island"}}
			},
			"",
		},
		{
			"entry with unknown start",
			func(d *WorkflowDefinition) {
				d.Entries = map[string]Entry{"followup": {Start: "zzz"}}
			},
			`entry followup: start state "zzz" does not exist`,
		},
		{
			"entry without start",
			func(d *WorkflowDefinition) {
				d.Entries = map[string]Entry{"followup": {}}
			},
			"entry followup: start state is required",
		},
		{
			"entry named start",
			func(d *WorkflowDefinition) {
				d.Entries = map[string]Entry{"start": {Start: "refining"}}
			},
			"reserved name",
		},
		{
			// A seed stands in for a state that already ran; a key naming no
			// state seeds a context nothing reads.
			"seed key that is not a state",
			func(d *WorkflowDefinition) {
				d.Entries = map[string]Entry{"followup": {
					Start: "refining",
					Seed:  map[string]map[string]string{"filign": {"ticket_id": "$input.ticket_id"}},
				}}
			},
			`seed key "filign" is not a state`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := valid()
			tc.mutate(def)
			errs := Validate(def)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.wantErr) {
					return
				}
			}
			t.Fatalf("no error containing %q in %v", tc.wantErr, errs)
		})
	}
}
