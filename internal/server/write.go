package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// The authenticated work-record write routes: create a task, comment on a
// record, move a task's status. See docs/rfc-0004-work-record-write-api.md.
//
// These exist so a program can write a work record without being a copy of
// Ark. /v1/sync/push is a replication protocol — its payloads are whole
// record documents in Ark's field vocabulary, with an operation verb and a
// base revision that serve the §10 merge rules rather than the caller's
// intent. A caller here sends what it means and gets back the record that
// was written, ULID and display number included.
//
// Nothing in this file names a particular consumer. It is a work-record API.

// writeFault is a handler failure carried out of a repodb.Update closure.
// Returning it rolls the transaction back, so a rejected write neither
// leaves a partial record behind nor re-uploads an unchanged database.
type writeFault struct {
	status int
	code   string
	msg    string
}

func (f *writeFault) Error() string { return f.msg }

func faultValidation(msg string) *writeFault {
	return &writeFault{http.StatusBadRequest, "validation", msg}
}

func faultNotFound(msg string) *writeFault {
	return &writeFault{http.StatusNotFound, "not_found", msg}
}

// unchanged carries a successful response out of an Update closure *without*
// committing: a replayed write and a status transition that asks for the
// status a task already has are both correct answers that must not mint a
// revision. Rolling back rather than committing a no-op is also what keeps
// them from re-uploading the repository database to the backend.
//
// resp is `any` because not every write route answers with a record —
// repository metadata is not one (repometa.go) — and the fault, replay and
// no-op machinery has no reason to know the difference.
type unchanged struct {
	resp     any
	status   int
	replayed bool
}

func (unchanged) Error() string { return "no change" }

// finish renders the outcome of a write closure. Faults and no-op successes
// travel as errors so their transactions roll back; everything else follows
// the existing repodb error contract.
func (s *Server) finish(w http.ResponseWriter, what string, err error, resp any, status int) {
	var u unchanged
	if errors.As(err, &u) {
		if u.replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		writeJSONStatus(w, u.status, u.resp)
		return
	}
	var f *writeFault
	if errors.As(err, &f) {
		writeErr(w, f.status, f.code, f.msg)
		return
	}
	if err != nil {
		s.respond(w, what, err)
		return
	}
	writeJSONStatus(w, status, resp)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// idempotencyKey reads the request's key and namespaces it. Caller-supplied
// keys share applied_mutations with client mutation IDs, so the prefix keeps
// a caller from reading back — or colliding with — someone's mutation.
func idempotencyKey(r *http.Request) string {
	k := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if k == "" {
		return ""
	}
	return "idem:" + k
}

// maxIdempotencyKey bounds what is stored as a primary key.
const maxIdempotencyKey = 255

// requireKey enforces Idempotency-Key on the create routes. It is not
// optional there: RFC-0001 reruns the whole Update closure on a lost
// compare-and-swap, so a keyless create would duplicate a record under
// contention — or under an ordinary client retry — with nothing in the
// result afterwards to tell the two apart.
func requireKey(r *http.Request) (string, *writeFault) {
	key := idempotencyKey(r)
	if key == "" {
		return "", faultValidation("Idempotency-Key header is required")
	}
	if len(key) > maxIdempotencyKey {
		return "", faultValidation("Idempotency-Key is too long")
	}
	return key, nil
}

// replayed returns a stored response for key, if this key has been served.
// into is a pointer to the response shape this route answers with; on a hit
// the stored document is decoded into it and travels back as unchanged.
func replayedResponse(ctx context.Context, tx *sql.Tx, key string, status int, into any) error {
	if key == "" {
		return nil
	}
	var stored sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT remote FROM applied_mutations WHERE mutation_id = ?`, key).
		Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !stored.Valid || json.Unmarshal([]byte(stored.String), into) != nil {
		// A key recorded without a readable response cannot be replayed
		// faithfully; refusing beats inventing an answer.
		return &writeFault{http.StatusConflict, "conflict", "idempotency key was used for a different request"}
	}
	return unchanged{resp: into, status: status, replayed: true}
}

// rememberResponse stores the outcome so a retry returns it verbatim. rev is
// passed rather than read off resp so the store works for any response shape.
func rememberResponse(ctx context.Context, tx *sql.Tx, key string, resp any, rev int64) error {
	if key == "" {
		return nil
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO applied_mutations
		(mutation_id, status, error, remote, server_revision, applied_at)
		VALUES (?, 'applied', '', ?, ?, ?)`,
		key, string(body), rev, records.Now())
	return err
}

