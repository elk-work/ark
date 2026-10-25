// Package api defines the wire protocol between the ark client and the
// sync service. See docs/v1-spec.md §9 and §19.
package api

import "encoding/json"

// Actor mirrors the client's actor identity for cross-client display.
type Actor struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	Email        string `json:"email,omitempty"`
	AgentName    string `json:"agent_name,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	DelegatedBy  string `json:"delegated_by,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
}

// Mutation is one intent from a client's local log.
type Mutation struct {
	ID                 string          `json:"id"`
	RecordType         string          `json:"record_type"`
	RecordID           string          `json:"record_id"`
	Operation          string          `json:"operation"` // create|update|delete|append|submit
	BaseServerRevision int64           `json:"base_server_revision"`
	Payload            json.RawMessage `json:"payload"`
	CreatedAt          string          `json:"created_at"`
	CreatedBy          string          `json:"created_by"`
}

// RegisterRepositoryRequest registers (idempotently) a repository.
type RegisterRepositoryRequest struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch,omitempty"`
	GitRemoteURL  string `json:"git_remote_url,omitempty"`
	// Actors are this checkout's identities, upserted alongside the
	// repository. They ride here as well as on PushRequest because
	// registration is the only part of a sync that runs unconditionally: a
	// repository that never pushed would otherwise have no actor records at
	// all, and every write route resolves its writer against them
	// (elk-work/ark#47).
	Actors []Actor `json:"actors,omitempty"`
	// LastRevision is the server revision this checkout has already synced
	// past — its cursor (§9.2). It is the one fact that tells a repository
	// being created apart from one being resurrected: a client above zero is
	// asserting it holds history this service issued, so a service with no
	// database for it has lost it rather than never had it, and registration
	// refuses to create (§19). Zero — a client that has never synced, and
	// equally a client too old to send the field — creates as it always has.
	LastRevision int64 `json:"last_revision,omitempty"`
}

// RepositoryMetadata is the service's copy of a repository's identity: the
// name, default branch, and Git remote a human reads when a repository is
// listed or recovered. It is not a record — nothing pulls it, and it has no
// created_by — which is why it lives in the service's meta row rather than
// in `records`, and why correcting it needs its own route.
type RepositoryMetadata struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	GitRemoteURL  string `json:"git_remote_url"`
	// Revision is the repository's revision counter, which a metadata
	// change advances like any other write.
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at,omitempty"`
	// Mirror is where the service mirrors this repository's Git, and how
	// the last attempt went (elk-work/ark#125). Absent when no mirror is
	// set. The mirror is a backup: the service's copy is the record.
	Mirror MirrorStatus `json:"mirror,omitzero"`
}

