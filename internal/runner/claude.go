package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Claude runs agents via `claude -p --output-format json`.
// Verified contract (2026-07-19): the JSON result carries session_id,
// total_cost_usd, is_error, result; skills load from cwd .claude/skills in
// -p mode; --resume <id> -p continues a session in place.
type Claude struct {
	Bin string // resolved binary; PATH may hold a cmux shim, so resolve explicitly
}

// ResolveBin finds the real claude binary: $KRAKOA_CLAUDE_BIN, then
// ~/.local/bin/claude, then PATH (skipping cmux shims).
func ResolveBin() (string, error) {
	if b := os.Getenv("KRAKOA_CLAUDE_BIN"); b != "" {
		return b, nil
	}
	home, _ := os.UserHomeDir()
	if p := filepath.Join(home, ".local", "bin", "claude"); isExec(p) {
		return p, nil
	}
	if p, err := exec.LookPath("claude"); err == nil && !strings.Contains(p, "cmux") {
		return p, nil
	}
	return "", fmt.Errorf("claude binary not found (set KRAKOA_CLAUDE_BIN)")
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// handoffProtocol is appended to every agent's CLAUDE.md.
const handoffProtocol = `
# Krakoa handoff protocol

You are one step of a durable workflow run. You MUST write a JSON file to
$KRAKOA_HANDOFF/result.json with at least:
{"outcome": "<one of the outcomes your instructions define>"} plus any
result fields your instructions ask for. The workflow cannot proceed
without this file.

Write it ATOMICALLY — to $KRAKOA_HANDOFF/result.json.tmp first, then
rename it over result.json (` + "`mv`" + `, one syscall). A step killed
mid-write otherwise leaves a truncated file, which reads as a corrupt
result instead of a missing one.

Write result.json THE MOMENT the outcome is known — immediately, before
any cleanup, wrap-up, summaries, or final checks. Writing it is part of
the work, not an afterthought: sessions have died between "done" and
"writing the handoff", losing an hour of finished work. If later cleanup
changes the picture, overwrite the file; a checkpointed result beats a
perfect one that never lands.

Before mutating any external system (ticket, MR, message), check first
whether the effect already happened — a crashed attempt may have half-run.
`

func (c *Claude) Run(ctx context.Context, req Request) (*Result, error) {
	cwd, err := c.prepare(req)
	if err != nil {
		return nil, err
	}

	var args []string
	prompt := buildPrompt(req)
	if req.Resume != "" {
		prompt = req.ResumeMessage
		args = append(args, "--resume", req.Resume)
	}
	// --setting-sources project: NEVER load the user's global CLAUDE.md,
	// settings, or hooks — a live probe once obeyed a user-global mandate,
	// asked a question into the void, and skipped its sweep. Only the
	// agent-home persona and carried skills may steer a step.
	args = append(args, "-p", prompt, "--output-format", "json",
		"--dangerously-skip-permissions", "--setting-sources", "project")
	if m := req.Spec.Model; m != "" {
		args = append(args, "--model", m)
	}
	if e := req.Spec.Effort; e != "" {
		args = append(args, "--effort", e)
	}

	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir = cwd
	cmd.Stdin = nil // headless: never wait on stdin
	cmd.Env = append(os.Environ(),
		"KRAKOA_HANDOFF="+req.HandoffDir,
		"KRAKOA_RUN="+req.RunID,
		"KRAKOA_STATE="+req.State,
	)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		detail := ""
		if errors.As(err, &ee) {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		// claude exits non-zero on is_error results too; try parsing anyway
		if res, perr := parseResult(out, cwd); perr == nil {
			res.IsError = true
			return res, nil
		}
		return nil, fmt.Errorf("claude: %w: %s", err, detail)
	}
	return parseResult(out, cwd)
}

// prepare builds the agent's cwd: a fresh git worktree of the working folder
// when the spec asks for one, otherwise a bare home dir. Persona CLAUDE.md
// and carried skills land inside it either way.
func (c *Claude) prepare(req Request) (string, error) {
	var cwd string
	if req.Spec.Worktree && req.Spec.WorkingFolder != "" {
		cwd = filepath.Join(req.BaseDir, "worktree")
	} else {
		// Non-worktree agents run from a bare home dir; a working folder,
		// if any, is named in the prompt (skills carry the tooling).
		cwd = filepath.Join(req.BaseDir, "home")
	}
	if req.Resume != "" {
		// resuming: cwd already prepared by the first attempt. Adding the
		// worktree again is what the resume path used to do, and `git
		// worktree add` onto an existing directory is a hard exit 128 — so
		// every schema retry on a worktree agent failed for a reason that had
		// nothing to do with the retry.
		return cwd, nil
	}
	if req.Spec.Worktree && req.Spec.WorkingFolder != "" {
		// A worktree can outlive its step: the daemon is killed mid-run, or a
		// previous attempt left one registered. Clear both the directory and
		// git's record of it before adding, or every later attempt inherits
		// the failure.
		exec.Command("git", "-C", req.Spec.WorkingFolder, "worktree", "remove", "--force", cwd).Run()
		exec.Command("git", "-C", req.Spec.WorkingFolder, "worktree", "prune").Run()
		if err := os.RemoveAll(cwd); err != nil {
			return "", fmt.Errorf("clear stale worktree %s: %w", cwd, err)
		}
		wt := exec.Command("git", "-C", req.Spec.WorkingFolder, "worktree", "add", "--detach", cwd)
		if out, err := wt.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git worktree add: %w: %s", err, out)
		}
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(req.HandoffDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte(req.Spec.Persona+"\n"+handoffProtocol), 0o644); err != nil {
		return "", err
	}
	for name, src := range req.Skills {
		dst := filepath.Join(cwd, ".claude", "skills", name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
			return "", fmt.Errorf("copy skill %s: %w", name, err)
		}
	}
	return cwd, nil
}

func buildPrompt(req Request) string {
	var b strings.Builder
	b.WriteString(req.Instruction)
	if len(req.Inputs) > 0 {
		in, _ := json.MarshalIndent(req.Inputs, "", "  ")
		b.WriteString("\n\nInputs:\n")
		b.Write(in)
	}
	if req.Spec.WorkingFolder != "" && !req.Spec.Worktree {
		b.WriteString("\n\nWorking folder: " + req.Spec.WorkingFolder)
	}
	if req.Spec.WorkingFolder == "" {
		// An agent with no checkout that is asked about code goes looking for
		// one — across Downloads, Music and / — and every one of those is a
		// permission prompt on someone's laptop. Say plainly that there is
		// nothing to find.
		b.WriteString("\n\nYou have NO repository and NO checkout: your working directory is a" +
			" scratch dir that exists only for your handoff file. Everything you need is in the" +
			" inputs above. Never search the filesystem for a repo, a clone or a source tree —" +
			" if the work seems to require one, that is a mismatch to report in your result," +
			" not something to go hunting for.")
	}
	if req.Spec.ResultSchema != "" {
		b.WriteString("\n\nYour $KRAKOA_HANDOFF/result.json must match this JSON Schema:\n")
		b.WriteString(req.Spec.ResultSchema)
	}
	return b.String()
}

type claudeJSON struct {
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

func parseResult(out []byte, cwd string) (*Result, error) {
	// The result object is the last JSON line on stdout.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var cj claudeJSON
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &cj); err != nil {
		return nil, fmt.Errorf("parse claude output: %w (%.200s)", err, out)
	}
	return &Result{
		SessionID:   cj.SessionID,
		SessionPath: sessionPath(cwd, cj.SessionID),
		CostUSD:     cj.TotalCostUSD,
		IsError:     cj.IsError,
		Output:      cj.Result,
	}, nil
}

var slugRe = regexp.MustCompile(`[^A-Za-z0-9-]`)

// sessionPath computes ~/.claude/projects/<cwd-slug>/<id>.jsonl (best-effort;
// empty if the file isn't there).
func sessionPath(cwd, sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil || sessionID == "" {
		return ""
	}
	slug := slugRe.ReplaceAllString(cwd, "-")
	p := filepath.Join(home, ".claude", "projects", slug, sessionID+".jsonl")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// CleanupWorktree removes a step's git worktree (transcripts and handoff
// artifacts remain; the worktree itself is disposable).
func CleanupWorktree(workingFolder, worktreeDir string) {
	if workingFolder == "" || worktreeDir == "" {
		return
	}
	exec.Command("git", "-C", workingFolder, "worktree", "remove", "--force", worktreeDir).Run()
}
