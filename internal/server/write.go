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
// The registration is the actor record, and the identity it registers is per
// (agent name, delegating human) — the semantics store.FindAgentActor gives a
// local agent (§6.0). RFC-0004 Decision 2 defines this side by pointing at
// that one, so when elk-work/ark#100 changed what the client resolves, the two
// halves of a stated equivalence stopped agreeing and this had to follow
// (elk-work/ark#102).
//
// Keying on the name alone is what it used to do, and it collapsed everyone
// writing under one agent name in a repository onto whichever actor happened
// to hold the lowest ULID. Every person using the CLI writes through the same
// `ark-cli` name, so in a repository where one of them ran `ark repo set`
// first, the next person's write was attributed to *their* agent, under a
// delegated_by naming the first person. It was never a privilege escalation —
// nothing here writes as a human — and it was the same attribution error #100
// fixed on the client, reached from the other surface.
//
// # Keying on the delegating human without letting the request assert it
//
// The value has to be known at lookup time and the request is the only place
// it can come from, which would be an assertion if nothing checked it. What
// checks it is the authenticated principal: delegatingHuman admits only a
// human actor this repository holds that does not already belong to somebody
// else. A request therefore *chooses among the registrations its caller is
// entitled to*, and cannot reach one it is not.
//
// Deriving the human from the principal instead — never reading the request —
// was the tempting alternative and is wrong here. `ark init` mints a human
// actor per checkout, so one principal legitimately owns several in a
// repository, and choosing one of them would attribute the write to a
// different identity than the client's own FindAgentActor used: the same
// divergence #102 is about, on a new axis. It also has no answer for the
// legacy service token, which binds nothing and identifies nobody.
//
// # Why Decision 2 rule 3 still holds
//
// No stored actor is ever rewritten from a request. delegated_by *selects* a
// registration; it is never written over one. A request naming a different
// human resolves to that human's agent or registers it, and the existing
// agent keeps pointing at the human it was registered under — so what
// attributes the record is still the stored actor record, which is what rule
// 3 protects.
//
// Within one (name, human) pair the lookup still picks the first
// registration, keyed on record_id — the client's ULID, which is immutable
// and lexically sortable. Not created_at: this server writes it as RFC3339Nano
// text and SQLite compares TEXT byte by byte, so it does not order
// chronologically (records.TimeCompare), and under LIMIT 1 that decides which
// identity a remote write is attributed to. Not server_revision either —
// every update rewrites it, so it is last-touched order rather than
// registration order.
//
// who is the principal making the call. An agent actor these routes register
// is bound to it (grantsactors.go), so the record of who introduced an
// identity is complete however the identity arrived. That binding is recorded
// and not *checked*: refusing a principal that writes as an agent another
// principal introduced is elk-work/ark#101, and it stays blocked until the
// fleet has moved past #100 — an un-upgraded client resolves to the shared
// actor by a choice made before the request exists.
func resolveWriter(ctx context.Context, tx *sql.Tx, wr api.Writer, who *authenticated) (string, error) {
	name := strings.TrimSpace(wr.AgentName)
	if name == "" {
		return "", faultValidation("writer.agent_name is required")
	}
	delegatedBy, err := delegatingHuman(ctx, tx, name, wr, who)
	if err != nil {
		return "", err
	}

	var actorID string
	err = tx.QueryRowContext(ctx, `SELECT record_id FROM records
		WHERE record_type = 'actor' AND deleted_at IS NULL
		  AND data->>'type' = 'agent' AND data->>'agent_name' = ?
		  AND data->>'delegated_by' = ?
		ORDER BY record_id LIMIT 1`, name, delegatedBy).Scan(&actorID)
	if err == nil {
		return actorID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
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
	if _, err := upsertActor(ctx, tx, actor); err != nil {
		return "", err
	}
	if err := bindActor(ctx, tx, actor.ID, binderOf(who)); err != nil {
		return "", err
	}
	return actor.ID, nil
}

// delegatingHuman validates the human a remote write claims to act under and
// returns it, so resolveWriter can key its lookup on it.
//
// Two rules, answering two different questions. The first is about the value
// and is RFC-0004 Decision 2 rule 2: it must name a human actor this
// repository already holds, or an agent would be inventing the authority it
// claims. That is `validation` — the request is malformed rather than
// forbidden, which is the code the RFC specifies and the one this route has
// always answered with.
//
// The second is about the caller, and is what makes the delegation safe to
// key on: a human actor bound to another principal is not one this caller may
// act under. That is `permission`, the code and the reasoning checkDelegation
// already applies on the push path (grantsactors.go) — the same rule at a
// second surface rather than a new policy, and one place to tighten when
// elk-work/ark#101 does.
//
// It runs on **every** write, not only where an agent is registered. A check
// that ran at registration alone would leave the reuse path as open as the
// name-only lookup was: name somebody else's human, land on their agent.
//
// "Not somebody else's" rather than "bound to me" is deliberate, and is taken
// from checkDelegation for its reason: every human actor in the live
// repositories was introduced under the legacy service token, which binds
// nothing, so requiring a positive binding would refuse the first write each
// of those people makes after moving to a credential.
//
// Under the legacy token itself there is no principal to check against — one
// string the whole fleet holds, identifying nobody — so only the first rule
// applies. That is what keeps the fleet writing, and it is the same
// break-glass §19.2 records against elk-work/ark#54.
func delegatingHuman(ctx context.Context, tx *sql.Tx, agentName string, wr api.Writer, who *authenticated) (string, error) {
	delegatedBy := strings.TrimSpace(wr.DelegatedBy)
	if delegatedBy == "" {
		return "", faultValidation("writer.delegated_by is required for the agent " + agentName +
			": a remote write is attributed to an agent acting under a human's authority, and an " +
			"agent identity is per (agent name, delegating human)")
	}
	// loadActorFacts is grantsactors.go's reader for exactly this question,
	// and reusing it keeps "what the service knows about an actor" in one
	// place. It does not filter deleted_at, and does not need to: actors
	// travel in a push's `actors` array rather than as mutations, so nothing
	// in Ark deletes one.
	facts, ok, err := loadActorFacts(ctx, tx, delegatedBy)
	if err != nil {
		return "", err
	}
	if !ok || facts.Type != string(records.ActorHuman) {
		return "", faultValidation("writer.delegated_by " + delegatedBy +
			" is not a human actor in this repository")
	}
	if binder := binderOf(who); binder != "" && facts.Principal != "" && facts.Principal != binder {
		return "", faultPermission("writer.delegated_by " + delegatedBy +
			" belongs to another principal: " + principalLabel(who) +
			" cannot write as an agent acting under somebody else's authority")
	}
	return delegatedBy, nil
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
	if !s.allow(w, r, repoID, api.GrantWrite) {
		return
	}
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
		actorID, err := resolveWriter(ctx, tx, req.Writer, principalOf(r))
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
	if !s.allow(w, r, repoID, api.GrantWrite) {
		return
	}
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
		actorID, err := resolveWriter(ctx, tx, req.Writer, principalOf(r))
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
	if !s.allow(w, r, repoID, api.GrantWrite) {
		return
	}
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
		// every route keeps the rule in one place — including the delegation
		// check, which resolveWriter now applies on every write rather than
		// only where an agent is registered. RFC-0003's principal binding is
		// the `write` grant checked at the top of this handler and the actor
		// binding resolveWriter records; both are in grants.go and
		// grantsactors.go.
		if _, err := resolveWriter(ctx, tx, req.Writer, principalOf(r)); err != nil {
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
