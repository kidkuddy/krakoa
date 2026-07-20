package core

import "time"

type RunStatus string

const (
	StatusQueued         RunStatus = "queued" // FIFO-parked behind the concurrency limit
	StatusRunning        RunStatus = "running"
	StatusWaiting        RunStatus = "waiting" // parked on a wait state's arms
	StatusGated          RunStatus = "gated"   // parked on a human gate
	StatusDone           RunStatus = "done"
	StatusFailed         RunStatus = "failed"
	StatusNeedsAttention RunStatus = "needs-attention"
	StatusCanceled       RunStatus = "canceled"
)

// Terminal reports whether no further work will happen on a run.
func (s RunStatus) Terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCanceled
}

// Run is one instance of a workflow.
type Run struct {
	ID        string
	Workspace string
	Workflow  string
	DefHash   string // pinned at start; in-flight runs never see edits
	WSVersion string // workspace git version at start

	State  string
	Status RunStatus
	// Thread groups runs serving the same piece of work (ticket id etc.);
	// empty until the definition's thread template resolves.
	Thread string

	Inputs  map[string]any
	Context map[string]any // state name -> that step's result
	// EdgeCounts tracks loop-budget consumption: "state/outcome" -> traversals.
	EdgeCounts map[string]int

	Parent    string // parent run id if spawned
	CreatedAt time.Time
	UpdatedAt time.Time
}

// StepExecution is one attempt of one state's work.
type StepExecution struct {
	ID      int64
	RunID   string
	State   string
	Kind    StepKind
	Attempt int

	Inputs  map[string]any
	Outcome string
	Result  map[string]any
	Error   string

	// Claude Code session, for replay/resume/post-mortem.
	SessionID   string
	SessionPath string
	HandoffDir  string
	CostUSD     float64

	StartedAt time.Time
	EndedAt   time.Time
}

type GateStatus string

const (
	GateOpen     GateStatus = "open"
	GateAnswered GateStatus = "answered"
	GateCanceled GateStatus = "canceled"
)

// Gate is a parked human interaction. First response wins.
type Gate struct {
	ID        string
	Workspace string
	RunID     string
	State     string
	Kind      GateKind
	Payload   string
	Options   []string

	Status     GateStatus
	Response   string         // outcome: option chosen / approved / rejected / answered
	Answers    map[string]any // question-gate answers
	Responder  string
	CreatedAt  time.Time
	ResolvedAt time.Time

	// Delivery records per-channel outcome ("ok" or the error) so an
	// undelivered ping is visible, never a silent fallback.
	Delivery map[string]string
}

// Event is one row of the append-only audit spine.
type Event struct {
	ID        int64
	Workspace string
	RunID     string
	State     string
	Kind      string // transition, step-started, step-finished, gate-opened, gate-answered, watcher-observed, emit, intervention, ...
	Data      map[string]any
	At        time.Time
}
