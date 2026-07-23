// krakoad is the Krakoa daemon: loads workspaces, recovers state, ticks the
// scheduler, and serves the HTTP ingress for krakoactl and external emits.
// 12-factor: all config from env, logs to stdout, supervised by launchd.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kidkuddy/krakoa/internal/contact"
	"github.com/kidkuddy/krakoa/internal/core"
	"github.com/kidkuddy/krakoa/internal/engine"
	"github.com/kidkuddy/krakoa/internal/runner"
	"github.com/kidkuddy/krakoa/internal/store"
	"github.com/kidkuddy/krakoa/internal/workspace"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitList parses a comma-separated env var into a workspace allowlist.
// Empty stays nil, which every scoped channel reads as "all workspaces".
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	log.SetFlags(log.LstdFlags)
	home, _ := os.UserHomeDir()
	dbPath := env("KRAKOA_DB", filepath.Join(home, ".krakoa", "krakoa.db"))
	dataDir := env("KRAKOA_DATA_DIR", filepath.Join(home, ".krakoa", "data"))
	addr := env("KRAKOA_HTTP_ADDR", "127.0.0.1:7770")
	nifftyURL := env("KRAKOA_NIFFTY_URL", "http://127.0.0.1:7777")
	nifftyTo := os.Getenv("KRAKOA_NIFFTY_TO") // empty = niffty channel off
	chanURL := os.Getenv("KRAKOA_CHAN_URL")   // empty = chan channel off
	wsPaths := os.Getenv("KRAKOA_WORKSPACES")
	if wsPaths == "" {
		log.Fatal("KRAKOA_WORKSPACES is required (comma-separated workspace dirs)")
	}
	os.MkdirAll(filepath.Dir(dbPath), 0o755)
	os.MkdirAll(dataDir, 0o755)

	// Load every workspace; refuse broken ones, keep serving the rest.
	workspaces := map[string]*workspace.Workspace{}
	invalid := map[string][]string{}
	for _, p := range strings.Split(wsPaths, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ws, errs := workspace.Load(p)
		if len(errs) > 0 {
			var msgs []string
			for _, e := range errs {
				msgs = append(msgs, e.Error())
			}
			invalid[p] = msgs
			log.Printf("REFUSED workspace %s:", p)
			for _, m := range msgs {
				log.Printf("  - %s", m)
			}
			continue
		}
		workspaces[ws.Name] = ws
		log.Printf("loaded workspace %s (%s, git %s): %d workflows, %d agents, %d watchers",
			ws.Name, p, ws.GitVersion, len(ws.Workflows), len(ws.Agents), len(ws.Watchers))
	}
	if len(workspaces) == 0 {
		log.Fatal("no valid workspaces")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	bin, err := runner.ResolveBin()
	if err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("claude binary: %s", bin)

	eng := engine.New(st, &runner.Claude{Bin: bin}, engine.RealClock{}, workspaces, dataDir)
	eng.Channels = []contact.Channel{&contact.Console{W: os.Stdout}}
	if nifftyTo != "" {
		nf := contact.NewNiffty(nifftyURL, nifftyTo)
		nf.Bin = os.Getenv("KRAKOA_NIFFTY_BIN")
		nf.Only = splitList(os.Getenv("KRAKOA_NIFFTY_WORKSPACES"))
		nf.ThreadTS = func(runID string) string { return eng.ThreadRefForRun(runID, "slack_ts") }
		nf.SaveRef = func(runID, kind, value string) { eng.Bind(runID, kind, value) }
		eng.Channels = append(eng.Channels, nf)
		if nf.Bin != "" {
			board := &contact.NifftyBoard{
				Bin: nf.Bin,
				GetRef: func(thread, kind string) string {
					v, _ := st.ThreadRef(thread, kind)
					return v
				},
				SaveRef: func(thread, kind, value string) { st.SetThreadRef(thread, kind, value) },
				Log:     log.Printf,
			}
			eng.Board = board.Upsert
		}
	}
	if chanURL != "" {
		cn := contact.NewChan(chanURL, os.Getenv("KRAKOA_CHAN_START"))
		cn.Only = splitList(os.Getenv("KRAKOA_CHAN_WORKSPACES"))
		eng.Channels = append(eng.Channels, cn)
	}
	// Workspace-declared channels. The engine learns nothing about the tool on
	// the other end: it execs a script with JSON on stdin. Declaration is also
	// the scope — a channel serves the workspace that declared it, so there is
	// no routing to configure.
	for _, ws := range workspaces {
		for _, name := range sortedNames(ws.Contact) {
			c := ws.Contact[name]
			eng.Channels = append(eng.Channels, &contact.Script{
				ChannelName: name, Workspace: ws.Name, Dir: ws.Path,
				Command: c.Command, Timeout: c.Timeout.D(),
				Refs:    func(runID string) map[string]string { return eng.ThreadRefsForRun(runID) },
				SaveRef: func(runID, kind, value string) { eng.Bind(runID, kind, value) },
			})
			log.Printf("contact %s/%s -> %s", ws.Name, name, c.Command)
		}
	}

	eng.Recover()
	log.Printf("recovered; listening on %s", addr)

	mux := api(eng, st, workspaces, invalid)
	srv := &http.Server{Addr: addr, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go eng.RunForever(ctx, 5*time.Second)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http: %v", err)
	}
	log.Print("bye")
}

