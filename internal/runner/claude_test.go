package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kidkuddy/krakoa/internal/core"
)

// gitRepo makes a throwaway repo with one commit — `git worktree add` refuses
// to run against a repo with no HEAD.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "root"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable or failing (%v): %s", err, out)
		}
	}
	return dir
}

func worktreeReq(t *testing.T, repo string) Request {
	return Request{
		RunID: "r1", State: "refining",
		Spec:    &core.AgentSpec{Worktree: true, WorkingFolder: repo},
		BaseDir: filepath.Join(t.TempDir(), "attempt-1"),
	}
}

// A schema retry resumes the same session and must reuse the cwd the first
// attempt built. Re-running `git worktree add` on it is a hard exit 128, and
// it accounted for the second-largest class of step failures in production.
func TestPrepareOnResumeDoesNotReAddTheWorktree(t *testing.T) {
	repo := gitRepo(t)
	c := &Claude{}
	req := worktreeReq(t, repo)
	req.HandoffDir = filepath.Join(req.BaseDir, "handoff")

	cwd, err := c.prepare(req)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".git")); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}

	req.Resume = "session-abc"
	resumed, err := c.prepare(req)
	if err != nil {
		t.Fatalf("resume must reuse the prepared worktree, got: %v", err)
	}
	if resumed != cwd {
		t.Errorf("resume cwd = %q, want %q", resumed, cwd)
	}
}

// A worktree outlives its step when the daemon is killed mid-run. The next
// attempt has to clear it rather than inherit the failure forever.
func TestPrepareClearsAStaleWorktree(t *testing.T) {
	repo := gitRepo(t)
	c := &Claude{}
	req := worktreeReq(t, repo)
	req.HandoffDir = filepath.Join(req.BaseDir, "handoff")

	if _, err := c.prepare(req); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	// leave a marker behind: a fresh worktree must not contain it
	stale := filepath.Join(req.BaseDir, "worktree", "STALE")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd, err := c.prepare(req)
	if err != nil {
		t.Fatalf("second attempt must clear the stale worktree, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "STALE")); err == nil {
		t.Error("stale worktree was reused instead of rebuilt")
	}
	if _, err := os.Stat(filepath.Join(cwd, ".git")); err != nil {
		t.Errorf("rebuilt worktree is not a worktree: %v", err)
	}
}
