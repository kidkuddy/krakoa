package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kidkuddy/krakoa/internal/runner"
	"github.com/kidkuddy/krakoa/internal/workspace"
)

// cmdDoctor checks every prerequisite for live runs. Read-only; each check
// prints ok/FAIL plus the fix. Exit 1 if anything failed.
func cmdDoctor() error {
	failed := 0
	check := func(name string, ok bool, detail, fix string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("[%s] %-22s %s\n", mark, name, detail)
		if !ok && fix != "" {
			fmt.Printf("       fix: %s\n", fix)
		}
	}
	httpOK := func(url string) (bool, string) {
		c := &http.Client{Timeout: 3 * time.Second}
		resp, err := c.Get(url)
		if err != nil {
			return false, err.Error()
		}
		defer resp.Body.Close()
		return resp.StatusCode < 300, resp.Status
	}
	run := func(timeout time.Duration, name string, args ...string) (string, error) {
		cmd := exec.Command(name, args...)
		done := make(chan struct{})
		var out []byte
		var err error
		go func() { out, err = cmd.CombinedOutput(); close(done) }()
		select {
		case <-done:
			return strings.TrimSpace(string(out)), err
		case <-time.After(timeout):
			cmd.Process.Kill()
			return "", fmt.Errorf("timed out after %s", timeout)
		}
	}

	// claude binary
	bin, err := runner.ResolveBin()
	check("claude binary", err == nil, bin, "install claude or set KRAKOA_CLAUDE_BIN")

	// workspaces
	wsPaths := os.Getenv("KRAKOA_WORKSPACES")
	if wsPaths == "" {
		check("KRAKOA_WORKSPACES", false, "not set", "export KRAKOA_WORKSPACES=<path-to-workspace-dir>[,...]")
	} else {
		for _, p := range strings.Split(wsPaths, ",") {
			p = strings.TrimSpace(p)
			ws, errs := workspace.Load(p)
			name := p
			if ws != nil && ws.Name != "" {
				name = ws.Name
			}
			check("workspace "+name, len(errs) == 0, fmt.Sprintf("%d error(s)", len(errs)), "krakoactl workspace validate "+p)
		}
	}

	// krakoad
	ok, detail := httpOK(addr() + "/healthz")
	check("krakoad", ok, detail, "start krakoad (launchctl load ~/Library/LaunchAgents/com.krakoa.krakoad.plist, or run bin/krakoad)")

	// niffty
	nifftyURL := os.Getenv("KRAKOA_NIFFTY_URL")
	if nifftyURL == "" {
		nifftyURL = "http://127.0.0.1:7777"
	}
	ok, detail = httpOK(nifftyURL + "/healthz")
	check("niffty daemon", ok, detail, "launchctl load ~/Library/LaunchAgents/com.krakoa.niffty.plist (or: cd ~/Desktop/work/clusterlab/niffty && make serve)")

	// multica auth reachable (CF Access proxy up)
	out, err := run(10*time.Second, "multica", "--profile", "callab", "auth", "status")
	lowerAuth := strings.ToLower(out)
	authOK := err == nil
	for _, bad := range []string{"not logged", "invalid", "expired", "401", "error"} {
		if strings.Contains(lowerAuth, bad) {
			authOK = false
		}
	}
	check("multica auth (callab)", authOK, firstLine(out), "run /callab-init in a clusterlab session to bring up the CF Access proxy and re-auth")

	// multica LOCAL daemon must be OFF (the builder runs on the remote VM;
	// a local daemon would steal assigned runs)
	out, _ = run(10*time.Second, "multica", "--profile", "callab", "daemon", "status")
	lower := strings.ToLower(out)
	daemonOff := strings.Contains(lower, "not running") || strings.Contains(lower, "stopped") || strings.Contains(lower, "no daemon")
	check("multica local daemon OFF", daemonOff, firstLine(out), "multica --profile callab daemon stop")

	// glab auth
	out, err = run(10*time.Second, "glab", "auth", "status")
	check("glab auth", err == nil, firstLine(out), "glab auth login")

	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	fmt.Println("\nall checks passed — ready for live runs")
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
