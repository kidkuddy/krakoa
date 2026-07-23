package contact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
)

// Script is a workspace-declared contact channel: a script in the workspace
// gets the gate or notice as JSON on stdin and reports delivery on stdout.
//
// This is the same shape command probes and watchers already have, and it is
// what keeps the engine use-case blind. A chat tool is environment, exactly
// like a ticket tracker or a git host, and environment lives in the workspace.
// The engine knows "exec a command, JSON in, JSON out" and nothing else — not
// what a thread is, not what "momo_root" means, not that Matrix exists.
type Script struct {
	ChannelName string        // as declared in workspace.yaml
	Workspace   string        // the only workspace it serves
	Dir         string        // workspace root; the command is relative to it
	Command     string        // e.g. "scripts/notify-momo"
	Timeout     time.Duration // 0 = ScriptTimeout

	// Refs returns every contact binding on the run's thread, and SaveRef
	// stores one back. Both are opaque string maps: the engine passes them
	// through so a script can thread its own conversation without the engine
	// learning what any key means.
	Refs    func(runID string) map[string]string
	SaveRef func(runID, kind, value string)
}

// ScriptTimeout bounds one delivery. A chat API that hangs must not wedge the
// delivery queue; a failed send is retried by the delivery sweep anyway.
const ScriptTimeout = 30 * time.Second

func (s *Script) Name() string { return s.ChannelName }

// Serves: a declared channel carries its own workspace and nothing else. This
// is why scoping needs no configuration — declaration IS the scope.
func (s *Script) Serves(ws string) bool { return ws == s.Workspace }

// message is the contact contract. Everything a channel could need to render
// a human-facing message, and nothing about how it should.
type message struct {
	Kind      string            `json:"kind"` // gate | notice
	ID        string            `json:"id"`
	Workspace string            `json:"workspace"`
	Run       string            `json:"run,omitempty"`
	State     string            `json:"state,omitempty"`
	Text      string            `json:"text"`
	GateKind  string            `json:"gate_kind,omitempty"`
	Options   []string          `json:"options,omitempty"`
	Questions []string          `json:"questions,omitempty"`
	Severity  string            `json:"severity,omitempty"` // notice kind: progress|done|stuck|blocked|unblocked
	Refs      map[string]string `json:"refs,omitempty"`
}

// reply is what the script prints: delivery status plus any thread bindings
// it wants remembered for next time.
type reply struct {
	OK    bool              `json:"ok"`
	Error string            `json:"error,omitempty"`
	Refs  map[string]string `json:"refs,omitempty"`
}

func (s *Script) Deliver(g *core.Gate) error {
	return s.send(message{
		Kind: "gate", ID: g.ID, Workspace: g.Workspace, Run: g.RunID, State: g.State,
		GateKind: string(g.Kind), Text: gateText(g, ""),
		Options:  g.Options, Questions: g.Questions,
		Refs: s.refs(g.RunID),
	})
}

func (s *Script) Notify(n *core.Notice) error {
	return s.send(message{
		Kind: "notice", ID: n.ID, Workspace: n.Workspace, Run: n.RunID, State: n.State,
		Text: n.Text, Severity: string(n.Kind),
		Refs: s.refs(n.RunID),
	})
}

func (s *Script) refs(runID string) map[string]string {
	if runID == "" || s.Refs == nil {
		return nil
	}
	return s.Refs(runID)
}

func (s *Script) send(m message) error {
	in, err := json.Marshal(m)
	if err != nil {
		return err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = ScriptTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	tok := strings.Fields(s.Command)
	cmd := exec.CommandContext(ctx, tok[0], tok[1:]...)
	cmd.Dir = s.Dir
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			return fmt.Errorf("%s: timed out after %s: %.200s", s.ChannelName, timeout, detail)
		}
		return fmt.Errorf("%s: %w: %.200s", s.ChannelName, err, detail)
	}

	// A script that says nothing but exits 0 delivered successfully; only a
	// malformed reply or an explicit failure is an error.
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return nil
	}
	var r reply
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return fmt.Errorf("%s: unparseable reply %.200q", s.ChannelName, out)
	}
	if !r.OK {
		if r.Error == "" {
			r.Error = "delivery reported not ok"
		}
		return fmt.Errorf("%s: %s", s.ChannelName, r.Error)
	}
	if m.Run != "" && s.SaveRef != nil {
		for k, v := range r.Refs {
			if v != "" {
				s.SaveRef(m.Run, k, v)
			}
		}
	}
	return nil
}
