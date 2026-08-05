package core

import "time"

// A Pause suspends work without destroying it. Cancelling a run is terminal
// and throws away everything it has done; a pause stops krakoa spawning
// anything NEW and leaves every run standing where it is, so the answer to
// "stop burning sessions" is not "lose the week's work".
//
// Workflow "" pauses every workflow in the workspace.
type Pause struct {
	Workspace string
	Workflow  string
	Reason    string
	Since     time.Time
}

// Covers reports whether this pause suspends the given workflow. A
// workspace-wide pause (Workflow "") covers all of them.
func (p Pause) Covers(ws, wf string) bool {
	return p.Workspace == ws && (p.Workflow == "" || p.Workflow == wf)
}

// Scope names what the pause holds, for messages and listings.
func (p Pause) Scope() string {
	if p.Workflow == "" {
		return p.Workspace
	}
	return p.Workspace + "/" + p.Workflow
}
