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
// A trailing "?" marks the ref optional: unresolvable -> nil, not an error
// (e.g. "$asking?" before any question round has happened).
func Resolve(run *Run, expr string) (any, error) {
	if !strings.HasPrefix(expr, "$") {
		return expr, nil
	}
	optional := strings.HasSuffix(expr, "?")
	if optional {
		expr = strings.TrimSuffix(expr, "?")
	}
	v, err := resolvePath(run, expr)
	if err != nil && optional {
		return nil, nil
	}
	return v, err
}

func resolvePath(run *Run, expr string) (any, error) {
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
	out, _ := interpolate(run, s)
	return out
}

// interpolate is Interpolate plus a count of the refs it could not resolve, for
// callers that need "is this fully resolved?" rather than a best effort.
func interpolate(run *Run, s string) (string, int) {
	unresolved := 0
	out := refRe.ReplaceAllStringFunc(s, func(ref string) string {
		v, err := Resolve(run, ref)
		if err != nil {
			unresolved++
			return ref
		}
		return fmt.Sprintf("%v", v)
	})
	return out, unresolved
}

// ResolveKey renders a key template — a thread or uniqueness key — that may mix
// literal text with refs, as in "scrum-$input.ceremony". Resolve alone cannot:
// it treats anything not starting with "$" as a literal, so a composite key
// stamped verbatim and every run collided on it.
//
// ok is false while any ref is still unresolved, which is what lets a key
// naming a later state ("$filing.ticket_id") wait for that state instead of
// stamping the ref itself as the key.
func ResolveKey(run *Run, tmpl string) (string, bool) {
	if tmpl == "" {
		return "", false
	}
	out, unresolved := interpolate(run, tmpl)
	if unresolved > 0 || out == "" || strings.Contains(out, "<nil>") {
		return "", false
	}
	return out, true
}
