// Package contact delivers gates to the human. Responses come back through
// krakoactl answer (the store enforces first-response-wins), so channels are
// delivery-only.
package contact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
)

type Channel interface {
	Name() string
	Deliver(g *core.Gate) error
	// Notify delivers a one-way message (no response expected).
	Notify(n *core.Notice) error
}

// Scoped is implemented by channels that serve only some workspaces — work
// pings Slack, personal work pings Matrix. A channel that does not implement
// it serves every workspace (the console, which is the audit trail).
type Scoped interface{ Serves(workspace string) bool }

// scope is the workspace allowlist shared by scoped channels. Empty = all,
// so an unconfigured channel keeps its old everywhere behaviour.
type scope []string

func (s scope) Serves(ws string) bool {
	if len(s) == 0 {
		return true
	}
	for _, w := range s {
		if w == ws {
			return true
		}
	}
	return false
}

// Console writes gates to the daemon log. Always configured; the audit trail
// itself is the events table, written by the engine.
type Console struct{ W io.Writer }

func (c *Console) Name() string { return "console" }

func (c *Console) Deliver(g *core.Gate) error {
	opts := ""
	if len(g.Options) > 0 {
		opts = " [" + strings.Join(g.Options, " | ") + "]"
	}
	_, err := fmt.Fprintf(c.W, "GATE %s (%s) run=%s state=%s: %s%s\n  answer with: krakoactl answer %s <response>\n",
		g.ID, g.Kind, g.RunID, g.State, g.Payload, opts, g.ID)
	return err
}

func (c *Console) Notify(n *core.Notice) error {
	_, err := fmt.Fprintf(c.W, "NOTICE %s (%s) run=%s: %s\n", n.ID, n.Kind, n.RunID, n.Text)
	return err
}

// Niffty relays gates to the user's own Slack DM via the local niffty
// daemon. With a thread binding, messages land in the task's Slack thread;
// question gates become editable canvases (one ANSWER: block per question)
// harvested later by `krakoactl harvest`.
type Niffty struct {
	URL  string // e.g. http://127.0.0.1:7777
	To   string // relay recipient email
	Bin  string // niffty CLI (canvas create); empty = plain messages only
	Only scope  // workspaces served; empty = all
	HTTP *http.Client

	// ThreadTS resolves a run's bound Slack thread (empty = top-level).
	ThreadTS func(runID string) string
	// SaveRef stores a contact ref on the run's thread (canvas permalinks).
	SaveRef func(runID, kind, value string)
}

