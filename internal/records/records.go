// Package records holds the shared vocabulary of Ark records: identifiers,
// timestamps, actor types, and validation used by every entity.
package records

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// ActorType identifies who created a record.
type ActorType string

const (
	ActorHuman ActorType = "human"
	ActorAgent ActorType = "agent"
)

func (a ActorType) Valid() bool { return a == ActorHuman || a == ActorAgent }

// RecordType names each Ark entity, used in mutations, comments, artifacts,
// and the search index.
type RecordType string

const (
	TypeRepository  RecordType = "repository"
	TypeTask        RecordType = "task"
	TypeComment     RecordType = "comment"
	TypeThread      RecordType = "agent_thread"
	TypeMessage     RecordType = "thread_message"
	TypeRun         RecordType = "agent_run"
	TypePullRequest RecordType = "pull_request"
	TypeReview      RecordType = "review"
	TypeArtifact    RecordType = "artifact"
	TypePromotion   RecordType = "promotion"
	// TypeGap      RecordType = "gap" // reserved for a future record type
)

var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// NewID returns a new ULID. ULIDs are locally generated, need no
// coordination, and — with monotonic entropy — sort strictly by creation
// order within a process even inside one millisecond.
func NewID() string {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now().UTC()), entropy).String()
}

// Now returns the current UTC time formatted for storage.
func Now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// ValidID reports whether s parses as a ULID.
func ValidID(s string) bool {
	_, err := ulid.ParseStrict(strings.ToUpper(s))
	return err == nil
}

// Task statuses.
var TaskStatuses = []string{"open", "in_progress", "blocked", "done", "closed"}

// Run statuses.
var RunStatuses = []string{"queued", "running", "succeeded", "failed", "cancelled"}

// TerminalRunStatuses are the statuses a run can finish in. The complement —
// queued, running — is a run still in flight.
var TerminalRunStatuses = []string{"succeeded", "failed", "cancelled"}

// TerminalRunStatus reports whether a run status means the run has ended.
func TerminalRunStatus(status string) bool {
	for _, s := range TerminalRunStatuses {
		if status == s {
			return true
		}
	}
	return false
}

// PR statuses.
var PRStatuses = []string{"open", "merged", "closed"}

// Review states.
var ReviewStates = []string{"comment", "approve", "request_changes"}

// Thread message roles.
var MessageRoles = []string{"user", "agent", "system", "tool"}

// Comment and artifact parent types.
var (
	CommentParents  = []string{"task", "pull_request", "agent_run", "review"}
	ArtifactParents = []string{"task", "agent_run", "pull_request", "review"}
)

// OneOf validates that value is a member of allowed, returning a typed
// validation error naming the field otherwise.
func OneOf(field, value string, allowed []string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return Validationf("invalid %s %q (allowed: %s)", field, value, strings.Join(allowed, ", "))
}

// ValidTaskTransition reports whether a task status change is allowed.
// V1 permits any transition except leaving a terminal state for itself;
// the useful check is that both values are legal statuses.
func ValidTaskTransition(from, to string) bool {
	if from == to {
		return true
	}
	return OneOf("status", from, TaskStatuses) == nil && OneOf("status", to, TaskStatuses) == nil
}

// Truncate shortens s to max runes for display, appending an ellipsis.
func Truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// TimeCompare orders two stored timestamps chronologically, returning -1, 0
// or 1 the way strings.Compare does.
//
// Never order stored timestamps with `<` on the strings. time.RFC3339Nano
// "removes trailing zeros from the seconds field", so the textual length of
// the fractional part depends on the value and lexical order stops matching
// chronological order the moment one timestamp lands on a round nanosecond:
//
//	earlier: 2026-08-26T07:00:00.92899Z
//	later:   2026-08-26T07:00:00.928991Z
//
// Both agree through ".92899". The next byte is 'Z' (0x5A) in the earlier one
// and '1' (0x31) in the later one, so the *later* timestamp sorts first. Go's
// own documentation calls the format unsuitable for sorting for this reason.
// It is not a rare edge either: records.Now() takes whatever resolution the
// host clock offers, and on a microsecond-resolution clock every value is
// trimmed, which put roughly one adjacent pair in 160 out of order in a
// 200k-sample measurement on macOS.
//
// A value that does not parse — an empty column, or a caller-supplied
// `--since` written as a bare date — falls back to a byte comparison for that
// pair, which is what such inputs already relied on.
//
// SQLite has no equivalent, because SQLite compares TEXT byte by byte and
// there is nothing to route through. So do not write `ORDER BY created_at`
// (or `activated_at`, or `started_at`) — it carries exactly this defect.
// Order by the record's ULID `id` instead: NewID() mints it from the same
// clock reading, ULIDs are lexically sortable by construction, and the ULID
// is the authoritative identifier anyway. Ordering by it is both correct and
// cheaper than ordering a text timestamp. The one thing to check first is
// that the column really is a NewID()-era stamp and not a time supplied from
// outside the process; for an externally supplied time the id is not a
// substitute, and the fix is a stored comparable form.
func TimeCompare(a, b string) int {
	if a == b {
		return 0
	}
	ta, errA := time.Parse(time.RFC3339Nano, a)
	tb, errB := time.Parse(time.RFC3339Nano, b)
	if errA != nil || errB != nil {
		return strings.Compare(a, b)
	}
	switch {
	case ta.Before(tb):
		return -1
	case ta.After(tb):
		return 1
	}
	return 0
}

// TimeBefore reports whether stored timestamp a is chronologically before b.
// See TimeCompare for why the strings must not be compared directly.
func TimeBefore(a, b string) bool { return TimeCompare(a, b) < 0 }

// TimeAfter reports whether stored timestamp a is chronologically after b.
// See TimeCompare for why the strings must not be compared directly.
func TimeAfter(a, b string) bool { return TimeCompare(a, b) > 0 }

// FormatTime renders a stored RFC3339 timestamp for human display.
func FormatTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

func Validationf(format string, args ...any) error {
	return &Error{Kind: KindValidation, Message: fmt.Sprintf(format, args...)}
}
