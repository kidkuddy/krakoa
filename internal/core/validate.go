package core

import (
	"fmt"
	"sort"
)

// Validate checks a definition's structural invariants. Reference checks
// (agent names, watcher names) belong to the workspace loader, which knows
// the full workspace.
func Validate(def *WorkflowDefinition) []error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if def.Name == "" {
		fail("workflow name is required")
	}
	if def.Start == "" {
		fail("start state is required")
	}
	if len(def.States) == 0 {
		fail("at least one state is required")
		return errs
	}
	if _, ok := def.States[def.Start]; def.Start != "" && !ok {
		fail("start state %q does not exist", def.Start)
	}

	switch def.Trigger.Kind {
	case TriggerManual:
	case TriggerSchedule:
		if def.Trigger.Cron == "" {
			fail("schedule trigger requires cron")
		}
	case TriggerWatcher:
		if def.Trigger.Watcher == "" {
			fail("watcher trigger requires watcher name")
		}
	default:
		fail("unknown trigger kind %q", def.Trigger.Kind)
	}

	// Deterministic iteration for stable error output.
	names := make([]string, 0, len(def.States))
	for n := range def.States {
		names = append(names, n)
	}
	sort.Strings(names)

	terminals := 0
	for _, name := range names {
		st := def.States[name]
		if st.Terminal {
			terminals++
			if st.Step != "" || len(st.On) > 0 {
				fail("state %s: terminal states carry no step or transitions", name)
			}
			continue
		}
		if len(st.On) == 0 {
			fail("state %s: non-terminal state needs at least one transition", name)
		}
		for outcome, target := range st.On {
			if _, ok := def.States[target]; !ok {
				fail("state %s: transition on %q targets unknown state %q", name, outcome, target)
			}
		}
		for outcome := range st.Budgets {
			if _, ok := st.On[outcome]; !ok {
				fail("state %s: budget on %q but no such transition", name, outcome)
			}
		}

		switch st.Step {
		case StepAgent:
			if st.Agent == "" {
				fail("state %s: agent step requires agent", name)
			}
		case StepGate:
			if st.Gate == nil {
				fail("state %s: gate step requires gate spec", name)
				continue
			}
			switch st.Gate.Kind {
			case GateQuestion, GateApproval:
			case GateChoice:
				if len(st.Gate.Options) == 0 {
					fail("state %s: choice gate requires options", name)
				}
			default:
				fail("state %s: unknown gate kind %q", name, st.Gate.Kind)
			}
		case StepWait:
			if len(st.Arms) == 0 {
				fail("state %s: wait step requires arms", name)
				continue
			}
			hasTimeout := false
			for i, arm := range st.Arms {
				set := 0
				if arm.Event != "" {
					set++
					if _, ok := st.On[arm.Event]; !ok {
						fail("state %s: arm %d event %q has no transition", name, i, arm.Event)
					}
				}
				if arm.Timeout > 0 {
					set++
					hasTimeout = true
					if _, ok := st.On["timeout"]; !ok {
						fail("state %s: timeout arm but no transition on \"timeout\"", name)
					}
				}
				if arm.Probe != nil {
					set++
					if arm.Probe.Agent == "" || arm.Probe.Every <= 0 {
						fail("state %s: arm %d probe requires agent and every", name, i)
					}
				}
				if set != 1 {
					fail("state %s: arm %d must set exactly one of event/timeout/probe", name, i)
				}
			}
			if !hasTimeout {
				fail("state %s: wait states must include a timeout arm", name)
			}
		case StepSpawn:
			fail("state %s: spawn steps are not supported yet", name)
		default:
			fail("state %s: unknown step kind %q", name, st.Step)
		}
	}
	if terminals == 0 {
		fail("at least one terminal state is required")
	}

	// Reachability from start.
	if _, ok := def.States[def.Start]; ok {
		seen := map[string]bool{def.Start: true}
		frontier := []string{def.Start}
		for len(frontier) > 0 {
			cur := frontier[len(frontier)-1]
			frontier = frontier[:len(frontier)-1]
			for _, target := range def.States[cur].On {
				if !seen[target] {
					seen[target] = true
					frontier = append(frontier, target)
				}
			}
		}
		for _, name := range names {
			if !seen[name] {
				fail("state %s: unreachable from start", name)
			}
		}
	}
	return errs
}
