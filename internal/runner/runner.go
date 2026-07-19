// Package runner executes agent steps as Claude Code headless sessions.
package runner

import (
	"context"

	"github.com/kidkuddy/krakoa/internal/core"
)

// Request is one agent step attempt.
type Request struct {
	RunID string
	State string
	Spec  *core.AgentSpec

	Instruction string
	Inputs      map[string]any

	// Skills maps skill name -> source dir (containing SKILL.md) to carry.
	Skills map[string]string

	// BaseDir is this attempt's private directory. The runner creates the
	// agent cwd under it; the handoff dir lives at BaseDir/handoff.
	BaseDir    string
	HandoffDir string

	// Resume, when set, resumes an existing session (schema-fix retry)
	// instead of starting fresh; ResumeMessage is the feedback prompt.
	Resume        string
	ResumeMessage string
}

// Result reports one attempt. The engine reads result.json from the handoff
// dir itself — schema policy is engine business, not runner business.
type Result struct {
	SessionID   string
	SessionPath string
	CostUSD     float64
	IsError     bool
	Output      string // final assistant text
}

type Runner interface {
	Run(ctx context.Context, req Request) (*Result, error)
}
