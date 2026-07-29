package contact

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidkuddy/krakoa/internal/core"
)

type nifftyFake struct {
	srv       *httptest.Server
	openCalls int
	events    []string // thread ts each event was posted to
	sends     []map[string]string
	openFails bool
	noSession bool
}

func newNifftyFake(t *testing.T) *nifftyFake {
	t.Helper()
	f := &nifftyFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", func(w http.ResponseWriter, r *http.Request) {
		f.openCalls++
		if f.openFails {
			http.Error(w, `{"ok":false,"error":"runner inert"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"thread_ts":"1785.0001","channel":"D1"}`))
	})
	mux.HandleFunc("POST /tasks/{ts}/event", func(w http.ResponseWriter, r *http.Request) {
		if f.noSession {
			http.Error(w, "no session", http.StatusNotFound)
			return
		}
		f.events = append(f.events, r.PathValue("ts"))
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.sends = append(f.sends, body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *nifftyFake) channel(bound string, bind func(runID, kind, value string)) *Niffty {
	n := NewNiffty(f.srv.URL, "owner@example.com")
	n.ThreadTS = func(string) string { return bound }
	n.SaveRef = bind
	return n
}

func gate() *core.Gate {
	return &core.Gate{ID: "g-1", RunID: "callab-scrum-1", Kind: core.GateQuestion, State: "deciding", Payload: "approve?"}
}

// Scheduled work has no thread bound by a human. Rather than a DM under no
// thread — which no session owns and no reply can reach — the delivery opens
// one and binds it back, so the next event lands in the same place.
func TestUnboundRunOpensAndBindsThread(t *testing.T) {
	f := newNifftyFake(t)
	var bound []string
	n := f.channel("", func(runID, kind, value string) {
		bound = append(bound, runID+" "+kind+"="+value)
	})

	if err := n.Deliver(gate()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if f.openCalls != 1 {
		t.Fatalf("opened %d threads, want 1", f.openCalls)
	}
	if len(f.events) != 1 || f.events[0] != "1785.0001" {
		t.Fatalf("events posted to %v, want the opened thread", f.events)
	}
	if len(f.sends) != 0 {
		t.Fatalf("fell back to a raw DM anyway: %v", f.sends)
	}
	want := "callab-scrum-1 slack_ts=1785.0001"
	if len(bound) != 1 || bound[0] != want {
		t.Fatalf("bound = %v, want [%q]", bound, want)
	}
}

// A run that already has a thread must reuse it — opening a second one would
// split one piece of work across two threads.
func TestBoundRunNeverOpensAnotherThread(t *testing.T) {
	f := newNifftyFake(t)
	n := f.channel("1700.0009", func(string, string, string) {})

	if err := n.Deliver(gate()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if f.openCalls != 0 {
		t.Fatalf("opened %d threads for an already-bound run", f.openCalls)
	}
	if len(f.events) != 1 || f.events[0] != "1700.0009" {
		t.Fatalf("events posted to %v, want the bound thread", f.events)
	}
}

// Opening is best-effort: if niffty cannot own a thread, the event still has to
// arrive, so it falls back to the raw DM it used before.
func TestOpenFailureFallsBackToRawDM(t *testing.T) {
	f := newNifftyFake(t)
	f.openFails = true
	var logged int
	n := f.channel("", func(string, string, string) {})
	n.Log = func(string, ...any) { logged++ }

	if err := n.Deliver(gate()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(f.sends) != 1 {
		t.Fatalf("sends = %v, want one raw DM", f.sends)
	}
	if f.sends[0]["thread_ts"] != "" {
		t.Fatalf("raw fallback claimed a thread: %q", f.sends[0]["thread_ts"])
	}
	if logged == 0 {
		t.Error("a silent fallback leaves nothing to debug")
	}
}

// Without a way to bind the thread back, opening one per event would spam a new
// thread every time — so it must not open at all.
func TestNoBinderMeansNoOpen(t *testing.T) {
	f := newNifftyFake(t)
	n := NewNiffty(f.srv.URL, "owner@example.com")
	n.ThreadTS = func(string) string { return "" }

	if err := n.Deliver(gate()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if f.openCalls != 0 {
		t.Fatalf("opened %d threads with nowhere to record them", f.openCalls)
	}
	if len(f.sends) != 1 {
		t.Fatalf("sends = %v, want one raw DM", f.sends)
	}
}

// A notice with no run cannot own a thread: there is nothing to bind it to.
func TestRunlessNoticeStaysARawDM(t *testing.T) {
	f := newNifftyFake(t)
	n := f.channel("", func(string, string, string) {})

	if err := n.Notify(&core.Notice{ID: "n-1", Kind: core.NoticeBlocked, Text: "multica-auth is failing"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if f.openCalls != 0 {
		t.Fatalf("opened %d threads for a run-less notice", f.openCalls)
	}
	if len(f.sends) != 1 {
		t.Fatalf("sends = %v, want one raw DM", f.sends)
	}
}

// The opened thread's session going missing must still land the event in that
// thread, not top-level.
func TestOpenedThreadWithoutSessionRelaysIntoTheThread(t *testing.T) {
	f := newNifftyFake(t)
	f.noSession = true
	n := f.channel("", func(string, string, string) {})

	if err := n.Deliver(gate()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(f.sends) != 1 {
		t.Fatalf("sends = %v, want one relayed DM", f.sends)
	}
	if f.sends[0]["thread_ts"] != "1785.0001" {
		t.Fatalf("relayed to thread %q, want the opened thread", f.sends[0]["thread_ts"])
	}
}

// The ANSWER-block ritual — "fill the blocks in the canvas, then reply done" —
// was completed zero times out of roughly ten in a week. Every one was answered
// in the thread after the session retyped the questions there, and twice the
// canvas never arrived at all. Short questions go inline.
func TestShortQuestionsRenderInline(t *testing.T) {
	g := &core.Gate{
		ID: "g-1", RunID: "r-1", Kind: core.GateQuestion,
		Payload:   "questions on the phone-number gate",
		Questions: []string{"steal a number off another campaign, or only a free one?", "extract the provisioning flow, or a narrower path?"},
	}
	got := gateText(g, "")

	for _, want := range []string{
		"questions on the phone-number gate",
		"1. steal a number off another campaign",
		"2. extract the provisioning flow",
		"reply in this thread, numbered.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("gate text missing %q:\n%s", want, got)
		}
	}
	// Nothing may send the human to a terminal: that instruction is why forty
	// gates in a week were answered by typing an option and spending a turn.
	if strings.Contains(got, "krakoactl") {
		t.Errorf("gate text still tells the human to run a CLI:\n%s", got)
	}
	if strings.Contains(got, "ANSWER") || strings.Contains(got, "reply done") {
		t.Errorf("gate text still asks for the canvas ritual:\n%s", got)
	}
}

// A canvas is a place to go, not a thing to read, so the trip is only worth
// making when the questions genuinely do not fit a message.
func TestCanvasOnlyForLongQuestionBlocks(t *testing.T) {
	short := &core.Gate{Kind: core.GateQuestion, Payload: "three small calls", Questions: []string{"a?", "b?"}}
	if questionsLen(short) > canvasThreshold {
		t.Fatalf("a two-line gate would have spent a canvas (%d chars)", questionsLen(short))
	}

	long := &core.Gate{Kind: core.GateQuestion, Payload: strings.Repeat("x", 400)}
	for i := 0; i < 6; i++ {
		long.Questions = append(long.Questions, strings.Repeat("q", 120))
	}
	if questionsLen(long) <= canvasThreshold {
		t.Fatalf("a six-question gate should earn its canvas (%d chars)", questionsLen(long))
	}

	// When one is made, it is offered as detail — never as the place to answer.
	got := gateText(long, "https://slack/docs/CANVAS")
	if !strings.Contains(got, "full detail: https://slack/docs/CANVAS") {
		t.Errorf("canvas not offered as detail:\n%s", got)
	}
	if !strings.Contains(got, "reply in this thread") {
		t.Errorf("long gate stopped saying where to answer:\n%s", got)
	}
}
