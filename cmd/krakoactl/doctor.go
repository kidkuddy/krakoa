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

// Set via -ldflags by make build.
var (
	buildCommit = "unknown"
	buildRepo   = ""
)

// cmdDoctor checks live-run prerequisites: the generic engine checks
// (binary freshness, claude bin, workspaces load, krakoad up) plus every
// check the loaded workspaces declare in their workspace.yaml doctor:
// section. Environment specifics never live here.
func cmdDoctor() error {
	failed := 0
	check := func(name string, ok bool, detail, fix string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("[%s] %-26s %s\n", mark, name, detail)
		if !ok && fix != "" {
			fmt.Printf("       fix: %s\n", fix)
		}
	}

	// build freshness: a stale installed binary silently masks fixes
	fmt.Printf("krakoactl build %s\n", buildCommit)
	if buildRepo != "" {
		if out, err := exec.Command("git", "-C", buildRepo, "rev-parse", "--short", "HEAD").Output(); err == nil {
			head := strings.TrimSpace(string(out))
			check("binary matches repo HEAD", head == buildCommit,
				fmt.Sprintf("binary %s, repo %s", buildCommit, head), "make install (in "+buildRepo+")")
		}
	}

	bin, err := runner.ResolveBin()
	check("claude binary", err == nil, bin, "install claude or set KRAKOA_CLAUDE_BIN")

	// The daemon owns workspaces: prefer its inventory (and its declared
	// doctor checks). KRAKOA_WORKSPACES is only a fallback for a client
	// shell when krakoad is down.
	type wsInfo struct {
		Name       string
		Path       string
		GitVersion string
		Doctor     []workspace.DoctorCheck
	}
	var daemonWS []wsInfo
	daemonUp := call("GET", "/v1/workspaces", nil, &daemonWS) == nil
	if !daemonUp {
		if ok, _ := httpOK(addr() + "/healthz"); ok {
			// alive but predates /v1/workspaces — an older binary
			check("krakoad", false, "running but stale (no /v1/workspaces)",
				"restart krakoad once no agent step is mid-flight (waiting/gated runs recover cleanly)")
		} else {
			check("krakoad", false, addr(), "start krakoad (launchctl or bin/krakoad)")
		}
	} else {
		check("krakoad", true, addr(), "")
	}

	var checks []struct {
		ws string
		dc workspace.DoctorCheck
	}
	if daemonUp {
		for _, ws := range daemonWS {
			check("workspace "+ws.Name, true, fmt.Sprintf("loaded by krakoad (git %s)", ws.GitVersion), "")
			for _, dc := range ws.Doctor {
				checks = append(checks, struct {
					ws string
					dc workspace.DoctorCheck
				}{ws.Name, dc})
			}
		}
	} else {
		wsPaths := os.Getenv("KRAKOA_WORKSPACES")
		if wsPaths == "" {
			check("KRAKOA_WORKSPACES", false, "not set (and krakoad is down — nothing to check workspaces against)",
				"start krakoad, or export KRAKOA_WORKSPACES=<path>[,...] for offline validation")
		}
		for _, p := range strings.Split(wsPaths, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			ws, errs := workspace.Load(p)
			name := p
			if ws != nil && ws.Name != "" {
				name = ws.Name
			}
			check("workspace "+name, len(errs) == 0, fmt.Sprintf("%d error(s)", len(errs)), "krakoactl workspace validate "+p)
			if len(errs) == 0 {
				for _, dc := range ws.Doctor {
					checks = append(checks, struct {
						ws string
						dc workspace.DoctorCheck
					}{ws.Name, dc})
				}
			}
		}
	}

	for _, c := range checks {
		ok, detail := runDoctorCheck(c.dc)
		check(c.ws+": "+c.dc.Name, ok, detail, c.dc.Fix)
	}

	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	fmt.Println("\nall checks passed — ready for live runs")
	return nil
}

func runDoctorCheck(dc workspace.DoctorCheck) (bool, string) {
	if dc.URL != "" {
		return httpOK(dc.URL)
	}
	out, err := runTimeout(15*time.Second, "sh", "-c", dc.Command)
	detail := firstLine(out)
	if err != nil {
		return false, detail + " (" + err.Error() + ")"
	}
	lower := strings.ToLower(out)
	for _, bad := range dc.Fail {
		if strings.Contains(lower, strings.ToLower(bad)) {
			return false, detail
		}
	}
	if dc.Expect != "" && !strings.Contains(lower, strings.ToLower(dc.Expect)) {
		return false, detail + fmt.Sprintf(" (missing %q)", dc.Expect)
	}
	return true, detail
}

func httpOK(url string) (bool, string) {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	return resp.StatusCode < 300, resp.Status
}

func runTimeout(timeout time.Duration, name string, args ...string) (string, error) {
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
