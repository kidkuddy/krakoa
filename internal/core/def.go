// Package core holds the domain entities and the pure interpreter.
// Zero I/O by design: everything here is table-testable.
package core

// StepKind is the closed set of work a state can carry.
type StepKind string

const (
	StepAgent StepKind = "agent"
	StepGate  StepKind = "gate"
	StepWait  StepKind = "wait"
	StepSpawn StepKind = "spawn" // parsed but rejected by the validator until a use case needs it
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

// ProbeSpec is a cheap agent probe evaluated on a cadence inside a wait arm.
type ProbeSpec struct {
	Agent       string   `yaml:"agent"`
	Instruction string   `yaml:"instruction"`
	Every       Duration `yaml:"every"`
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

	// wait
	Arms []WaitArm `yaml:"arms,omitempty"`

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
	Start       string               `yaml:"start"`
	States      map[string]State     `yaml:"states"`
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

// WatcherSpec is a scheduled probe agent emitting deduped events.
type WatcherSpec struct {
	Name        string   `yaml:"name"`
	Agent       string   `yaml:"agent"`
	Instruction string   `yaml:"instruction,omitempty"`
	Every       Duration `yaml:"every"`
	Mode        string   `yaml:"mode"`               // spawn | resume
	Workflow    string   `yaml:"workflow,omitempty"` // spawn mode: workflow to start
	// SpawnOn lists the event names that spawn runs (spawn mode); other
	// events route as correlation resumes. Empty = every unmatched event
	// spawns.
	SpawnOn []string `yaml:"spawn_on,omitempty"`
}
