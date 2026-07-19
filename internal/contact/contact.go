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

// Niffty relays the gate to the user's own Slack DM via the local niffty
// daemon (POST /send, always to the fixed relay recipient).
type Niffty struct {
	URL  string // e.g. http://127.0.0.1:7777
	To   string // relay recipient email
	HTTP *http.Client
}

func NewNiffty(url, to string) *Niffty {
	return &Niffty{URL: url, To: to, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (n *Niffty) Name() string { return "niffty" }

func (n *Niffty) Deliver(g *core.Gate) error {
	text := fmt.Sprintf("[krakoa gate %s] %s", g.ID, g.Payload)
	if len(g.Options) > 0 {
		text += "\noptions: " + strings.Join(g.Options, " | ")
	}
	text += fmt.Sprintf("\nanswer: krakoactl answer %s <response>", g.ID)
	body, _ := json.Marshal(map[string]string{"to": n.To, "text": text})
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
