// Package store is the SQLite (WAL) persistence layer: runs, steps, gates,
// events, watcher dedupe, timers, buffered signals. The event table is the
// append-only audit spine everything else is debugged from.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kidkuddy/krakoa/internal/core"
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id          TEXT PRIMARY KEY,
  workspace   TEXT NOT NULL,
  workflow    TEXT NOT NULL,
  def_hash    TEXT NOT NULL,
  ws_version  TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL,
  status      TEXT NOT NULL,
  inputs      TEXT NOT NULL DEFAULT '{}',
  context     TEXT NOT NULL DEFAULT '{}',
  edge_counts TEXT NOT NULL DEFAULT '{}',
  parent      TEXT NOT NULL DEFAULT '',
  thread      TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS runs_status ON runs(workspace, workflow, status);

CREATE TABLE IF NOT EXISTS steps (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id       TEXT NOT NULL,
  state        TEXT NOT NULL,
  kind         TEXT NOT NULL,
  attempt      INTEGER NOT NULL DEFAULT 1,
  inputs       TEXT NOT NULL DEFAULT '{}',
  outcome      TEXT NOT NULL DEFAULT '',
  result       TEXT NOT NULL DEFAULT '{}',
  error        TEXT NOT NULL DEFAULT '',
  session_id   TEXT NOT NULL DEFAULT '',
  session_path TEXT NOT NULL DEFAULT '',
  handoff_dir  TEXT NOT NULL DEFAULT '',
  cost_usd     REAL NOT NULL DEFAULT 0,
  started_at   TEXT NOT NULL,
  ended_at     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS steps_run ON steps(run_id);

CREATE TABLE IF NOT EXISTS gates (
  id          TEXT PRIMARY KEY,
  workspace   TEXT NOT NULL,
  run_id      TEXT NOT NULL,
  state       TEXT NOT NULL,
  kind        TEXT NOT NULL,
  payload     TEXT NOT NULL DEFAULT '',
  options     TEXT NOT NULL DEFAULT '[]',
  status      TEXT NOT NULL,
  response    TEXT NOT NULL DEFAULT '',
  answers     TEXT NOT NULL DEFAULT '{}',
  responder   TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  resolved_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS gates_status ON gates(status);

CREATE TABLE IF NOT EXISTS events (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace TEXT NOT NULL,
  run_id    TEXT NOT NULL DEFAULT '',
  state     TEXT NOT NULL DEFAULT '',
  kind      TEXT NOT NULL,
  data      TEXT NOT NULL DEFAULT '{}',
  at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_run ON events(run_id);

CREATE TABLE IF NOT EXISTS watch_dedupe (
  watcher TEXT NOT NULL,
  key     TEXT NOT NULL,
  seen_at TEXT NOT NULL,
  PRIMARY KEY (watcher, key)
);

CREATE TABLE IF NOT EXISTS timers (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id   TEXT NOT NULL,
  state    TEXT NOT NULL,
  kind     TEXT NOT NULL,             -- timeout | probe | watcher | schedule
  fire_at  TEXT NOT NULL,
  every_ms INTEGER NOT NULL DEFAULT 0, -- recurring cadence; 0 = one-shot
  payload  TEXT NOT NULL DEFAULT '{}',
  active   INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS timers_due ON timers(active, fire_at);

CREATE TABLE IF NOT EXISTS signals (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id   TEXT NOT NULL,
  event    TEXT NOT NULL,
  payload  TEXT NOT NULL DEFAULT '{}',
  at       TEXT NOT NULL,
  consumed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS signals_pending ON signals(run_id, consumed);

-- per-gate redelivery/nagging bookkeeping. In memory this was lost on every
-- restart, so each restart re-nagged and re-escalated every open gate.
CREATE TABLE IF NOT EXISTS gate_nags (
  gate_id   TEXT PRIMARY KEY,
  last_try  TEXT NOT NULL DEFAULT '',
  tries     INTEGER NOT NULL DEFAULT 0,
  escalated INTEGER NOT NULL DEFAULT 0
);

-- contact bindings per effective thread key (run id until the thread
-- template stamps, migrated to the thread key after)
CREATE TABLE IF NOT EXISTS thread_refs (
  thread TEXT NOT NULL,
  kind   TEXT NOT NULL,   -- slack_ts | board_item | canvas:<gate-id>
  value  TEXT NOT NULL,
  PRIMARY KEY (thread, kind)
);
`

type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database and applies the schema. Path may be
// ":memory:" for hermetic tests.
func Open(path string) (*Store, error) {
	// modernc: single writer; busy_timeout smooths concurrent readers.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite is single-writer; one connection avoids SQLITE_BUSY entirely at
	// our minutes-to-days latencies.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// migrate applies additive column migrations (the schema above is
// CREATE IF NOT EXISTS, so existing tables never pick up new columns).
func (s *Store) migrate() error {
	for _, m := range []struct{ table, column, ddl string }{
		{"gates", "delivery", `ALTER TABLE gates ADD COLUMN delivery TEXT NOT NULL DEFAULT '{}'`},
		{"runs", "thread", `ALTER TABLE runs ADD COLUMN thread TEXT NOT NULL DEFAULT ''`},
		{"gates", "questions", `ALTER TABLE gates ADD COLUMN questions TEXT NOT NULL DEFAULT '[]'`},
	} {
		has, err := s.hasColumn(m.table, m.column)
		if err != nil {
			return err
		}
		if !has {
			if _, err := s.db.Exec(m.ddl); err != nil {
				return err
			}
		}
	}
	// Indexes on migrated columns must be created AFTER the ALTERs — in the
	// base schema they would fail against a pre-migration table.
	_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS runs_thread ON runs(thread)`)
	return err
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for read-only introspection (krakoactl why, ops).
func (s *Store) DB() *sql.DB { return s.db }

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func jenc(v any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func jdec(s string) map[string]any {
	m := map[string]any{}
	json.Unmarshal([]byte(s), &m)
	return m
}

// --- runs ---

func (s *Store) CreateRun(r *core.Run) error {
	_, err := s.db.Exec(`INSERT INTO runs
		(id, workspace, workflow, def_hash, ws_version, state, status, inputs, context, edge_counts, parent, thread, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Workspace, r.Workflow, r.DefHash, r.WSVersion, r.State, string(r.Status),
		jenc(r.Inputs), jenc(r.Context), jenc(r.EdgeCounts), r.Parent, r.Thread, ts(r.CreatedAt), ts(r.UpdatedAt))
	return err
}

func (s *Store) SaveRun(r *core.Run) error {
	res, err := s.db.Exec(`UPDATE runs SET state=?, status=?, context=?, edge_counts=?, updated_at=? WHERE id=?`,
		r.State, string(r.Status), jenc(r.Context), jenc(r.EdgeCounts), ts(r.UpdatedAt), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("run %s not found", r.ID)
	}
	return err
}

func scanRun(row interface{ Scan(...any) error }) (*core.Run, error) {
	var r core.Run
	var status, inputs, ctx, edges, created, updated string
	err := row.Scan(&r.ID, &r.Workspace, &r.Workflow, &r.DefHash, &r.WSVersion,
		&r.State, &status, &inputs, &ctx, &edges, &r.Parent, &r.Thread, &created, &updated)
	if err != nil {
		return nil, err
	}
	r.Status = core.RunStatus(status)
	r.Inputs = jdec(inputs)
	r.Context = jdec(ctx)
	r.EdgeCounts = map[string]int{}
	json.Unmarshal([]byte(edges), &r.EdgeCounts)
	r.CreatedAt, r.UpdatedAt = parseTS(created), parseTS(updated)
	return &r, nil
}

const runCols = "id, workspace, workflow, def_hash, ws_version, state, status, inputs, context, edge_counts, parent, thread, created_at, updated_at"

func (s *Store) GetRun(id string) (*core.Run, error) {
	return scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, id))
}

// ListRuns returns runs filtered by status set (empty = all), newest first.
func (s *Store) ListRuns(statuses ...core.RunStatus) ([]*core.Run, error) {
	q := `SELECT ` + runCols + ` FROM runs`
	var args []any
	if len(statuses) > 0 {
		q += ` WHERE status IN (?` + repeat(",?", len(statuses)-1) + `)`
		for _, st := range statuses {
			args = append(args, string(st))
		}
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*core.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRunThread stamps a run's thread key (once resolvable) and migrates any
// contact refs bound under the run id to the thread key.
func (s *Store) SetRunThread(id, thread string) error {
	if _, err := s.db.Exec(`UPDATE runs SET thread=? WHERE id=?`, thread, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE OR IGNORE thread_refs SET thread=? WHERE thread=?`, thread, id)
	return err
}

// SetThreadRef stores a contact binding for an effective thread key.
func (s *Store) SetThreadRef(thread, kind, value string) error {
	_, err := s.db.Exec(`INSERT INTO thread_refs (thread, kind, value) VALUES (?,?,?)
		ON CONFLICT(thread, kind) DO UPDATE SET value=excluded.value`, thread, kind, value)
	return err
}

// ThreadRef fetches one binding ("" if absent).
func (s *Store) ThreadRef(thread, kind string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM thread_refs WHERE thread=? AND kind=?`, thread, kind).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// RunsByThread returns a thread's runs, oldest first.
func (s *Store) RunsByThread(thread string) ([]*core.Run, error) {
	rows, err := s.db.Query(`SELECT `+runCols+` FROM runs WHERE thread=? ORDER BY created_at ASC`, thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*core.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ThreadSummary is one line of `krakoactl threads`.
type ThreadSummary struct {
	Thread    string
	Runs      int
	Statuses  string // distinct statuses, comma-joined
	CostUSD   float64
	FirstSeen time.Time
	LastSeen  time.Time
}

// Threads aggregates runs by thread key, most recently active first.
func (s *Store) Threads() ([]*ThreadSummary, error) {
	rows, err := s.db.Query(`
		SELECT r.thread, COUNT(DISTINCT r.id),
		       GROUP_CONCAT(DISTINCT r.status),
		       IFNULL((SELECT SUM(s2.cost_usd) FROM steps s2 WHERE s2.run_id IN
		               (SELECT id FROM runs r2 WHERE r2.thread = r.thread)), 0),
		       MIN(r.created_at), MAX(r.updated_at)
		FROM runs r WHERE r.thread != ''
		GROUP BY r.thread ORDER BY MAX(r.updated_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ThreadSummary
	for rows.Next() {
		var t ThreadSummary
		var first, last string
		if err := rows.Scan(&t.Thread, &t.Runs, &t.Statuses, &t.CostUSD, &first, &last); err != nil {
			return nil, err
		}
		t.FirstSeen, t.LastSeen = parseTS(first), parseTS(last)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// CountActive counts a workflow's runs currently occupying a concurrency slot.
func (s *Store) CountActive(workspace, workflow string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM runs
		WHERE workspace=? AND workflow=? AND status IN ('running','waiting','gated','needs-attention')`,
		workspace, workflow).Scan(&n)
	return n, err
}

// NextQueued returns the oldest queued run for a workflow, or nil.
func (s *Store) NextQueued(workspace, workflow string) (*core.Run, error) {
	r, err := scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs
		WHERE workspace=? AND workflow=? AND status='queued' ORDER BY created_at ASC LIMIT 1`,
		workspace, workflow))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// --- steps ---

func (s *Store) InsertStep(st *core.StepExecution) error {
	res, err := s.db.Exec(`INSERT INTO steps
		(run_id, state, kind, attempt, inputs, outcome, result, error, session_id, session_path, handoff_dir, cost_usd, started_at, ended_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		st.RunID, st.State, string(st.Kind), st.Attempt, jenc(st.Inputs), st.Outcome, jenc(st.Result),
		st.Error, st.SessionID, st.SessionPath, st.HandoffDir, st.CostUSD, ts(st.StartedAt), ts(st.EndedAt))
	if err != nil {
		return err
	}
	st.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateStep(st *core.StepExecution) error {
	_, err := s.db.Exec(`UPDATE steps SET outcome=?, result=?, error=?, session_id=?, session_path=?, handoff_dir=?, cost_usd=?, ended_at=? WHERE id=?`,
		st.Outcome, jenc(st.Result), st.Error, st.SessionID, st.SessionPath, st.HandoffDir, st.CostUSD, ts(st.EndedAt), st.ID)
	return err
}

func (s *Store) StepsForRun(runID string) ([]*core.StepExecution, error) {
	rows, err := s.db.Query(`SELECT id, run_id, state, kind, attempt, inputs, outcome, result, error,
		session_id, session_path, handoff_dir, cost_usd, started_at, ended_at
		FROM steps WHERE run_id=? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*core.StepExecution
	for rows.Next() {
		var st core.StepExecution
		var kind, inputs, result, started, ended string
		if err := rows.Scan(&st.ID, &st.RunID, &st.State, &kind, &st.Attempt, &inputs, &st.Outcome,
			&result, &st.Error, &st.SessionID, &st.SessionPath, &st.HandoffDir, &st.CostUSD, &started, &ended); err != nil {
			return nil, err
		}
		st.Kind = core.StepKind(kind)
		st.Inputs, st.Result = jdec(inputs), jdec(result)
		st.StartedAt, st.EndedAt = parseTS(started), parseTS(ended)
		out = append(out, &st)
	}
	return out, rows.Err()
}

// --- gates ---

func (s *Store) CreateGate(g *core.Gate) error {
	opts, _ := json.Marshal(g.Options)
	_, err := s.db.Exec(`INSERT INTO gates
		(id, workspace, run_id, state, kind, payload, options, status, response, answers, responder, created_at, resolved_at, delivery, questions)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.Workspace, g.RunID, g.State, string(g.Kind), g.Payload, string(opts),
		string(g.Status), g.Response, jenc(g.Answers), g.Responder, ts(g.CreatedAt), ts(g.ResolvedAt), jenc(anyMap(g.Delivery)), jencList(g.Questions))
	return err
}

func scanGate(row interface{ Scan(...any) error }) (*core.Gate, error) {
	var g core.Gate
	var kind, opts, status, answers, created, resolved, delivery, questions string
	err := row.Scan(&g.ID, &g.Workspace, &g.RunID, &g.State, &kind, &g.Payload, &opts,
		&status, &g.Response, &answers, &g.Responder, &created, &resolved, &delivery, &questions)
	if err != nil {
		return nil, err
	}
	g.Kind = core.GateKind(kind)
	g.Status = core.GateStatus(status)
	json.Unmarshal([]byte(opts), &g.Options)
	g.Answers = jdec(answers)
	g.Delivery = map[string]string{}
	json.Unmarshal([]byte(delivery), &g.Delivery)
	json.Unmarshal([]byte(questions), &g.Questions)
	g.CreatedAt, g.ResolvedAt = parseTS(created), parseTS(resolved)
	return &g, nil
}

func jencList(xs []string) string {
	if xs == nil {
		xs = []string{}
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

func anyMap(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SetGatePayload rewrites an open gate's text in place — one standing gate
// that stays current beats a stream of new gate ids saying the same thing.
func (s *Store) SetGatePayload(id, payload string) error {
	_, err := s.db.Exec(`UPDATE gates SET payload=? WHERE id=? AND status='open'`, payload, id)
	return err
}

// SetGateDelivery records the per-channel delivery outcome.
func (s *Store) SetGateDelivery(id string, delivery map[string]string) error {
	_, err := s.db.Exec(`UPDATE gates SET delivery=? WHERE id=?`, jenc(anyMap(delivery)), id)
	return err
}

const gateCols = "id, workspace, run_id, state, kind, payload, options, status, response, answers, responder, created_at, resolved_at, delivery, questions"

func (s *Store) GetGate(id string) (*core.Gate, error) {
	return scanGate(s.db.QueryRow(`SELECT `+gateCols+` FROM gates WHERE id=?`, id))
}

func (s *Store) OpenGates() ([]*core.Gate, error) {
	rows, err := s.db.Query(`SELECT ` + gateCols + ` FROM gates WHERE status='open' ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*core.Gate
	for rows.Next() {
		g, err := scanGate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// OpenGateForRun returns the open gate parked on a run, or nil.
func (s *Store) OpenGateForRun(runID string) (*core.Gate, error) {
	g, err := scanGate(s.db.QueryRow(`SELECT `+gateCols+` FROM gates WHERE run_id=? AND status='open' LIMIT 1`, runID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return g, err
}

// ResolveGate applies first-response-wins: it only flips an open gate.
// Returns false if the gate was already resolved (late duplicate — ignore).
func (s *Store) ResolveGate(id, response string, answers map[string]any, responder string, at time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE gates SET status='answered', response=?, answers=?, responder=?, resolved_at=?
		WHERE id=? AND status='open'`,
		response, jenc(answers), responder, ts(at), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CancelGate closes an open gate without a response (e.g. run canceled).
func (s *Store) CancelGate(id string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE gates SET status='canceled', resolved_at=? WHERE id=? AND status='open'`, ts(at), id)
	return err
}

// --- events ---

func (s *Store) AppendEvent(e *core.Event) error {
	res, err := s.db.Exec(`INSERT INTO events (workspace, run_id, state, kind, data, at) VALUES (?,?,?,?,?,?)`,
		e.Workspace, e.RunID, e.State, e.Kind, jenc(e.Data), ts(e.At))
	if err != nil {
		return err
	}
	e.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) EventsForRun(runID string) ([]*core.Event, error) {
	rows, err := s.db.Query(`SELECT id, workspace, run_id, state, kind, data, at FROM events WHERE run_id=? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*core.Event
	for rows.Next() {
		var e core.Event
		var data, at string
		if err := rows.Scan(&e.ID, &e.Workspace, &e.RunID, &e.State, &e.Kind, &data, &at); err != nil {
			return nil, err
		}
		e.Data = jdec(data)
		e.At = parseTS(at)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// --- watcher dedupe ---

// DedupeSeen reports whether a watcher observation key was already consumed.
func (s *Store) DedupeSeen(watcher, key string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM watch_dedupe WHERE watcher=? AND key=?`, watcher, key).Scan(&n)
	return n > 0, err
}

// DedupeMark records a consumed observation key. Only mark keys whose event
// was actually acted on — unconsumed observations must retry next sweep.
func (s *Store) DedupeMark(watcher, key string, at time.Time) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO watch_dedupe (watcher, key, seen_at) VALUES (?,?,?)`,
		watcher, key, ts(at))
	return err
}

// --- timers ---

type Timer struct {
	ID      int64
	RunID   string
	State   string
	Kind    string // timeout | probe | watcher | schedule
	FireAt  time.Time
	Every   time.Duration
	Payload map[string]any
}

func (s *Store) ArmTimer(t *Timer) error {
	res, err := s.db.Exec(`INSERT INTO timers (run_id, state, kind, fire_at, every_ms, payload) VALUES (?,?,?,?,?,?)`,
		t.RunID, t.State, t.Kind, ts(t.FireAt), t.Every.Milliseconds(), jenc(t.Payload))
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

// DueTimers returns active timers with fire_at <= now.
func (s *Store) DueTimers(now time.Time) ([]*Timer, error) {
	rows, err := s.db.Query(`SELECT id, run_id, state, kind, fire_at, every_ms, payload FROM timers
		WHERE active=1 AND fire_at<=? ORDER BY fire_at ASC`, ts(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTimers(rows)
}

// RunTimers returns a run's armed timers (live wait rendering).
func (s *Store) RunTimers(runID string) ([]*Timer, error) {
	rows, err := s.db.Query(`SELECT id, run_id, state, kind, fire_at, every_ms, payload FROM timers WHERE active=1 AND run_id=?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTimers(rows)
}

// ActiveTimers returns all armed timers (startup re-arm).
func (s *Store) ActiveTimers() ([]*Timer, error) {
	rows, err := s.db.Query(`SELECT id, run_id, state, kind, fire_at, every_ms, payload FROM timers WHERE active=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTimers(rows)
}

func scanTimers(rows *sql.Rows) ([]*Timer, error) {
	var out []*Timer
	for rows.Next() {
		var t Timer
		var fire, payload string
		var everyMS int64
		if err := rows.Scan(&t.ID, &t.RunID, &t.State, &t.Kind, &fire, &everyMS, &payload); err != nil {
			return nil, err
		}
		t.FireAt = parseTS(fire)
		t.Every = time.Duration(everyMS) * time.Millisecond
		t.Payload = jdec(payload)
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *Store) DisarmTimer(id int64) error {
	_, err := s.db.Exec(`UPDATE timers SET active=0 WHERE id=?`, id)
	return err
}

// DisarmRunTimers kills every armed timer for a run (state transition).
func (s *Store) DisarmRunTimers(runID string) error {
	_, err := s.db.Exec(`UPDATE timers SET active=0 WHERE run_id=? AND active=1`, runID)
	return err
}

// Reschedule moves a recurring timer to its next firing.
func (s *Store) Reschedule(id int64, next time.Time) error {
	_, err := s.db.Exec(`UPDATE timers SET fire_at=? WHERE id=?`, ts(next), id)
	return err
}

// --- gate nagging ---

// GateNag is how far the delivery sweep has gone with one gate.
type GateNag struct {
	GateID    string
	LastTry   time.Time
	Tries     int
	Escalated bool
}

func (s *Store) GateNags() (map[string]*GateNag, error) {
	rows, err := s.db.Query(`SELECT gate_id, last_try, tries, escalated FROM gate_nags`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*GateNag{}
	for rows.Next() {
		var n GateNag
		var last string
		var esc int
		if err := rows.Scan(&n.GateID, &last, &n.Tries, &esc); err != nil {
			return nil, err
		}
		n.LastTry, n.Escalated = parseTS(last), esc == 1
		out[n.GateID] = &n
	}
	return out, rows.Err()
}

func (s *Store) SaveGateNag(n *GateNag) error {
	esc := 0
	if n.Escalated {
		esc = 1
	}
	_, err := s.db.Exec(`INSERT INTO gate_nags (gate_id, last_try, tries, escalated) VALUES (?,?,?,?)
		ON CONFLICT(gate_id) DO UPDATE SET last_try=excluded.last_try, tries=excluded.tries, escalated=excluded.escalated`,
		n.GateID, ts(n.LastTry), n.Tries, esc)
	return err
}

// PruneGateNags drops bookkeeping for gates that are no longer open.
func (s *Store) PruneGateNags() error {
	_, err := s.db.Exec(`DELETE FROM gate_nags WHERE gate_id NOT IN (SELECT id FROM gates WHERE status='open')`)
	return err
}

// --- buffered signals ---

// BufferSignal stores an event targeting a run that can't consume it yet.
// Idempotent per (run, event): watchers re-observe unchanged world state
// every sweep, and one pending copy is all a wait state can consume.
func (s *Store) BufferSignal(runID, event string, payload map[string]any, at time.Time) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM signals WHERE run_id=? AND event=? AND consumed=0`,
		runID, event).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO signals (run_id, event, payload, at) VALUES (?,?,?,?)`,
		runID, event, jenc(payload), ts(at))
	return err
}

type Signal struct {
	ID      int64
	RunID   string
	Event   string
	Payload map[string]any
	At      time.Time
}

// PendingSignals returns unconsumed signals for a run, oldest first.
func (s *Store) PendingSignals(runID string) ([]*Signal, error) {
	rows, err := s.db.Query(`SELECT id, run_id, event, payload, at FROM signals WHERE run_id=? AND consumed=0 ORDER BY id ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Signal
	for rows.Next() {
		var sig Signal
		var payload, at string
		if err := rows.Scan(&sig.ID, &sig.RunID, &sig.Event, &payload, &at); err != nil {
			return nil, err
		}
		sig.Payload = jdec(payload)
		sig.At = parseTS(at)
		out = append(out, &sig)
	}
	return out, rows.Err()
}

func (s *Store) ConsumeSignal(id int64) error {
	_, err := s.db.Exec(`UPDATE signals SET consumed=1 WHERE id=?`, id)
	return err
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
