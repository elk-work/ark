package api

// Dangling references: what the service accepted while holding nothing at
// the other end (docs/v1-spec.md §9.1, elk-work/ark#56, #77, #74).
//
// The service does not enforce referential integrity on push, deliberately —
// nothing orders one client's push against another's, so the record a child
// names may still be sitting in somebody else's queue, and refusing the child
// would turn a skew that ends by itself into a rejection that never does.
// What it guarantees instead is that it says so, and until this route the
// only way to hear it was to copy `repos/<id>.db` out of the bucket and run
// §9.1's query by hand.

// DanglingReference is one entry in that ledger: a record the service holds,
// the field in it that pointed somewhere, and the record that pointer named.
//
// It is the event, not the state. Whether the reference *still* dangles is a
// live comparison against `records` — see Resolved — because both sides of
// the question are one table away on the service and records are never hard
// deleted, so the comparison is monotone and true whenever it is made. There
// is no `resolved_at` and there must not be one: a stamp would have to be
// written by every path that can create a record, and the path that forgot
// would leave the ledger naming an orphan that no longer exists.
type DanglingReference struct {
	// RecordType and RecordID name the child — the record that was accepted.
	RecordType string `json:"record_type"`
	RecordID   string `json:"record_id"`
	// Field is the referring field: parent_id, thread_id, task_id, ...
	Field string `json:"field"`
	// ParentType and ParentID name the record it pointed at, which the
	// service did not hold when the reference was first seen.
	ParentType string `json:"parent_type"`
	ParentID   string `json:"parent_id"`
	// MutationID is the push that carried the child. Empty for entries
	// written before the column existed, and for a record created by a write
	// route rather than by a mutation.
	MutationID string `json:"mutation_id,omitempty"`
	// FirstSeenAt is when the service first could not resolve this
	// reference. It does not move if the same reference is seen again.
	FirstSeenAt string `json:"first_seen_at"`
	// Resolved reports that the record this one named has since arrived, so
	// the entry is history rather than a defect. Never set on the default
	// answer, which lists only what is outstanding.
	Resolved bool `json:"resolved,omitempty"`
}

// DanglingResponse is the answer to GET /v1/repositories/{repo}/dangling.
//
// The counts are always the whole truth about the repository and are never
// affected by `all` or `limit`: an operator asking "is anything wrong here"
// gets a number without having to page through entries, and a truncated
// listing can never make the repository look healthier than it is.
type DanglingResponse struct {
	RepositoryID string `json:"repository_id"`
	// References is what was listed: the outstanding set by default, every
	// entry ever recorded when `all` was asked for.
	References []DanglingReference `json:"references"`
	// Outstanding is how many references in this repository resolve to
	// nothing right now. It is the defect count — §9.1: "an outstanding
	// entry is a defect, not a statistic".
	Outstanding int `json:"outstanding"`
	// Recorded is every entry ever written, resolved ones included. That a
	// repository sees this skew at all — and how often — is worth knowing,
	// which is why entries survive their own resolution.
	Recorded int `json:"recorded"`
	// Truncated reports that `limit` cut the listing short. The counts above
	// still describe the whole repository.
	Truncated bool `json:"truncated,omitempty"`
	// ServerRevision dates the answer, the way every other read of the
	// service's copy is dated. The set is a comparison made at a moment.
	ServerRevision int64 `json:"server_revision"`
}

// DanglingDefaultLimit and DanglingMaxLimit bound one listing. The ledger
// grows with the skew a repository has seen rather than with its size, so it
// is normally tiny — but it is client-driven data on a service that holds a
// whole repository in memory to answer, and an unbounded response is not
// something the caller should be able to ask for by accident.
const (
	DanglingDefaultLimit = 100
	DanglingMaxLimit     = 1000
)
