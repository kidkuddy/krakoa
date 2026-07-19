package core

import "fmt"

// Actions are what the executor performs through ports. The interpreter only
// decides; it never does.

type ActionRunAgent struct {
	State       string
	Agent       string
	Instruction string
	Inputs      map[string]any
	Retry       int
}

type ActionOpenGate struct {
	State   string
	Kind    GateKind
	Payload string // resolved
	Options []string
}

type ActionArmWait struct {
	State string
	Arms  []WaitArm
}

// ActionPark opens a needs-attention gate (options: retry, abandon).
type ActionPark struct {
	State  string
	Reason string
}

type ActionFinish struct {
	State string // terminal state name reached
}

type Action any

// Decision is the interpreter's output: the updated run plus the actions
// required to make the world match it.
type Decision struct {
	Run     Run
	Actions []Action
}

// AttentionOptions are the fixed choices on a needs-attention gate.
var AttentionOptions = []string{"retry", "abandon"}

// Enter produces the actions for the run's current state. Called after
// admission (run.State == def.Start) and after every transition.
func Enter(def *WorkflowDefinition, run Run) Decision {
	st, ok := def.States[run.State]
	if !ok {
		return park(run, fmt.Sprintf("state %q not in definition %s", run.State, def.Hash))
	}
	if st.Terminal {
		run.Status = StatusDone
		return Decision{Run: run, Actions: []Action{ActionFinish{State: run.State}}}
	}
	switch st.Step {
	case StepAgent:
		inputs, err := ResolveInputs(&run, st.In)
		if err != nil {
			return park(run, fmt.Sprintf("state %s: %v", run.State, err))
		}
		run.Status = StatusRunning
		return Decision{Run: run, Actions: []Action{ActionRunAgent{
			State:       run.State,
			Agent:       st.Agent,
			Instruction: Interpolate(&run, st.Instruction),
			Inputs:      inputs,
			Retry:       st.Retry,
		}}}
	case StepGate:
		run.Status = StatusGated
		return Decision{Run: run, Actions: []Action{ActionOpenGate{
			State:   run.State,
			Kind:    st.Gate.Kind,
			Payload: Interpolate(&run, st.Gate.Payload),
			Options: st.Gate.Options,
		}}}
	case StepWait:
		run.Status = StatusWaiting
		return Decision{Run: run, Actions: []Action{ActionArmWait{State: run.State, Arms: st.Arms}}}
	default:
		return park(run, fmt.Sprintf("state %s: unsupported step kind %q", run.State, st.Step))
	}
}

// Advance consumes an outcome for the run's current state and enters the
// next one. result (may be nil) is saved into run.Context[state].
func Advance(def *WorkflowDefinition, run Run, outcome string, result map[string]any) Decision {
	st, ok := def.States[run.State]
	if !ok {
		return park(run, fmt.Sprintf("state %q not in definition %s", run.State, def.Hash))
	}
	if result != nil {
		if run.Context == nil {
			run.Context = map[string]any{}
		}
		run.Context[run.State] = result
		// "$last" always names the result that caused this transition —
		// the only safe payload source for states with multiple incoming
		// edges (e.g. a question gate fed by refiner OR grounder).
		run.Context["last"] = result
	}
	next, ok := st.On[outcome]
	if !ok {
		return park(run, fmt.Sprintf("state %s: no transition for outcome %q", run.State, outcome))
	}
	if budget, has := st.Budgets[outcome]; has {
		if run.EdgeCounts == nil {
			run.EdgeCounts = map[string]int{}
		}
		key := run.State + "/" + outcome
		run.EdgeCounts[key]++
		if run.EdgeCounts[key] > budget {
			return park(run, fmt.Sprintf("loop budget exceeded: %s (%d/%d)", key, run.EdgeCounts[key], budget))
		}
	}
	run.State = next
	return Enter(def, run)
}

// ResolveAttention handles the response to a needs-attention gate:
// retry re-enters the current state, abandon fails the run.
func ResolveAttention(def *WorkflowDefinition, run Run, choice string) Decision {
	switch choice {
	case "retry":
		return Enter(def, run)
	case "abandon":
		run.Status = StatusFailed
		return Decision{Run: run}
	default:
		return park(run, fmt.Sprintf("unknown needs-attention choice %q", choice))
	}
}

func park(run Run, reason string) Decision {
	run.Status = StatusNeedsAttention
	return Decision{Run: run, Actions: []Action{ActionPark{State: run.State, Reason: reason}}}
}
