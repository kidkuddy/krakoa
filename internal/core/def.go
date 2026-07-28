// Package core holds the domain entities and the pure interpreter.
// Zero I/O by design: everything here is table-testable.
package core

import (
	"fmt"
	"sort"
)

// StepKind is the closed set of work a state can carry.
type StepKind string

const (
	StepAgent StepKind = "agent"
	StepGate  StepKind = "gate"
	StepWait  StepKind = "wait"
	// StepNotify says one line to the human through the same contact channel
	// gates use — thread-aware, instant, free. It replaces spawning an agent
	// session per sentence.
	StepNotify StepKind = "notify"
	StepSpawn  StepKind = "spawn" // parsed but rejected by the validator until a use case needs it
)

// GateKind is what we ask the human for.
type GateKind string

const (
	GateQuestion GateKind = "question"
	GateApproval GateKind = "approval"
	GateChoice   GateKind = "choice"
)

// TriggerKind starts runs.
type TriggerKind string

const (
	TriggerManual   TriggerKind = "manual"
	TriggerSchedule TriggerKind = "schedule"
	TriggerWatcher  TriggerKind = "watcher"
)

type Trigger struct {
	Kind          TriggerKind `yaml:"kind"`
	Cron          string      `yaml:"cron,omitempty"`
	SkipIfRunning bool        `yaml:"skip_if_running,omitempty"`
	Watcher       string      `yaml:"watcher,omitempty"` // watcher name for TriggerWatcher
}

type InputSpec struct {
	Type     string `yaml:"type"` // text | string | number | bool
	Required bool   `yaml:"required,omitempty"`
	Default  string `yaml:"default,omitempty"`
}

// GateSpec configures a gate state.
type GateSpec struct {
	Kind    GateKind `yaml:"kind"`
	Payload string   `yaml:"payload"` // template; $refs resolved against run context
	Options []string `yaml:"options,omitempty"`
}

// ProbeSpec is a probe evaluated on a cadence inside a wait arm: either a
// cheap agent (judgment) or a deterministic workspace command (pure
// observation — no LLM, no session, no cost). Exactly one of Agent/Command.
type ProbeSpec struct {
	Agent       string `yaml:"agent,omitempty"`
	Instruction string `yaml:"instruction,omitempty"`
	// Command is a workspace-relative script invocation (templates allowed,
	// e.g. "scripts/pipeline-check $merging.merge_sha"); it must print the
	// probe result JSON on stdout.
	Command string   `yaml:"command,omitempty"`
	Every   Duration `yaml:"every"`
}

// WaitArm is one arm of a wait state; first to fire wins.
// Exactly one of Event / Timeout / Probe is set.
type WaitArm struct {
	Event   string     `yaml:"event,omitempty"`   // fires outcome = event name
	Timeout Duration   `yaml:"timeout,omitempty"` // fires outcome = "timeout"
	Probe   *ProbeSpec `yaml:"probe,omitempty"`   // fires outcome = probe result outcome
	// Correlate is a template (e.g. "$filing.ticket_id") matched against an
	// incoming event's key to route resume-mode watcher events to the right
	// run. Empty = match any event of this name in the workspace.
	Correlate string `yaml:"correlate,omitempty"`
}

// State carries at most one unit of work and its transitions.
type State struct {
	Step     StepKind `yaml:"step,omitempty"` // empty iff Terminal
	Terminal bool     `yaml:"terminal,omitempty"`
	// Class tags a step for workspace policies (e.g. gatekeeper: class
	// "code-review" may only bind agent "reviewer").
	Class string `yaml:"class,omitempty"`

	// agent
	Agent       string            `yaml:"agent,omitempty"`
	Instruction string            `yaml:"instruction,omitempty"`
	In          map[string]string `yaml:"in,omitempty"` // input templates
	Retry       int               `yaml:"retry,omitempty"`

	// gate
	Gate *GateSpec `yaml:"gate,omitempty"`

	// notify
	Message string `yaml:"message,omitempty"` // template; transitions on "ok"

	// wait
	Arms []WaitArm `yaml:"arms,omitempty"`

	// Requires names workspace checks that must pass to ENTER this state
	// (e.g. only rollout-wait needs the cluster). A failing check blocks the
	// run instead of letting it fail expensively.
	Requires []string `yaml:"requires,omitempty"`

	// transitions: outcome -> next state
	On map[string]string `yaml:"on,omitempty"`
	// loop budgets: outcome -> max traversals of that edge; exceed parks the run
	Budgets map[string]int `yaml:"budgets,omitempty"`
}

