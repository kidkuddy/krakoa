// Package contact delivers gates to the human. Responses come back through
// krakoactl answer (the store enforces first-response-wins), so channels are
// delivery-only.
package contact

import (
	"bytes"
	"encoding/json"
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

// Niffty relays gates to the user's own Slack DM via the local niffty
// daemon. With a thread binding, messages land in the task's Slack thread;
// question gates become editable canvases (one ANSWER: block per question)
// harvested later by `krakoactl harvest`.
type Niffty struct {
	URL  string // e.g. http://127.0.0.1:7777
	To   string // relay recipient email
	Bin  string // niffty CLI (canvas create); empty = plain messages only
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

func (n *Niffty) Deliver(g *core.Gate) error {
	var text string
	if g.Kind == core.GateQuestion && n.Bin != "" && len(g.Questions) > 0 {
		if link, err := n.createCanvas(g); err == nil {
			if n.SaveRef != nil {
				n.SaveRef(g.RunID, "canvas:"+g.ID, link)
			}
			text = fmt.Sprintf("⏸ YOUR TURN — questions on %s. Fill the ANSWER blocks in the canvas, then reply done (or run: krakoactl harvest %s)\n%s", g.RunID, g.ID, link)
		}
	}
	if text == "" {
		text = fmt.Sprintf("⏸ YOUR TURN — [krakoa gate %s] %s", g.ID, g.Payload)
		if len(g.Options) > 0 {
			text += "\noptions: " + strings.Join(g.Options, " | ")
		}
		text += fmt.Sprintf("\nanswer: krakoactl answer %s <response>", g.ID)
	}
	payload := map[string]string{"to": n.To, "text": text}
	if n.ThreadTS != nil {
		if ts := n.ThreadTS(g.RunID); ts != "" {
			payload["thread_ts"] = ts
		}
	}
	body, _ := json.Marshal(payload)
	resp, err := n.HTTP.Post(n.URL+"/send", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("niffty down: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("niffty /send: %s: %s", resp.Status, b)
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