// MirrorStatus is a repository's Git mirror: the URL the service runs `git
// push --mirror` to after every push and on a ticker, and the outcome of the
// last attempt. Only URL is stored with the repository; the rest is the
// service's own observation and is empty until it has tried.
type MirrorStatus struct {
	URL           string `json:"url"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastOKAt      string `json:"last_ok_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// PushEvent is the body of the push webhook (ARK_PUSH_WEBHOOK_URL): one POST
// per ref a push to the service's Git updated (elk-work/ark#125). Ark knows
// nothing about what the receiver does with it — Elk turns it into check
// runs.
type PushEvent struct {
	RepositoryID string     `json:"repository_id"`
	Ref          string     `json:"ref"`
	Before       string     `json:"before"` // all zeros for a created ref
	After        string     `json:"after"`  // all zeros for a deleted ref
	Pusher       PushPusher `json:"pusher"`
	// ChangedPaths is `git diff --name-only before after`; for a created ref,
	// the paths the tip commit changed (every path, for a root commit); for a
	// deleted ref, empty.
	ChangedPaths []string `json:"changed_paths"`
	// ChangedPathsTruncated is set when ChangedPaths was cut short.
	ChangedPathsTruncated bool `json:"changed_paths_truncated,omitempty"`
	// Manifests carries known files at After by path ("ci/suites.yml"),
	// present only when the file exists there.
	Manifests map[string]string `json:"manifests"`
}

// PushPusher names the principal whose credential made the push.
type PushPusher struct {
	PrincipalID string `json:"principal_id"`
	Email       string `json:"email"`
}

// SetRepositoryMetadataRequest is the body of
// POST /v1/repositories/{repo}/metadata.
//
// Every field is a pointer because omitting one and clearing one are
// different requests. Registration can only ever backfill an empty field
// (server.go), so a plain string here would make every partial update send
// back values the caller never meant to assert — which is the shape of the
// bug that closed this path in the first place.
type SetRepositoryMetadataRequest struct {
	Writer        Writer  `json:"writer"`
	Name          *string `json:"name,omitempty"`
	DefaultBranch *string `json:"default_branch,omitempty"`
	// GitRemoteURL is the one field an explicit empty string clears: a
	// repository can genuinely have no remote, and refusing would leave a
	// wrong non-empty value uncorrectable.
	GitRemoteURL *string `json:"git_remote_url,omitempty"`
	// MirrorURL sets where the service mirrors this repository's Git; an
	// explicit empty string clears it. An https URL with no credential in
	// it — the credential is the service's own (ARK_MIRROR_CREDENTIAL) —
	// and setting it is an operator act on top of `admin`, because it
	// points that credential somewhere.
	MirrorURL *string `json:"mirror_url,omitempty"`
}

// RepositoryResponse carries the repository metadata as it now stands and
// the revision that ordered the change.
type RepositoryResponse struct {
	Repository RepositoryMetadata `json:"repository"`
	// Changed is false when every field asserted already held that value.
	// The request succeeded and no revision was minted; a caller that
	// reports what it corrected needs to know which of the two happened.
	Changed        bool  `json:"changed"`
	ServerRevision int64 `json:"server_revision"`
}

// PushRequest sends pending mutations in creation order.
type PushRequest struct {
	RepositoryID string     `json:"repository_id"`
	ClientID     string     `json:"client_id"`
	Actors       []Actor    `json:"actors,omitempty"`
	Mutations    []Mutation `json:"mutations"`
}

// MutationOutcome reports what the server did with one mutation.
type MutationOutcome struct {
	MutationID     string          `json:"mutation_id"`
	Error          string          `json:"error,omitempty"`
	Remote         json.RawMessage `json:"remote,omitempty"` // current server record on conflict
	ServerRevision int64           `json:"server_revision,omitempty"`
}

// PushResponse follows the spec §9.1 shape.
type PushResponse struct {
	Applied        []MutationOutcome `json:"applied"`
	Rejected       []MutationOutcome `json:"rejected"`
	Conflicts      []MutationOutcome `json:"conflicts"`
	ServerRevision int64             `json:"server_revision"`
}

// PullRequest asks for accepted changes after a revision.
type PullRequest struct {
	RepositoryID  string `json:"repository_id"`
	AfterRevision int64  `json:"after_revision"`
}

// Record is one server-authoritative record. Data is the JSON encoding of
// the record's client-side struct (store.Task, store.Comment, ...). Actors
// travel as records with RecordType "actor".
type Record struct {
	RecordType     string          `json:"record_type"`
	RecordID       string          `json:"record_id"`
	Data           json.RawMessage `json:"data"`
	ServerRevision int64           `json:"server_revision"`
}

// Tombstone marks a soft-deleted record.
type Tombstone struct {
	RecordType     string `json:"record_type"`
	RecordID       string `json:"record_id"`
	DeletedAt      string `json:"deleted_at"`
	ServerRevision int64  `json:"server_revision"`
}

// PullResponse follows the spec §9.2 shape.
type PullResponse struct {
	Records        []Record    `json:"records"`
	Tombstones     []Tombstone `json:"tombstones"`
	ServerRevision int64       `json:"server_revision"`
}

// MergeRequest asks the server to confirm a pull-request merge.
type MergeRequest struct {
	RepositoryID    string `json:"repository_id"`
	ExpectedHeadSHA string `json:"expected_head_sha,omitempty"`
	HeadCommitSHA   string `json:"head_commit_sha"`
	MergeCommitSHA  string `json:"merge_commit_sha"`
	MergedBy        string `json:"merged_by"`
}

// MergeResponse returns the updated PR record.
type MergeResponse struct {
	Record         Record `json:"record"`
	ServerRevision int64  `json:"server_revision"`
}

// UploadURLRequest asks for a signed URL to store an artifact blob.
type UploadURLRequest struct {
	RepositoryID string `json:"repository_id"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
	MediaType    string `json:"media_type,omitempty"`
}

// UploadURLResponse carries the signed PUT target. A blob already present
// returns AlreadyStored=true and no URL.
type UploadURLResponse struct {
	URL           string `json:"url,omitempty"`
	StorageKey    string `json:"storage_key"`
	AlreadyStored bool   `json:"already_stored,omitempty"`
}

// DownloadURLRequest asks for a signed URL to fetch an artifact blob.
type DownloadURLRequest struct {
	RepositoryID string `json:"repository_id"`
	StorageKey   string `json:"storage_key"`
}

// DownloadURLResponse carries the signed GET target.
type DownloadURLResponse struct {
	URL string `json:"url"`
}

// Error is the JSON error body for non-2xx responses.
type Error struct {
	Code    string `json:"code"` // validation|not_found|conflict|permission|internal|repository_corrupt
	Message string `json:"message"`
}

// ErrorCodeRepositoryCorrupt is the one error code the client reads rather
// than inferring from the status. The service's stored copy of a repository
// will not open, or holds no repository row; that is a 500 like any other
// server-side fault, and unlike any other it will still be true on the next
// attempt. The status cannot express the difference, so the code has to, and
// both sides name it from here rather than from two string literals that can
// drift apart in silence (elk-work/ark#65).
const ErrorCodeRepositoryCorrupt = "repository_corrupt"

// Writer names the agent identity a server-side write is attributed to.
// The service resolves it to a repository-local actor, per (AgentName,
// DelegatedBy): first use of a pair creates that actor, every later write
// under the same pair reuses it. See docs/rfc-0004-work-record-write-api.md
// Decision 2 and v1-spec §6.0.
type Writer struct {
	AgentName    string `json:"agent_name"`
	AgentVersion string `json:"agent_version,omitempty"`
	// DelegatedBy is the ULID of a human actor already in the repository,
	// and is required on every write: it is half the key the writer is
	// resolved by, not only a value stamped at registration. It selects a
	// registration and never rewrites one, so a request still cannot
	// re-point an existing agent at a different human.
	DelegatedBy string `json:"delegated_by,omitempty"`
}

// CreateTaskRequest is the body of POST /v1/repositories/{repo}/tasks.
type CreateTaskRequest struct {
	Writer Writer `json:"writer"`
	Title  string `json:"title"`
	Body   string `json:"body,omitempty"`
	Status string `json:"status,omitempty"` // defaults to open
}

// CreateCommentRequest is the body of POST /v1/repositories/{repo}/comments.
type CreateCommentRequest struct {
	Writer       Writer `json:"writer"`
	ParentType   string `json:"parent_type"`
	ParentID     string `json:"parent_id"`
	Body         string `json:"body"`
	SupersedesID string `json:"supersedes_id,omitempty"`
}

// TaskStatusRequest is the body of
// POST /v1/repositories/{repo}/tasks/{id}/status.
type TaskStatusRequest struct {
	Writer Writer `json:"writer"`
	Status string `json:"status"`
}

// RecordResponse is what every write route returns: the record as written,
// and the revision that carries it to clients on their next pull.
type RecordResponse struct {
	Record         Record `json:"record"`
	ServerRevision int64  `json:"server_revision"`
}
