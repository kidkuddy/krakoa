package core

import (
	"fmt"
	"regexp"
	"strings"
)

// refRe matches $input.x, $state, $state.field.sub in payload strings.
var refRe = regexp.MustCompile(`\$[a-zA-Z0-9_-]+(\.[a-zA-Z0-9_-]+)*`)

// Resolve looks up a $-path against a run's inputs and context.
// "$input.idea" -> run.Inputs["idea"]; "$grounding.questions" ->
// run.Context["grounding"].(map)["questions"]; non-$ values are literals.
func Resolve(run *Run, expr string) (any, error) {
	if !strings.HasPrefix(expr, "$") {
		return expr, nil
	}
	parts := strings.Split(strings.TrimPrefix(expr, "$"), ".")
	var cur any
	if parts[0] == "input" {
		if len(parts) == 1 {
			return run.Inputs, nil
		}
		cur = run.Inputs
	} else {
		cur = run.Context
	}
	start := 0
	if parts[0] == "input" {
		start = 1
	}
	for i := start; i < len(parts); i++ {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resolve %s: %s is not a map", expr, strings.Join(parts[:i], "."))
		}
		cur, ok = m[parts[i]]
		if !ok {
			return nil, fmt.Errorf("resolve %s: %q not found", expr, parts[i])
		}
	}
	return cur, nil
}

// ResolveInputs materializes a state's `in:` template map.
func ResolveInputs(run *Run, in map[string]string) (map[string]any, error) {
	out := make(map[string]any, len(in))
	for k, v := range in {
		rv, err := Resolve(run, v)
		if err != nil {
			return nil, err
		}
		out[k] = rv
	}
	return out, nil
}

// Interpolate replaces $-refs inside a payload string with their values.
// Unresolvable refs are left verbatim (payloads are human-facing; better a
// visible $ref than a hard failure at gate-delivery time).
func Interpolate(run *Run, s string) string {
	return refRe.ReplaceAllStringFunc(s, func(ref string) string {
		v, err := Resolve(run, ref)
		if err != nil {
			return ref
		}
		return fmt.Sprintf("%v", v)
	})
}