// WorkflowDefinition is a declarative state machine, authored as data.
type WorkflowDefinition struct {
	Name        string               `yaml:"name"`
	Workspace   string               `yaml:"-"` // stamped by the loader
	Hash        string               `yaml:"-"` // content hash, stamped by the loader
	Trigger     Trigger              `yaml:"trigger"`
	Inputs      map[string]InputSpec `yaml:"inputs,omitempty"`
	Concurrency int                  `yaml:"concurrency,omitempty"` // 0 = unlimited
	// Thread is a template naming the piece of work this run serves (e.g.
	// "$filing.ticket_id" or "$input.ticket_id"). Stamped on the run as
	// soon as it resolves; runs sharing a thread key group together.
	Thread string `yaml:"thread,omitempty"`
	// Requires names workspace checks that must pass at admission.
	Requires []string `yaml:"requires,omitempty"`
	// Unique is a template identifying the work a run does. Two active runs may
	// not share one — a manual review and a watcher-spawned review of the same
	// MR at the same head are the same job done twice.
	Unique string `yaml:"unique,omitempty"`
	Start  string `yaml:"start"`
	// Entries are the named verbs into this workflow beside the default
	// `start`. A followup is not a new piece of work — it re-enters the
	// same lifecycle further down, carrying what the earlier run already
	// established.
	Entries map[string]Entry `yaml:"entries,omitempty"`
	States  map[string]State `yaml:"states"`
}

// Entry is one named way into a workflow — a verb. It begins at its own
// state and seeds the facts an earlier run established, so the states
// downstream resolve their $refs exactly as they would mid-lifecycle.
type Entry struct {
	Start string `yaml:"start"`
	// Inputs are merged over the workflow's own: an entry adds the inputs
	// its verb needs, and redeclares any shared input whose `required` does
	// not apply to it.
	Inputs map[string]InputSpec `yaml:"inputs,omitempty"`
	// Seed writes state-name -> result into the run's context at creation,
	// each value a template over $input. Seeding `filing.ticket_id` is not
	// a fiction: the ticket really was filed, by the run this one follows.
	Seed map[string]map[string]string `yaml:"seed,omitempty"`
}

// EntryFor resolves an entry name ("" = the default) to the state a run
// starts in and the input specs it must satisfy.
func (d *WorkflowDefinition) EntryFor(name string) (Entry, error) {
	if name == "" {
		return Entry{Start: d.Start, Inputs: d.Inputs}, nil
	}
	e, ok := d.Entries[name]
	if !ok {
		return Entry{}, fmt.Errorf("workflow %s has no entry %q (declares %v)", d.Name, name, d.EntryNames())
	}
	merged := make(map[string]InputSpec, len(d.Inputs)+len(e.Inputs))
	for k, v := range d.Inputs {
		merged[k] = v
	}
	for k, v := range e.Inputs {
		merged[k] = v
	}
	e.Inputs = merged
	return e, nil
}

// EntryNames lists the declared verbs, sorted, for error messages.
func (d *WorkflowDefinition) EntryNames() []string {
	names := make([]string, 0, len(d.Entries))
	for n := range d.Entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// AgentSpec is a named agent description, referenced by workflow states.
type AgentSpec struct {
	Name          string   `yaml:"name"`
	Persona       string   `yaml:"persona"`          // becomes the agent home CLAUDE.md
	Skills        []string `yaml:"skills,omitempty"` // workspace skill names carried
	WorkingFolder string   `yaml:"working_folder,omitempty"`
	Worktree      bool     `yaml:"worktree,omitempty"` // run in a fresh git worktree of WorkingFolder
	Model         string   `yaml:"model,omitempty"`
	Effort        string   `yaml:"effort,omitempty"`
	ResultSchema  string   `yaml:"result_schema,omitempty"` // JSON Schema for result.json
}

// WatcherSpec is a scheduled probe emitting deduped events: an agent probe
// (judgment) or a deterministic workspace command (pure observation).
// Exactly one of Agent/Command.
type WatcherSpec struct {
	Name        string `yaml:"name"`
	Agent       string `yaml:"agent,omitempty"`
	Instruction string `yaml:"instruction,omitempty"`
	// Command is a workspace-relative script printing the observations JSON
	// ({"outcome":"ok","events":[...]}) on stdout.
	Command  string   `yaml:"command,omitempty"`
	Every    Duration `yaml:"every"`
	Mode     string   `yaml:"mode"`               // spawn | resume
	Workflow string   `yaml:"workflow,omitempty"` // spawn mode: workflow to start
	// SpawnOn lists the event names that spawn runs (spawn mode); other
	// events route as correlation resumes. Empty = every unmatched event
	// spawns.
	SpawnOn []string `yaml:"spawn_on,omitempty"`
}
