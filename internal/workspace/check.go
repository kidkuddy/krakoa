package workspace

import (
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// RunCheck probes one prerequisite. It lives here, not in the CLI, because
// three consumers need the identical verdict: krakoactl doctor, the engine's
// `requires` admission, and the engine's unblock sweep.
func RunCheck(dc DoctorCheck) (bool, string) {
	if dc.URL != "" {
		return httpOK(dc.URL)
	}
	out, err := runTimeout(15*time.Second, "sh", "-c", dc.Command)
	detail := firstLine(out)
	if err != nil {
		return false, detail + " (" + err.Error() + ")"
	}
	lower := strings.ToLower(out)
	// CLIs that exit 0 on a dead token: the failure lives in the output.
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

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
