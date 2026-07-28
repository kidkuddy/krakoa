package contact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kidkuddy/krakoa/internal/core"
)

// writeScript drops an executable shell script in a temp dir.
func writeScript(t *testing.T, body string) (dir, name string) {
	t.Helper()
	dir = t.TempDir()
	name = "notify"
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, "./" + name
}

// The whole contract in one pass: the message reaches the script as JSON, the
// refs it returns are saved, and a second delivery carries them back — which
// is how a channel threads a conversation without the engine knowing what a
// thread is.
func TestScriptDeliversAndRoundTripsRefs(t *testing.T) {
	dir, cmd := writeScript(t, `
cat > "$0.seen"
echo '{"ok":true,"refs":{"thread_root":"$root-1"}}'
`)
	saved := map[string]string{}
	s := &Script{
		ChannelName: "test", Workspace: "personal", Dir: dir, Command: cmd,
		Refs:    func(runID string) map[string]string { return saved },
		SaveRef: func(runID, kind, value string) { saved[kind] = value },
	}

	if !s.Serves("personal") || s.Serves("callab") {
		t.Fatal("a declared channel must serve exactly its own workspace")
	}

	g := &core.Gate{
		ID: "g-1", Workspace: "personal", RunID: "run-1", State: "pinging",
		Kind: core.GateChoice, Payload: "inbox time", Options: []string{"ok"},
	}
	if err := s.Deliver(g); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if saved["thread_root"] != "$root-1" {
		t.Fatalf("ref not saved: %v", saved)
	}

	seen, err := os.ReadFile(filepath.Join(dir, "notify.seen"))
	if err != nil {
		t.Fatal(err)
	}
	var got message
	if err := json.Unmarshal(seen, &got); err != nil {
		t.Fatalf("script got unparseable stdin %q: %v", seen, err)
	}
	if got.Kind != "gate" || got.ID != "g-1" || got.Run != "run-1" {
		t.Fatalf("wrong message: %+v", got)
	}
	if len(got.Options) != 1 || got.Options[0] != "ok" {
		t.Fatalf("options lost: %+v", got)
	}

	// Second delivery: the saved ref rides back in.
	if err := s.Notify(&core.Notice{ID: "n-1", Workspace: "personal", RunID: "run-1", Kind: core.NoticeProgress, Text: "still open"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	seen, _ = os.ReadFile(filepath.Join(dir, "notify.seen"))
	got = message{}
	json.Unmarshal(seen, &got)
	if got.Refs["thread_root"] != "$root-1" {
		t.Fatalf("refs not handed back: %+v", got.Refs)
	}
}

// A channel that cannot deliver must SAY so: the engine's retry and the
// "⚠ not delivered" surface both depend on an error coming back.
func TestScriptReportsFailure(t *testing.T) {
	dir, cmd := writeScript(t, `echo '{"ok":false,"error":"matrix unreachable"}'`)
	s := &Script{ChannelName: "test", Workspace: "personal", Dir: dir, Command: cmd}
	err := s.Deliver(&core.Gate{ID: "g-2", Workspace: "personal", Payload: "x"})
	if err == nil {
		t.Fatal("a not-ok reply must be an error")
	}

	dir, cmd = writeScript(t, `exit 3`)
	s = &Script{ChannelName: "test", Workspace: "personal", Dir: dir, Command: cmd}
	if err := s.Deliver(&core.Gate{ID: "g-3", Workspace: "personal", Payload: "x"}); err == nil {
		t.Fatal("a non-zero exit must be an error")
	}

	// Silence with exit 0 is success — not every channel has something to say.
	dir, cmd = writeScript(t, `exit 0`)
	s = &Script{ChannelName: "test", Workspace: "personal", Dir: dir, Command: cmd}
	if err := s.Deliver(&core.Gate{ID: "g-4", Workspace: "personal", Payload: "x"}); err != nil {
		t.Fatalf("silent success must not error: %v", err)
	}
}
