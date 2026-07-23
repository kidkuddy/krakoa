package contact

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/kidkuddy/krakoa/internal/core"
)

// Momo relays gates and notices to the user's Matrix DM through the local
// momo CLI. A run's first message opens a momo thread (posted and pinned);
// every later message for that run lands inside it, so one piece of work
// stays one conversation — the same shape the niffty channel gives Slack.
//
// Deliberately NOT used: momo's `--kind`, `--wip` and `nudge`. Krakoa already
// owns nagging and escalation (delivery.go) and already refuses duplicate work
// (unique + skip_if_running + concurrency). A kind would hand the same two jobs
// to momo as well, and two systems nagging about one gate is the wallpaper
// effect both were built to avoid.
type Momo struct {
	Bin     string // absolute: a launchd PATH will not contain ~/.local/bin
	Profile string
	// Room is the Matrix room to post in. Empty means momo picks the DM with
	// the user it obeys — which works for opening threads but leaves replies
	// top-level, since `momo send` needs a room to thread into.
	Room string
	Only scope // workspaces served; empty = all

	// ThreadRoot resolves a run's bound momo thread root (empty = none yet).
	ThreadRoot func(runID string) string
	// SaveRef binds the thread root to the run's thread.
	SaveRef func(runID, kind, value string)
	Log     func(format string, a ...any)
}

func (m *Momo) Name() string { return "momo" }

func (m *Momo) Serves(ws string) bool { return m.Only.Serves(ws) }

func (m *Momo) Deliver(g *core.Gate) error {
	return m.post(g.RunID, gateText(g, ""))
}

func (m *Momo) Notify(n *core.Notice) error {
	return m.post(n.RunID, n.Text)
}

// post sends into the run's thread, opening one if this is the run's first
// message. Engine-level messages (no run) always open their own thread: there
// is no piece of work to attach them to.
func (m *Momo) post(runID, text string) error {
	if root := m.rootFor(runID); root != "" && m.Room != "" {
		err := m.run("send", m.Room, text, "--thread", root)
		if err == nil {
			return nil
		}
		// The thread is gone (redacted room, wiped history): say so and open a
		// new one rather than dropping the message.
		m.logf("momo send into %s failed, opening a new thread: %v", root, err)
	}
	args := []string{"start"}
	if m.Room != "" {
		args = append(args, m.Room)
	}
	args = append(args, "--message", text, "--no-agent")
	out, err := m.output(args...)
	if err != nil {
		return err
	}
	root := firstLine(out)
	if root == "" {
		return fmt.Errorf("momo start: no thread root in output %.200q", out)
	}
	if runID != "" && m.SaveRef != nil {
		m.SaveRef(runID, "momo_root", root)
	}
	return nil
}

func (m *Momo) rootFor(runID string) string {
	if runID == "" || m.ThreadRoot == nil {
		return ""
	}
	return m.ThreadRoot(runID)
}

func (m *Momo) run(args ...string) error {
	_, err := m.output(args...)
	return err
}

func (m *Momo) output(args ...string) (string, error) {
	cmd := exec.Command(m.Bin, append([]string{"--profile", m.Profile}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("momo %s: %w: %.200s", args[0], err, out)
	}
	return string(out), nil
}

func (m *Momo) logf(format string, a ...any) {
	if m.Log != nil {
		m.Log(format, a...)
	}
}

// firstLine is momo start's contract: the thread root event id, alone on the
// first line. (A `--wip` skip prints a "skipped:" line first — we never pass
// --wip, so a first line that is not an event id is a real parse failure.)
func firstLine(s string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
	if !strings.HasPrefix(line, "$") {
		return ""
	}
	return line
}
