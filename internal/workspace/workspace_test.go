package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
)

func TestLoadValid(t *testing.T) {
	ws, errs := Load("testdata/valid")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if ws.Name != "testws" {
		t.Errorf("name = %q", ws.Name)
	}
	if len(ws.Agents) != 2 || ws.Agents["reviewer"] == nil || ws.Agents["operator"] == nil {
		t.Errorf("agents = %v", ws.Agents)
	}
	if !ws.Agents["reviewer"].Worktree {
		t.Error("reviewer worktree flag lost")
	}
	if _, ok := ws.Skills["glab-ops"]; !ok {
		t.Errorf("skills = %v", ws.Skills)
	}
	if len(ws.Checks) != 2 || ws.Checks["fake-daemon"].URL == "" || ws.Checks["fake-cli"].Command != "echo hello" || ws.Checks["fake-cli"].Fail[0] != "boom" {
		t.Errorf("checks = %+v", ws.Checks)
	}
	w := ws.Watchers["draft-mr-watch"]
	if w == nil || w.Every.D() != 10*time.Minute || w.Mode != "spawn" {
		t.Fatalf("watcher = %+v", w)
	}

	def := ws.Workflows["sweep"]
	if def == nil {
		t.Fatal("workflow sweep not loaded")
	}
	if def.Hash == "" || def.Workspace != "testws" {
		t.Errorf("stamping: hash=%q ws=%q", def.Hash, def.Workspace)
	}
	// The `on:` YAML key must decode as transitions, not a YAML-1.1 bool.
	rev := def.States["reviewing"]
	if rev.On["pass"] != "marking" || rev.On["noop"] != "done" {
		t.Errorf("on-map broken: %+v", rev.On)
	}
	if rev.Class != "code-review" {
		t.Errorf("class = %q", rev.Class)
	}
	wait := def.States["waiting"]
	if len(wait.Arms) != 2 || wait.Arms[1].Timeout.D() != 2*time.Hour {
		t.Errorf("arms = %+v", wait.Arms)
	}
	if wait.Budgets["retriggered"] != 3 {
		t.Errorf("budgets = %+v", wait.Budgets)
	}
}

// broken copies the valid fixture into a temp dir and lets the test mutate it.
func broken(t *testing.T, mutate func(dir string)) []error {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/valid")); err != nil {
		t.Fatal(err)
	}
	mutate(dir)
	_, errs := Load(dir)
	return errs
}

func wantErr(t *testing.T, errs []error, sub string) {
	t.Helper()
	for _, err := range errs {
		if strings.Contains(err.Error(), sub) {
			return
		}
	}
	t.Fatalf("no error containing %q in %v", sub, errs)
}

func TestLoadBroken(t *testing.T) {
	t.Run("gatekeeper violation", func(t *testing.T) {
		errs := broken(t, func(dir string) {
			f := filepath.Join(dir, "workflows", "sweep.yaml")
			raw, _ := os.ReadFile(f)
			out := strings.Replace(string(raw), "agent: reviewer", "agent: operator", 1)
			os.WriteFile(f, []byte(out), 0o644)
		})
		wantErr(t, errs, `class "code-review" only allows`)
	})

	t.Run("unknown agent in state", func(t *testing.T) {
		errs := broken(t, func(dir string) {
			os.Remove(filepath.Join(dir, "agents", "operator.yaml"))
		})
		wantErr(t, errs, "unknown agent")
	})

	t.Run("unknown skill on agent", func(t *testing.T) {
		errs := broken(t, func(dir string) {
			os.RemoveAll(filepath.Join(dir, "skills", "glab-ops"))
		})
		wantErr(t, errs, "unknown skill")
	})

	t.Run("watcher with unknown workflow", func(t *testing.T) {
		errs := broken(t, func(dir string) {
			os.Remove(filepath.Join(dir, "workflows", "sweep.yaml"))
		})
		wantErr(t, errs, "unknown workflow")
	})

	t.Run("unknown yaml field rejected", func(t *testing.T) {
		errs := broken(t, func(dir string) {
			f := filepath.Join(dir, "agents", "operator.yaml")
			raw, _ := os.ReadFile(f)
			os.WriteFile(f, append(raw, []byte("\ntypo_field: x\n")...), 0o644)
		})
		wantErr(t, errs, "typo_field")
	})

	t.Run("check with both command and url", func(t *testing.T) {
		errs := broken(t, func(dir string) {
			f := filepath.Join(dir, "workspace.yaml")
			body := "name: testws\nchecks:\n  bad: {command: echo x, url: http://x/}\n"
			os.WriteFile(f, []byte(body), 0o644)
		})
		wantErr(t, errs, "exactly one of command/url")
	})

	t.Run("missing workspace.yaml", func(t *testing.T) {
		errs := broken(t, func(dir string) {
			os.Remove(filepath.Join(dir, "workspace.yaml"))
		})
		wantErr(t, errs, "workspace.yaml")
	})
}