func NewNiffty(url, to string) *Niffty {
	return &Niffty{URL: url, To: to, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (n *Niffty) Name() string { return "niffty" }

func (n *Niffty) Serves(ws string) bool { return n.Only.Serves(ws) }

// event is what niffty's /tasks/{ts}/event accepts: the thread's agent
// session renders it (one renderer, P9) and decides whether to act.
type event struct {
	Kind      string   `json:"kind"`
	EventID   string   `json:"event_id"`
	RunID     string   `json:"run_id,omitempty"`
	GateID    string   `json:"gate_id,omitempty"`
	Text      string   `json:"text"`
	Options   []string `json:"options,omitempty"`
	Questions []string `json:"questions,omitempty"`
	// Wake asks niffty to spend a turn: gate questions, done-and-deployed,
	// and stuck steps only. Routine progress is posted, never woken on.
	Wake bool `json:"wake"`
}

func (n *Niffty) Deliver(g *core.Gate) error {
	var canvas string
	if g.Kind == core.GateQuestion && n.Bin != "" && len(g.Questions) > 0 {
		if link, err := n.createCanvas(g); err == nil {
			if n.SaveRef != nil {
				n.SaveRef(g.RunID, "canvas:"+g.ID, link)
			}
			canvas = link
		}
	}
	return n.post(g.RunID, event{
		Kind: "gate", EventID: g.ID, RunID: g.RunID, GateID: g.ID,
		Text:      gateText(g, canvas),
		Options:   g.Options,
		Questions: g.Questions,
		Wake:      true,
	})
}

func (n *Niffty) Notify(no *core.Notice) error {
	return n.post(no.RunID, event{
		Kind: string(no.Kind), EventID: no.ID, RunID: no.RunID, Text: no.Text,
		Wake: no.Kind == core.NoticeDone || no.Kind == core.NoticeStuck || no.Kind == core.NoticeBlocked,
	})
}

// gateText renders a gate for a human. A question gate NEVER falls back to
// the workflow payload: that template interpolates a Go slice and arrives as
// "[Q1 …? Q2 …?]". The questions travel structurally, so they are rendered
// structurally.
func gateText(g *core.Gate, canvas string) string {
	if canvas != "" {
		return fmt.Sprintf("questions on %s — fill the ANSWER blocks in the canvas, then reply done (or run: krakoactl harvest %s)\n%s", g.RunID, g.ID, canvas)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n[%s]", g.Payload, g.ID)
	for i, q := range g.Questions {
		fmt.Fprintf(&b, "\n%d. %s", i+1, q)
	}
	if len(g.Questions) > 0 {
		b.WriteString("\nreply in this thread, numbered.")
	}
	if len(g.Options) > 0 {
		b.WriteString("\noptions: " + strings.Join(g.Options, " | "))
	}
	fmt.Fprintf(&b, "\nanswer: krakoactl answer %s <response>", g.ID)
	return b.String()
}

// post routes through the thread's agent session when the run has a Slack
// thread bound, and falls back to a raw DM otherwise (or when niffty says
// the session is gone).
func (n *Niffty) post(runID string, ev event) error {
	ts := ""
	if n.ThreadTS != nil {
		ts = n.ThreadTS(runID)
	}
	if ts != "" {
		err := n.postJSON("/tasks/"+ts+"/event", ev)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errNoSession) {
			return err
		}
		// session gone: say so, then relay raw into the same thread
		ev.Text += "\n(the thread's session is gone — relayed raw)"
	}
	body := map[string]string{"to": n.To, "text": ev.Text}
	if ts != "" {
		body["thread_ts"] = ts
	}
	return n.postJSON("/send", body)
}

// errNoSession marks "this thread has no live agent session" — a routing
// fact, not a delivery failure.
var errNoSession = errors.New("no session for thread")

func (n *Niffty) postJSON(path string, v any) error {
	body, _ := json.Marshal(v)
	resp, err := n.HTTP.Post(n.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("niffty down: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return errNoSession
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("niffty %s: %s: %s", path, resp.Status, b)
	}
	return nil
}

// createCanvas renders one section per question with a prefilled ANSWER:
// block (the deterministic harvest contract) and returns the permalink.
func (n *Niffty) createCanvas(g *core.Gate) (string, error) {
	var md strings.Builder
	fmt.Fprintf(&md, "# Krakoa needs answers — %s\n\n%s\n", g.RunID, g.Payload)
	for i, q := range g.Questions {
		fmt.Fprintf(&md, "\n## Q%d: %s\n\nANSWER: \n", i+1, q)
	}
	cmd := exec.Command(n.Bin, "canvas", fmt.Sprintf("krakoa %s (%s)", g.ID, g.RunID))
	cmd.Stdin = strings.NewReader(md.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("canvas create: %w: %.200s", err, out)
	}
	link := lastURL(string(out))
	if link == "" {
		return "", fmt.Errorf("canvas create: no permalink in output %.200q", out)
	}
	return link, nil
}

var urlRe = regexp.MustCompile(`https?://\S+`)

func lastURL(s string) string {
	m := urlRe.FindAllString(s, -1)
	if len(m) == 0 {
		return ""
	}
	return strings.TrimRight(m[len(m)-1], ">.,)")
}

// NifftyBoard projects threads onto niffty's Slack List (phase -> lane).
// Failures log-and-continue: the board is a projection, never a blocker.
type NifftyBoard struct {
	Bin     string
	GetRef  func(thread, kind string) string
	SaveRef func(thread, kind, value string)
	Log     func(format string, a ...any)
}

func (b *NifftyBoard) Upsert(thread, title, lane string) {
	id := b.GetRef(thread, "board_item")
	if id == "" {
		out, err := exec.Command(b.Bin, "list", "add", "--status", lane, title).CombinedOutput()
		if err != nil {
			b.Log("board add %s: %v: %.200s", thread, err, out)
			return
		}
		fields := strings.Fields(string(out))
		if len(fields) == 0 {
			b.Log("board add %s: unparseable output %.100q", thread, out)
			return
		}
		b.SaveRef(thread, "board_item", fields[len(fields)-1])
		return
	}
	if out, err := exec.Command(b.Bin, "list", "move", id, lane).CombinedOutput(); err != nil {
		b.Log("board move %s -> %s: %v: %.200s", thread, lane, err, out)
	}
}
