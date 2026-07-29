package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
	"github.com/kidkuddy/krakoa/internal/store"
)

// copyWorkspace makes a throwaway copy of the demo fixture the test can edit.
func copyWorkspace(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := "testdata/demo"
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// The daemon used to hold its startup copy of every workspace for its whole
// lifetime: a config change sat on disk while runs used the version before
// it, with validate and doctor both green. It re-reads them now.
func TestReloadPicksUpEdits(t *testing.T) {
	dir := copyWorkspace(t)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &fakeClock{t: time.Now()} // real base: mtimes are real, so the detector is actually exercised
	eng := New(st, newFakeRunner(), clk, nil, t.TempDir())
	eng.Spawn = func(f func()) { f() }
	eng.WorkspacePaths = []string{dir}
	eng.LoadWorkspaces()

	if _, err := eng.Workspaces["demo"].Repo("acme/late-add"); err == nil {
		t.Fatal("fixture already knows the repo this test adds")
	}

	// add a repo to the workspace, exactly the uncommitted edit from the report
	f := filepath.Join(dir, "workspace.yaml")
	raw, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, append(raw, "\n  acme/late-add: /clones/late\n"...), 0o644); err != nil {
		t.Fatal(err)
	}

	clk.t = clk.t.Add(reloadSweepEvery + time.Second)
	eng.sweepReload()

	got, err := eng.Workspaces["demo"].Repo("acme/late-add")
	if err != nil || got != "/clones/late" {
		t.Fatalf("edit not picked up: %q, %v", got, err)
	}
}

// A reload must not swap definitions out from under a run whose agent is
// mid-step — the same condition a restart carries.
func TestReloadDefersWhileAStepRuns(t *testing.T) {
	dir := copyWorkspace(t)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &fakeClock{t: time.Now()} // real base: mtimes are real, so the detector is actually exercised
	eng := New(st, newFakeRunner(), clk, nil, t.TempDir())
	eng.Spawn = func(f func()) { f() }
	eng.WorkspacePaths = []string{dir}
	eng.LoadWorkspaces()

	now := clk.Now()
	if err := st.CreateRun(&core.Run{
		ID: "task-lifecycle-live", Workspace: "demo", Workflow: "task-lifecycle",
		DefHash: "h", State: "refining", Status: core.StatusRunning,
		Inputs: map[string]any{}, Context: map[string]any{}, EdgeCounts: map[string]int{},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	f := filepath.Join(dir, "workspace.yaml")
	raw, _ := os.ReadFile(f)
	os.WriteFile(f, append(raw, "\n  acme/late-add: /clones/late\n"...), 0o644)

	clk.t = clk.t.Add(reloadSweepEvery + time.Second)
	eng.sweepReload()
	if _, err := eng.Workspaces["demo"].Repo("acme/late-add"); err == nil {
		t.Fatal("reloaded under a running step")
	}

	// once the step is done the edit lands
	run, _ := st.GetRun("task-lifecycle-live")
	run.Status = core.StatusWaiting
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(reloadSweepEvery + time.Second)
	eng.sweepReload()
	if _, err := eng.Workspaces["demo"].Repo("acme/late-add"); err != nil {
		t.Fatalf("edit never landed: %v", err)
	}
}

// A broken edit must not take a working workspace away: the daemon keeps
// serving the copy it has and says why.
func TestReloadKeepsPreviousOnBrokenEdit(t *testing.T) {
	dir := copyWorkspace(t)
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	clk := &fakeClock{t: time.Now()} // real base: mtimes are real, so the detector is actually exercised
	eng := New(st, newFakeRunner(), clk, nil, t.TempDir())
	eng.Spawn = func(f func()) { f() }
	eng.WorkspacePaths = []string{dir}
	eng.LoadWorkspaces()

	os.WriteFile(filepath.Join(dir, "agents", "broken.yaml"), []byte("name: broken\nskills: [nope]\n"), 0o644)
	clk.t = clk.t.Add(reloadSweepEvery + time.Second)
	eng.sweepReload()

	if eng.Workspaces["demo"] == nil {
		t.Fatal("a bad edit took the workspace away")
	}
	if len(eng.Invalid[dir]) == 0 {
		t.Fatal("the refusal was not reported")
	}
	if !strings.Contains(strings.Join(eng.Invalid[dir], " "), "nope") {
		t.Fatalf("refusal does not name the cause: %v", eng.Invalid[dir])
	}
}

// Deleting a workflow must take effect too. A removed file leaves no mtime
// to compare — only its parent directory's — so the first cut of the change
// detector kept deleted state machines live until the next restart.
func TestReloadSeesDeletions(t *testing.T) {
	dir := copyWorkspace(t)
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	clk := &fakeClock{t: time.Now()} // real base: mtimes are real, so the detector is actually exercised
	eng := New(st, newFakeRunner(), clk, nil, t.TempDir())
	eng.Spawn = func(f func()) { f() }
	eng.WorkspacePaths = []string{dir}
	eng.LoadWorkspaces()
	if eng.Workspaces["demo"].Workflows["task-lifecycle"] == nil {
		t.Fatal("fixture has no task-lifecycle to delete")
	}

	if err := os.Remove(filepath.Join(dir, "workflows", "task-lifecycle.yaml")); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(reloadSweepEvery + time.Second)
	eng.sweepReload()

	if eng.Workspaces["demo"].Workflows["task-lifecycle"] != nil {
		t.Fatal("deleted workflow is still live")
	}
}
