package engine

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kidkuddy/krakoa/internal/workspace"
)

func TestDryRunBothWorkflows(t *testing.T) {
	ws, errs := workspace.Load("testdata/demo")
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, wf := range []string{"task-lifecycle", "review-sweeper"} {
		var out bytes.Buffer
		if err := DryRun(ws, wf, &out); err != nil {
			t.Errorf("%s: %v\n%s", wf, err, out.String())
			continue
		}
		if !strings.Contains(out.String(), "dry-run OK") {
			t.Errorf("%s: no OK verdict:\n%s", wf, out.String())
		}
	}
}
