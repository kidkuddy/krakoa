package core

import "time"

// NoticeKind classifies a one-way message. It is what decides whether the
// human's session is worth waking (P30): progress is not, a decision or a
// broken step is.
type NoticeKind string

const (
	NoticeProgress  NoticeKind = "progress"  // routine lifecycle chatter
	NoticeDone      NoticeKind = "done"      // merged + deployed: your check needed
	NoticeStuck     NoticeKind = "stuck"     // a step is failing; try to recover first
	NoticeBlocked   NoticeKind = "blocked"   // the world isn't ready (a check failed)
	NoticeUnblocked NoticeKind = "unblocked" // ...and now it is
)

// Notice is a message to the human that expects no answer. Gates carry
// decisions; notices carry facts.
type Notice struct {
	ID        string // stable: the receiving side dedupes on it
	Workspace string
	RunID     string // empty = engine-level (a failing check, not a run)
	State     string
	Kind      NoticeKind
	Text      string
	At        time.Time
}
