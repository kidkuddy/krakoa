package main

import (
	"fmt"
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

	// Spawned sessions (Slack agent mode) get a clean PATH with no shell
	// profile — krakoactl must resolve there too, or tasks can't start.
	cleanPath := "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
	found := ""
	for _, dir := range strings.Split(cleanPath, ":") {
		p := dir + "/krakoactl"
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			found = p
			break
		}
	}
	check("krakoactl on clean PATH", found != "", found, "make install (symlinks into /opt/homebrew/bin)")

	// The daemon owns workspaces: prefer its inventory (and its declared
	// doctor checks). KRAKOA_WORKSPACES is only a fallback for a client
	// shell when krakoad is down.
	type wsInfo struct {
		Name       string
		Path       string
		GitVersion string
		LoadedAt   time.Time
		Doctor     []workspace.DoctorCheck
	}
	var daemonWS []wsInfo
	daemonUp := call("GET", "/v1/workspaces", nil, &daemonWS) == nil
	if !daemonUp {
		if ok, _ := workspace.RunCheck(workspace.DoctorCheck{URL: addr() + "/healthz"}); ok {
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
			// krakoad re-reads its workspaces when their files move, but not
			// while an agent step is running — an edit landing under a live
			// run is the case the deferral exists for. Until it lands, the
			// runtime is serving something other than what is on disk, and
			// nothing else in this output would say so.
			detail := fmt.Sprintf("loaded by krakoad (working tree at git %s)", ws.GitVersion)
			if !ws.LoadedAt.IsZero() {
				detail = fmt.Sprintf("loaded by krakoad %s (working tree at git %s)",
					ws.LoadedAt.Format("Jan 2 15:04"), ws.GitVersion)
			}
			if n := workspace.ChangedSince(ws.Path, ws.LoadedAt); n > 0 {
				check("workspace "+ws.Name, false,
					fmt.Sprintf("%s — %d file(s) changed on disk since", detail, n),
					"krakoad reloads within ~10s once no agent step is running; `krakoactl runs` shows what is holding it")
			} else {
				check("workspace "+ws.Name, true, detail, "")
			}
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

	// Prefer the daemon's own verdicts: it probes on its launchd PATH, which is
	// the PATH the runs actually use. A client shell without /opt/homebrew/bin
	// reported four checks failed that were all fine — a false alarm is worse
	// than no alarm, because it sends you fixing what is not broken.
	live := map[string]struct {
		ok     bool
		detail string
	}{}
	if daemonUp {
		var board []struct {
			Workspace, Name, Detail string
			OK, Probed              bool
		}
		if call("GET", "/v1/checks", nil, &board) == nil {
			for _, r := range board {
				if r.Probed {
					live[r.Workspace+"/"+r.Name] = struct {
						ok     bool
						detail string
					}{r.OK, r.Detail}
				}
			}
		}
	}
	for _, c := range checks {
		if v, ok := live[c.ws+"/"+c.dc.Name]; ok {
			check(c.ws+": "+c.dc.Name, v.ok, v.detail+"  (as krakoad sees it)", c.dc.Fix)
			continue
		}
		ok, detail := workspace.RunCheck(c.dc)
		check(c.ws+": "+c.dc.Name, ok, detail, c.dc.Fix)
	}

	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	fmt.Println("\nall checks passed — ready for live runs")
	return nil
}