// Guard: core.Validate must run during Load (structure errors surface).
func TestLoadRunsValidator(t *testing.T) {
	errs := broken(t, func(dir string) {
		f := filepath.Join(dir, "workflows", "sweep.yaml")
		raw, _ := os.ReadFile(f)
		// drop the timeout arm -> validator must complain
		out := strings.Replace(string(raw), "      - timeout: 2h\n", "", 1)
		if out == string(raw) {
			t.Fatal("mutation did not apply")
		}
		os.WriteFile(f, []byte(out), 0o644)
	})
	wantErr(t, errs, "timeout arm")
}

var _ = core.StepAgent // keep the import if assertions above change

// A repo key that misses the map is a configuration error, not a path. The
// fall-through it replaces sent "callab.ai/echo-js" to git as a directory.
func TestRepoResolution(t *testing.T) {
	ws := &Workspace{Name: "demo", Repos: map[string]string{"acme/app": "/clones/app"}}
	if got, err := ws.Repo("acme/app"); err != nil || got != "/clones/app" {
		t.Fatalf("known key = %q, %v", got, err)
	}
	if got, err := ws.Repo("/literal/path"); err != nil || got != "/literal/path" {
		t.Fatalf("absolute path = %q, %v", got, err)
	}
	_, err := ws.Repo("acme/missing")
	if err == nil {
		t.Fatal("unknown key resolved instead of failing")
	}
	if !strings.Contains(err.Error(), "acme/missing") || !strings.Contains(err.Error(), "acme/app") {
		t.Fatalf("error names neither the bad key nor the known ones: %v", err)
	}
}

// The repos map is checked at use, but by then a run has been admitted and an
// agent spent, and the failure arrives as `git worktree add: cannot change to
// 'callab.ai/echo-js'` several states downstream. The one case knowable at load
// is the default, so it is refused there.
func TestUnknownRepoDefaultFailsValidation(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/valid")); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("workspace.yaml", "name: testws\nrepos:\n  acme/app: /clones/app\n")
	write("workflows/wf.yaml", `name: wf
trigger: {kind: manual}
inputs:
  repo: {type: string, default: acme/typo}
start: done
states:
  done: {terminal: true}
`)

	_, errs := Load(dir)
	if len(errs) == 0 {
		t.Fatal("a repo default that is not in the repos map loaded cleanly")
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "acme/typo") || !strings.Contains(joined, "acme/app") {
		t.Fatalf("error names neither the bad default nor the known repos:\n%s", joined)
	}

	// The same workflow with a real key is fine.
	write("workflows/wf.yaml", `name: wf
trigger: {kind: manual}
inputs:
  repo: {type: string, default: acme/app}
start: done
states:
  done: {terminal: true}
`)
	if _, errs := Load(dir); len(errs) != 0 {
		t.Fatalf("a known repo default was refused: %v", errs)
	}
}
