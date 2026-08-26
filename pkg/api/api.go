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
	Code    string `json:"code"` // validation|not_found|conflict|permission|internal
	Message string `json:"message"`
}

// Writer names the agent identity a server-side write is attributed to.
// The service resolves it to a repository-local actor: first use creates
// that actor, every later write reuses it, and DelegatedBy is read from the
// stored actor record rather than from the request. See
// docs/rfc-0004-work-record-write-api.md Decision 2.
type Writer struct {
	AgentName    string `json:"agent_name"`
	AgentVersion string `json:"agent_version,omitempty"`
	// DelegatedBy is the ULID of a human actor already in the repository.
	// Consulted only when the agent actor is created; ignored afterwards.
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
