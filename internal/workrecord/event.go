// Package workrecord normalizes Ark records into the work-record event
// contract Elk's connector consumes. See docs/rfc-0002-elk-work-record-adapter.md.
//
// The contract is Elk's, not Ark's: the GitHub adapter shipped first and this
// package is adapter #2, producing the same event shape so everything
// downstream of normalization (workspace routing, identity, external_refs,
// `Closes elk:<id>` resolution) is untouched.
//
// Nothing here performs I/O or talks to Elk. Mapping is pure so the record
// semantics Ark owns — which statuses exist, that ULIDs are authoritative,
// that an agent acts under a delegating human — are tested here, beside the
// invariants they encode, rather than a repository away.
package workrecord

// Provider is the value Elk stores in external_refs.provider and
// source_bindings.provider for Ark. Both already allow it.
const Provider = "ark"

// Event kinds. The first six deliberately reuse the vocabulary the GitHub
// adapter established, so they inherit its mapping behaviour with no new
// arms downstream — an Ark task is an issue (`ark gh issue` already maps
// issues to tasks, spec §14).
//
// The last three have no GitHub equivalent, and they are the reason this
// adapter is worth building: an agent run's reasoning, a promotion, and a
// closed design thread are exactly the context Git and GitHub structurally
// cannot hold.
const (
	KindTaskOpened      = "issue.opened"
	KindTaskClosed      = "issue.closed"
	KindTaskStatus      = "issue.status"
	KindPROpened        = "pr.opened"
	KindPRMerged        = "pr.merged"
	KindReviewSubmitted = "review.submitted"
	KindComment         = "comment"

	KindRunFinished     = "run.finished"
	KindPromotionActive = "promotion.activated"
	KindThreadClosed    = "thread.closed"
)

// Actor identifies who acted, in the shape Elk's source_actor_map consumes.
//
// ProviderLogin is the map key. It is the actor's email when known and the
// name otherwise — never the actor ULID, because actors are minted per
// repository at `ark init`, so one human is a different ULID in every repo
// and a ULID-keyed identity map needs a row per person per repository.
//
// DelegatedByLogin carries the human an agent acted for. Ark records
// authority; GitHub does not. Resolution prefers the delegating human so an
// agent's work lands on the person who authorized it.
type Actor struct {
	ProviderLogin    string `json:"provider_login"`
	DisplayName      string `json:"display_name"`
	ActorType        string `json:"actor_type"` // human|agent
	AgentName        string `json:"agent_name,omitempty"`
	DelegatedByLogin string `json:"delegated_by_login,omitempty"`
}

// Refs carries kind-specific detail the mapper downstream may use without
// reparsing the body.
type Refs struct {
	TaskNumber      int64    `json:"task_number,omitempty"`
	TaskID          string   `json:"task_id,omitempty"`
	PRNumber        int64    `json:"pr_number,omitempty"`
	PRID            string   `json:"pr_id,omitempty"`
	RunID           string   `json:"run_id,omitempty"`
	ThreadID        string   `json:"thread_id,omitempty"`
	Status          string   `json:"status,omitempty"`
	ReviewState     string   `json:"review_state,omitempty"`
	Branch          string   `json:"branch,omitempty"`
	BaseCommitSHA   string   `json:"base_commit_sha,omitempty"`
	ResultCommitSHA string   `json:"result_commit_sha,omitempty"`
	MergeCommitSHA  string   `json:"merge_commit_sha,omitempty"`
	Environment     string   `json:"environment,omitempty"`
	MessageCount    int      `json:"message_count,omitempty"`
	ArtifactNames   []string `json:"artifact_names,omitempty"`
	ExitCode        *int64   `json:"exit_code,omitempty"`
}

// Event is one normalized work-record event.
//
// ExternalID is the dedup key and ObjectKey is the binding key; the split
// matters. Signals and captures reference the *event* key so a replayed pull
// is a no-op; actions reference the *object* key so promotion, closure, and
// resolution all meet on the same row. This mirrors the GitHub adapter
// exactly.
type Event struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	ObjectKey  string `json:"object_key"`
	Kind       string `json:"kind"`
	Actor      Actor  `json:"actor"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	URL        string `json:"url"`
	OccurredAt string `json:"occurred_at"`

	// RepoFullName carries the Ark repository ULID, which is what
	// source_bindings routes on and what survives a Git remote rename.
	// RepoName is the human label and is display-only.
	RepoFullName string `json:"repo_full_name"`
	RepoName     string `json:"repo_name"`

	// Revision is the server revision this event was derived from, or 0 for
	// a repository that has never synced. Advisory: the external_refs unique
	// index, not this field, is what makes replay safe.
	Revision int64 `json:"revision,omitempty"`

	Refs Refs `json:"refs"`
}