// resolveWriter turns the request's writer into a repository-local actor ID
// and guarantees the record it authors is attributable (spec §3, §20).
//
// The registration is the actor record. A named agent is created once and
// reused forever after — the semantics store.FindAgentActor already gives a
// local agent — and a *new* one is admitted only with a delegated_by naming
// a human actor that already exists here. An agent cannot invent the
// authority it claims to act under, and a later request cannot re-point an
// existing agent at a different human, because delegated_by is read from the
// stored record and never from the request.
//
// The lookup picks the first registration, keyed on record_id — the client's
// ULID, which is immutable and lexically sortable. Not created_at: this
// server writes it as RFC3339Nano text and SQLite compares TEXT byte by byte,
// so it does not order chronologically (records.TimeCompare), and under
// LIMIT 1 that decides which identity a remote write is attributed to. Not
// server_revision either — every update rewrites it, so it is last-touched
// order rather than registration order.
func resolveWriter(ctx context.Context, tx *sql.Tx, wr api.Writer) (string, error) {
	name := strings.TrimSpace(wr.AgentName)
	if name == "" {
		return "", faultValidation("writer.agent_name is required")
	}

	var actorID string
	err := tx.QueryRowContext(ctx, `SELECT record_id FROM records
		WHERE record_type = 'actor' AND deleted_at IS NULL
		  AND data->>'type' = 'agent' AND data->>'agent_name' = ?
		ORDER BY record_id LIMIT 1`, name).Scan(&actorID)
	if err == nil {
		return actorID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	delegatedBy := strings.TrimSpace(wr.DelegatedBy)
	if delegatedBy == "" {
		return "", faultValidation(
			"writer.delegated_by is required to register the agent " + name +
				": a remote write is attributed to an agent acting under a human's authority")
	}
	var human bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM records
		WHERE record_type = 'actor' AND record_id = ? AND deleted_at IS NULL
		  AND data->>'type' = 'human'`, delegatedBy).Scan(&human)
	if errors.Is(err, sql.ErrNoRows) {
		return "", faultValidation("writer.delegated_by " + delegatedBy +
			" is not a human actor in this repository")
	}
	if err != nil {
		return "", err
	}

	actor := api.Actor{
		ID:           records.NewID(),
		Type:         string(records.ActorAgent),
		Name:         name,
		AgentName:    name,
		AgentVersion: wr.AgentVersion,
		DelegatedBy:  delegatedBy,
		CreatedAt:    records.Now(),
	}
	if err := upsertActor(ctx, tx, actor); err != nil {
		return "", err
	}
	return actor.ID, nil
}

// writeRecord inserts a new record document at a fresh revision and stamps
// its field revisions, so /v1/sync/pull carries it and the §10.4 field merge
// can see it as a concurrent change.
func writeRecord(ctx context.Context, tx *sql.Tx, recordType, recordID string, doc map[string]any) (api.RecordResponse, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return api.RecordResponse{}, err
	}
	rev, err := nextRevision(ctx, tx)
	if err != nil {
		return api.RecordResponse{}, err
	}
	now := records.Now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO records
		(record_type, record_id, data, server_revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		recordType, recordID, string(data), rev, now, now); err != nil {
		return api.RecordResponse{}, err
	}
	fields := make([]string, 0, len(doc))
	for k := range doc {
		fields = append(fields, k)
	}
	if err := setFieldRevisions(ctx, tx, recordType, recordID, fields, rev); err != nil {
		return api.RecordResponse{}, err
	}
	return api.RecordResponse{
		Record: api.Record{RecordType: recordType, RecordID: recordID,
			Data: json.RawMessage(data), ServerRevision: rev},
		ServerRevision: rev,
	}, nil
}

// mintNumber allocates the next display number for a numbered record type.
// This is the one creation path in Ark with no collision to reconcile: the
// service that renumbers everyone else's colliding numbers (§6.2) is the one
// allocating, inside the transaction that has exclusive access to the
// repository. The number is final; the ULID is still the identity.
func mintNumber(ctx context.Context, tx *sql.Tx, recordType string) (int64, error) {
	var max sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT MAX(CAST(data->>'number' AS INTEGER)) FROM records
		WHERE record_type = ?`, recordType).Scan(&max)
	if err != nil {
		return 0, err
	}
	return max.Int64 + 1, nil
}