func api(eng *engine.Engine, st *store.Store, workspaces map[string]*workspace.Workspace, invalid map[string][]string) *http.ServeMux {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, code int, err error) {
		writeJSON(w, code, map[string]string{"error": err.Error()})
	}

	mux.Handle("GET /ui", uiHandler())
	mux.Handle("GET /ui/", uiHandler())

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		wss := map[string]string{}
		for name, ws := range workspaces {
			wss[name] = ws.GitVersion
		}
		writeJSON(w, 200, map[string]any{"ok": true, "workspaces": wss, "invalid": invalid})
	})

	// Slim workspace inventory so clients (doctor) never need
	// KRAKOA_WORKSPACES themselves — the daemon owns workspaces.
	mux.HandleFunc("GET /v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		type wsInfo struct {
			Name       string
			Path       string
			GitVersion string
			Doctor     []workspace.DoctorCheck
		}
		out := []wsInfo{}
		for _, ws := range workspaces {
			out = append(out, wsInfo{Name: ws.Name, Path: ws.Path, GitVersion: ws.GitVersion, Doctor: ws.Doctor})
		}
		writeJSON(w, 200, out)
	})

	mux.HandleFunc("POST /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Workspace string         `json:"workspace"`
			Workflow  string         `json:"workflow"`
			Inputs    map[string]any `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, 400, err)
			return
		}
		run, err := eng.StartRun(body.Workspace, body.Workflow, body.Inputs, "")
		if err != nil {
			fail(w, 400, err)
			return
		}
		writeJSON(w, 201, run)
	})

	mux.HandleFunc("GET /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		if thread := r.URL.Query().Get("thread"); thread != "" {
			runs, err := st.RunsByThread(thread)
			if err != nil {
				fail(w, 500, err)
				return
			}
			writeJSON(w, 200, runs)
			return
		}
		var statuses []core.RunStatus
		if s := r.URL.Query().Get("status"); s != "" {
			for _, part := range strings.Split(s, ",") {
				statuses = append(statuses, core.RunStatus(part))
			}
		}
		runs, err := st.ListRuns(statuses...)
		if err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, 200, runs)
	})

	mux.HandleFunc("GET /v1/threads", func(w http.ResponseWriter, r *http.Request) {
		threads, err := st.Threads()
		if err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, 200, threads)
	})

	mux.HandleFunc("GET /v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		run, err := st.GetRun(id)
		if err != nil {
			fail(w, 404, fmt.Errorf("run %s not found", id))
			return
		}
		steps, _ := st.StepsForRun(id)
		events, _ := st.EventsForRun(id)
		timers, _ := st.RunTimers(id)
		writeJSON(w, 200, map[string]any{"run": run, "steps": steps, "events": events, "timers": timers})
	})

	mux.HandleFunc("GET /v1/gates/{id}", func(w http.ResponseWriter, r *http.Request) {
		g, err := st.GetGate(r.PathValue("id"))
		if err != nil {
			fail(w, 404, fmt.Errorf("gate not found"))
			return
		}
		writeJSON(w, 200, g)
	})

	mux.HandleFunc("POST /v1/runs/{id}/bind", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Kind, Value string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Kind == "" || body.Value == "" {
			fail(w, 400, fmt.Errorf("kind and value required"))
			return
		}
		if err := eng.Bind(r.PathValue("id"), body.Kind, body.Value); err != nil {
			fail(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /v1/runs/{id}/refs", func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("kind")
		writeJSON(w, 200, map[string]string{"value": eng.ThreadRefForRun(r.PathValue("id"), kind)})
	})

	mux.HandleFunc("GET /v1/checks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, eng.ChecksBoard())
	})

	mux.HandleFunc("POST /v1/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if err := eng.CancelRun(r.PathValue("id"), body.Reason); err != nil {
			fail(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /v1/runs/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		if err := eng.ResumeRun(r.PathValue("id")); err != nil {
			fail(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /v1/gates", func(w http.ResponseWriter, r *http.Request) {
		gates, err := st.OpenGates()
		if err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, 200, gates)
	})

	mux.HandleFunc("POST /v1/gates/{id}/answer", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Response  string         `json:"response"`
			Answers   map[string]any `json:"answers"`
			Responder string         `json:"responder"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, 400, err)
			return
		}
		if body.Responder == "" {
			body.Responder = "cli"
		}
		if err := eng.AnswerGate(r.PathValue("id"), body.Response, body.Answers, body.Responder); err != nil {
			fail(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /v1/emit", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Workspace string         `json:"workspace"`
			Event     string         `json:"event"`
			Key       string         `json:"key"`
			Run       string         `json:"run"`
			Payload   map[string]any `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, 400, err)
			return
		}
		if body.Event == "" {
			fail(w, 400, fmt.Errorf("event is required"))
			return
		}
		disposition := eng.HandleEmit(body.Workspace, engine.EmittedEvent{
			Event: body.Event, Key: body.Key, RunID: body.Run, Payload: body.Payload,
		})
		writeJSON(w, 200, map[string]string{"disposition": disposition})
	})

	return mux
}

// sortedNames keeps channel construction deterministic.
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
