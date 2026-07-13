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