// recordExists reports whether a live record of this type and ID is here.
func recordExists(ctx context.Context, tx *sql.Tx, recordType, recordID string) (bool, error) {
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT true FROM records
		WHERE record_type = ? AND record_id = ? AND deleted_at IS NULL`,
		recordType, recordID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// oneOf validates a caller-supplied enum against the shared vocabulary.
func oneOf(field, value string, allowed []string) *writeFault {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return faultValidation("invalid " + field + " " + value +
		" (allowed: " + strings.Join(allowed, ", ") + ")")
}

// handleCreateTask creates a task and mints its display number.
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	repoID := r.PathValue("repo")
	req, ok := decode[api.CreateTaskRequest](w, r)
	if !ok {
		return
	}
	key, fault := requireKey(r)
	if fault != nil {
		writeErr(w, fault.status, fault.code, fault.msg)
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeErr(w, http.StatusBadRequest, "validation", "title is required")
		return
	}
	status := req.Status
	if status == "" {
		status = "open"
	}
	if f := oneOf("status", status, records.TaskStatuses); f != nil {
		writeErr(w, f.status, f.code, f.msg)
		return
	}

	ctx := r.Context()
	var resp api.RecordResponse
	err := s.Repos.Update(ctx, repoID, false, func(tx *sql.Tx) error {
		// The closure reruns on a lost CAS; reset the accumulator so a
		// retry cannot report a revision the committed database never had.
		resp = api.RecordResponse{}
		if err := replayedResponse(ctx, tx, key, http.StatusCreated, &api.RecordResponse{}); err != nil {
			return err
		}
		actorID, err := resolveWriter(ctx, tx, req.Writer)
		if err != nil {
			return err
		}
		number, err := mintNumber(ctx, tx, "task")
		if err != nil {
			return err
		}
		id, now := records.NewID(), records.Now()
		doc := map[string]any{
			"id":              id,
			"repository_id":   repoID,
			"number":          number,
			"title":           title,
			"body":            req.Body,
			"status":          status,
			"created_at":      now,
			"created_by":      actorID,
			"created_by_type": string(records.ActorAgent),
			"updated_at":      now,
			"version":         1,
			// The record is authoritative the moment it is written here, so
			// it is born synced. A client that pulls it forces the same
			// value; carrying it keeps the document honest either way.
			"sync_state": "synced",
		}
		resp, err = writeRecord(ctx, tx, "task", id, doc)
		if err != nil {
			return err
		}
		return rememberResponse(ctx, tx, key, resp, resp.ServerRevision)
	})
	s.finish(w, "create task", err, resp, http.StatusCreated)
}

// handleCreateComment appends a comment to a record in this repository.
func (s *Server) handleCreateComment(w http.ResponseWriter, r *http.Request) {
	repoID := r.PathValue("repo")
	req, ok := decode[api.CreateCommentRequest](w, r)
	if !ok {
		return
	}
	key, fault := requireKey(r)
	if fault != nil {
		writeErr(w, fault.status, fault.code, fault.msg)
		return
	}
	if f := oneOf("parent_type", req.ParentType, records.CommentParents); f != nil {
		writeErr(w, f.status, f.code, f.msg)
		return
	}
	if req.ParentID == "" {
		writeErr(w, http.StatusBadRequest, "validation", "parent_id is required")
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		writeErr(w, http.StatusBadRequest, "validation", "body is required")
		return
	}

	ctx := r.Context()
	var resp api.RecordResponse
	err := s.Repos.Update(ctx, repoID, false, func(tx *sql.Tx) error {
		resp = api.RecordResponse{}
		if err := replayedResponse(ctx, tx, key, http.StatusCreated, &api.RecordResponse{}); err != nil {
			return err
		}
		present, err := recordExists(ctx, tx, req.ParentType, req.ParentID)
		if err != nil {
			return err
		}
		if !present {
			return faultNotFound(req.ParentType + " " + req.ParentID + " not found")
		}
		actorID, err := resolveWriter(ctx, tx, req.Writer)
		if err != nil {
			return err
		}
		id := records.NewID()
		doc := map[string]any{
			"id":              id,
			"repository_id":   repoID,
			"parent_type":     req.ParentType,
			"parent_id":       req.ParentID,
			"body":            req.Body,
			"created_at":      records.Now(),
			"created_by":      actorID,
			"created_by_type": string(records.ActorAgent),
		}
		// Comments are append-only (§6.3): a correction is a new comment
		// carrying supersedes_id, which is why there is no edit route.
		if req.SupersedesID != "" {
			doc["supersedes_id"] = req.SupersedesID
		}
		resp, err = writeRecord(ctx, tx, "comment", id, doc)
		if err != nil {
			return err
		}
		return rememberResponse(ctx, tx, key, resp, resp.ServerRevision)
	})
	s.finish(w, "create comment", err, resp, http.StatusCreated)
}

// handleTaskStatus moves a task within the allowed set.
//
// This is the only mutable-field write these routes offer, and it is safe
// for a reason the general case is not: status is a cloud-wins field under
// §10.4, so a server-side change needs no base revision from the caller and
// can never require a person to resolve it. Title and body can, which is why
// there is no general record editor here.
func (s *Server) handleTaskStatus(w http.ResponseWriter, r *http.Request) {
	repoID, taskID := r.PathValue("repo"), r.PathValue("id")
	req, ok := decode[api.TaskStatusRequest](w, r)
	if !ok {
		return
	}
	// A key is honoured but not required: unlike a create, asking twice for
	// the status a task already has is answerable from the record itself.
	key := idempotencyKey(r)
	if len(key) > maxIdempotencyKey {
		writeErr(w, http.StatusBadRequest, "validation", "Idempotency-Key is too long")
		return
	}
	if f := oneOf("status", req.Status, records.TaskStatuses); f != nil {
		writeErr(w, f.status, f.code, f.msg)
		return
	}

	ctx := r.Context()
	var resp api.RecordResponse
	err := s.Repos.Update(ctx, repoID, false, func(tx *sql.Tx) error {
		resp = api.RecordResponse{}
		if err := replayedResponse(ctx, tx, key, http.StatusOK, &api.RecordResponse{}); err != nil {
			return err
		}
		var data string
		var currentRev int64
		err := tx.QueryRowContext(ctx, `SELECT data, server_revision FROM records
			WHERE record_type = 'task' AND record_id = ? AND deleted_at IS NULL`,
			taskID).Scan(&data, &currentRev)
		if errors.Is(err, sql.ErrNoRows) {
			return faultNotFound("task " + taskID + " not found")
		}
		if err != nil {
			return err
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &doc); err != nil {
			return err
		}
		var current string
		json.Unmarshal(doc["status"], &current)
		if current == req.Status {
			// Already there. Answering without minting a revision keeps
			// every client from pulling an empty change set — the same
			// reasoning applyDelete uses for deleting what is already gone.
			return unchanged{status: http.StatusOK, resp: api.RecordResponse{
				Record: api.Record{RecordType: "task", RecordID: taskID,
					Data: json.RawMessage(data), ServerRevision: currentRev},
				ServerRevision: currentRev,
			}}
		}
		// Resolved for the authorization rule, not for attribution: a task
		// record has no updated_by field (§6.2), so who moved a status is
		// not representable on it today. Running the same writer check on
		// every route keeps the rule in one place, and is where RFC-0003's
		// principal binding will go.
		if _, err := resolveWriter(ctx, tx, req.Writer); err != nil {
			return err
		}

		now := records.Now()
		set := func(key string, val any) {
			b, _ := json.Marshal(val)
			doc[key] = b
		}
		set("status", req.Status)
		set("updated_at", now)
		updated, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		rev, err := nextRevision(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE records
			SET data = ?, server_revision = ?, updated_at = ?
			WHERE record_type = 'task' AND record_id = ?`,
			string(updated), rev, now, taskID); err != nil {
			return err
		}
		if err := setFieldRevisions(ctx, tx, "task", taskID,
			[]string{"status", "updated_at"}, rev); err != nil {
			return err
		}
		resp = api.RecordResponse{
			Record: api.Record{RecordType: "task", RecordID: taskID,
				Data: json.RawMessage(updated), ServerRevision: rev},
			ServerRevision: rev,
		}
		return rememberResponse(ctx, tx, key, resp, resp.ServerRevision)
	})
	s.finish(w, "set task status", err, resp, http.StatusOK)
}
