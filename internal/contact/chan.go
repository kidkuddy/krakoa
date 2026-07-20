package contact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kidkuddy/krakoa/internal/core"
)

// Chan is the ambient notifier (F11): a 1-2 line attention ping to a local
// avatar server — no details, no UI opening, always /ping (a daemon has no
// "immediate speech" context). Disconnected app is fine (pings queue
// server-side); a fully unreachable server after one headless start attempt
// skips silently. Ambient means Deliver NEVER returns an error — this
// channel can't raise delivery-failure noise.
type Chan struct {
	URL      string // e.g. http://localhost:8765
	StartCmd string // optional headless start attempt (sh -c), tried once
	HTTP     *http.Client

	startOnce sync.Once
}

func NewChan(url, startCmd string) *Chan {
	return &Chan{URL: url, StartCmd: startCmd, HTTP: &http.Client{Timeout: 5 * time.Second}}
}

func (c *Chan) Name() string { return "chan" }

func (c *Chan) Deliver(g *core.Gate) error {
	// severity → phrasing; the avatar speaks Japanese, the subtitle is the
	// English attention line. Details live in niffty/UI, never here.
	subject := g.RunID
	if subject == "" {
		subject = g.State
	}
	var text, subtitle string
	switch {
	case strings.HasPrefix(g.Payload, "needs attention"):
		text = "マスター、ちょっと問題があったみたい…見てくれる？"
		subtitle = fmt.Sprintf("Something broke on %s — needs you", subject)
	case g.Kind == core.GateQuestion:
		text = "マスター、聞きたいことがあるよ！"
		subtitle = fmt.Sprintf("Questions waiting on %s", subject)
	default:
		text = "マスター、決めてほしいことがあるよ！"
		subtitle = fmt.Sprintf("Decision waiting on %s", subject)
	}

	body, _ := json.Marshal(map[string]string{"text": text, "subtitle": subtitle})
	if err := c.post(body); err != nil {
		// one headless start attempt, then one retry; still down → silent
		c.startOnce.Do(func() {
			if c.StartCmd != "" {
				exec.Command("sh", "-c", c.StartCmd).Start()
				time.Sleep(3 * time.Second)
			}
		})
		c.post(body)
	}
	return nil // ambient: never blocking, never a delivery failure
}

func (c *Chan) post(body []byte) error {
	resp, err := c.HTTP.Post(c.URL+"/ping", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("chan: %s", resp.Status)
	}
	return nil
}
