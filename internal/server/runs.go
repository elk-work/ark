package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// The run routes: open a run, finish it, record an artifact on it, and list
// what it recorded. RFC-0004 left runs out ("a run is produced by something
// that ran") and named the way back in as exit trigger 3: a caller that needs
// to write a run gets a route in the same shape. elk-work/ark#130 is that
// caller — a server that itself runs something (a deploy) and has no copy of
// Ark to record it in.
//
// Everything about authority is the three existing routes' and nothing is
// new: `write` on {repo} for the writes and `read` for the list
// (grants.go), and a writer resolved to an agent actor under a delegating
// human this caller may act under (resolveWriter). Like those routes, this
// file names no consumer.

// runDoc is the record document a run is stored as. Its keys are the JSON
// names of store.Run, which is what a client unmarshals the record into on
// pull; TestRunRoutesRoundTripThroughTheClientStructs is the guard against
// drift between the two.
func runDoc(id, repoID, actorID string, req api.CreateRunRequest, metadata string) map[string]any {
	agentName := strings.TrimSpace(req.AgentName)
	if agentName == "" {
		agentName = strings.TrimSpace(req.Writer.AgentName)
	}
	agentVersion := req.AgentVersion
	if agentVersion == "" {
		agentVersion = req.Writer.AgentVersion
	}
	now := records.Now()
	doc := map[string]any{
		"id":              id,
		"repository_id":   repoID,
		"agent_name":      agentName,
		"agent_version":   agentVersion,
		"status":          "running",
		"input_summary":   req.InputSummary,
		"started_at":      now,
		"branch_name":     req.BranchName,
		"base_commit_sha": req.BaseCommitSHA,
		"metadata_json":   metadata,
		"created_at":      now,
		"created_by":      actorID,
		"created_by_type": string(records.ActorAgent),
	}
	if req.TaskID != "" {
		doc["task_id"] = req.TaskID
	}
	return doc
}

// runMetadata validates the request's metadata and returns it as the string
// store.Run keeps in metadata_json. An object or nothing: metadata is a set
// of named facts about the run, and a bare string or array is a caller bug
// that would otherwise be replicated to every client as-is.
func runMetadata(raw json.RawMessage) (string, *writeFault) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	if trimmed[0] != '{' {
		return "", faultValidation("metadata must be a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return "", faultValidation("metadata is not valid JSON: " + err.Error())
	}
	return buf.String(), nil
}

// handleCreateRun records a run that has started.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	repoID := r.PathValue("repo")
	if !s.allow(w, r, repoID, api.GrantWrite) {
		return
	}
	req, ok := decode[api.CreateRunRequest](w, r)
	if !ok {
		return
	}
	key, fault := requireKey(r)
	if fault != nil {
		writeErr(w, fault.status, fault.code, fault.msg)
		return
	}
	metadata, fault := runMetadata(req.Metadata)
	if fault != nil {
		writeErr(w, fault.status, fault.code, fault.msg)
		return
	}

	ctx := r.Context()
	var resp api.RecordResponse
	err := s.Repos.Update(ctx, repoID, false, func(tx *sql.Tx) error {
		// Reset on every attempt: a lost CAS reruns this closure.
		resp = api.RecordResponse{}
		if err := replayedResponse(ctx, tx, key, http.StatusCreated, &api.RecordResponse{}); err != nil {
			return err
		}
		if req.TaskID != "" {
			// The client's agent_runs.task_id references tasks(id), so a run
			// naming a task this repository does not hold would fail the
			// pull of every client that tried to store it.
			present, err := recordExists(ctx, tx, "task", req.TaskID)
			if err != nil {
				return err
			}
			if !present {
				return faultNotFound("task " + req.TaskID + " not found")
			}
		}
		actorID, err := resolveWriter(ctx, tx, req.Writer, principalOf(r))
		if err != nil {
			return err
		}
		id := records.NewID()
		resp, err = writeRecord(ctx, tx, string(records.TypeRun), id, runDoc(id, repoID, actorID, req, metadata))
		if err != nil {
			return err
		}
		return rememberResponse(ctx, tx, key, resp, resp.ServerRevision)
	})
	s.finish(w, "create run", err, resp, http.StatusCreated)
}

// handleFinishRun moves a running run to a terminal status.
//
// It is the status route's shape for the same reason: finishing is an
// assertion about state, so a key is honoured but not required, and asking
// for the status a run already finished in answers the current record
// without minting a revision. A run that finished differently is a conflict —
// a run finishes once (store.FinishRun refuses the same), and silently
// rewriting how it ended would erase the record of what happened.
func (s *Server) handleFinishRun(w http.ResponseWriter, r *http.Request) {
	repoID, runID := r.PathValue("repo"), r.PathValue("id")
	if !s.allow(w, r, repoID, api.GrantWrite) {
		return
	}
	req, ok := decode[api.FinishRunRequest](w, r)
	if !ok {
		return
	}
	key := idempotencyKey(r)
	if len(key) > maxIdempotencyKey {
		writeErr(w, http.StatusBadRequest, "validation", "Idempotency-Key is too long")
		return
	}
	if f := oneOf("status", req.Status, records.TerminalRunStatuses); f != nil {
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
			WHERE record_type = 'agent_run' AND record_id = ? AND deleted_at IS NULL`,
			runID).Scan(&data, &currentRev)
		if errors.Is(err, sql.ErrNoRows) {
			return faultNotFound("run " + runID + " not found")
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
		if records.TerminalRunStatus(current) {
			if current == req.Status {
				return unchanged{status: http.StatusOK, resp: api.RecordResponse{
					Record: api.Record{RecordType: "agent_run", RecordID: runID, ID: runID,
						Data: json.RawMessage(data), ServerRevision: currentRev},
					ServerRevision: currentRev,
				}}
			}
			return &writeFault{http.StatusConflict, "conflict",
				"run " + runID + " already finished " + current + "; a run finishes once"}
		}
		// Authorization, as on the status route: an agent_run record has
		// no updated_by, so who finished it is not representable on it.
		if _, err := resolveWriter(ctx, tx, req.Writer, principalOf(r)); err != nil {
			return err
		}

		set := func(field string, val any) {
			b, _ := json.Marshal(val)
			doc[field] = b
		}
		fields := []string{"status", "finished_at"}
		set("status", req.Status)
		set("finished_at", records.Now())
		if req.ResultSummary != "" {
			set("result_summary", req.ResultSummary)
			fields = append(fields, "result_summary")
		}
		if req.ResultCommitSHA != "" {
			set("result_commit_sha", req.ResultCommitSHA)
			fields = append(fields, "result_commit_sha")
		}
		if req.ExitCode != nil {
			set("exit_code", *req.ExitCode)
			fields = append(fields, "exit_code")
		}
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
			WHERE record_type = 'agent_run' AND record_id = ?`,
			string(updated), rev, records.Now(), runID); err != nil {
			return err
		}
		if err := setFieldRevisions(ctx, tx, "agent_run", runID, fields, rev); err != nil {
			return err
		}
		resp = api.RecordResponse{
			Record: api.Record{RecordType: "agent_run", RecordID: runID, ID: runID,
				Data: json.RawMessage(updated), ServerRevision: rev},
			ServerRevision: rev,
		}
		return rememberResponse(ctx, tx, key, resp, resp.ServerRevision)
	})
	s.finish(w, "finish run", err, resp, http.StatusOK)
}

// isSHA256 reports whether s is a lowercase hex SHA-256, the form every
// artifact record and blob key in Ark is written in.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// handleCreateRunArtifact records a stored blob as an artifact of a run.
//
// It takes no bytes. Artifact content already has one way into the service —
// upload-url, the signed PUT, confirm — and confirm is where the service
// proves the bytes hash to their key (verifyBlobDigest). A second door that
// accepted bytes inline would need that proof again and would put artifact
// content through a JSON body bounded by decode's limit and the proxy's.
// So the blob must already be stored, by this repository: the blobs table is
// per repository, which is also what keeps this route from attaching a blob
// another repository uploaded.
func (s *Server) handleCreateRunArtifact(w http.ResponseWriter, r *http.Request) {
	repoID, runID := r.PathValue("repo"), r.PathValue("id")
	if !s.allow(w, r, repoID, api.GrantWrite) {
		return
	}
	req, ok := decode[api.CreateRunArtifactRequest](w, r)
	if !ok {
		return
	}
	key, fault := requireKey(r)
	if fault != nil {
		writeErr(w, fault.status, fault.code, fault.msg)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "validation", "name is required")
		return
	}
	if !isSHA256(req.SHA256) {
		writeErr(w, http.StatusBadRequest, "validation", "sha256 must be 64 lowercase hex characters")
		return
	}
	mediaType := strings.TrimSpace(req.MediaType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}

	ctx := r.Context()
	var resp api.RecordResponse
	err := s.Repos.Update(ctx, repoID, false, func(tx *sql.Tx) error {
		resp = api.RecordResponse{}
		if err := replayedResponse(ctx, tx, key, http.StatusCreated, &api.RecordResponse{}); err != nil {
			return err
		}
		present, err := recordExists(ctx, tx, "agent_run", runID)
		if err != nil {
			return err
		}
		if !present {
			return faultNotFound("run " + runID + " not found")
		}
		var size int64
		var storageKey string
		var stored bool
		err = tx.QueryRowContext(ctx, `SELECT size_bytes, storage_key, stored FROM blobs WHERE sha256 = ?`,
			req.SHA256).Scan(&size, &storageKey, &stored)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !stored) {
			return &writeFault{http.StatusConflict, "conflict", "blob " + req.SHA256 +
				" is not stored in this repository: upload it first (POST /v1/artifacts/upload-url, " +
				"PUT the bytes, POST /v1/artifacts/confirm)"}
		}
		if err != nil {
			return err
		}
		if req.SizeBytes != nil && *req.SizeBytes != size {
			return faultValidation("size_bytes does not match the stored blob")
		}
		actorID, err := resolveWriter(ctx, tx, req.Writer, principalOf(r))
		if err != nil {
			return err
		}
		id := records.NewID()
		doc := map[string]any{
			"id":              id,
			"repository_id":   repoID,
			"parent_type":     "agent_run",
			"parent_id":       runID,
			"name":            name,
			"media_type":      mediaType,
			"size_bytes":      size,
			"sha256":          req.SHA256,
			"storage_key":     storageKey,
			"created_at":      records.Now(),
			"created_by":      actorID,
			"created_by_type": string(records.ActorAgent),
		}
		resp, err = writeRecord(ctx, tx, string(records.TypeArtifact), id, doc)
		if err != nil {
			return err
		}
		return rememberResponse(ctx, tx, key, resp, resp.ServerRevision)
	})
	s.finish(w, "create run artifact", err, resp, http.StatusCreated)
}

// handleListRunArtifacts answers the artifacts a run recorded. A `read` act,
// the same level as fetching any record or an artifact's bytes.
func (s *Server) handleListRunArtifacts(w http.ResponseWriter, r *http.Request) {
	repoID, runID := r.PathValue("repo"), r.PathValue("id")
	if !s.allow(w, r, repoID, api.GrantRead) {
		return
	}
	ctx := r.Context()
	var out api.RunArtifactsResponse
	found := false
	err := s.Repos.View(ctx, repoID, func(db *sql.DB) error {
		out = api.RunArtifactsResponse{Artifacts: []api.Record{}}
		err := db.QueryRowContext(ctx, `SELECT true FROM records
			WHERE record_type = 'agent_run' AND record_id = ? AND deleted_at IS NULL`, runID).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
			return nil
		}
		if err != nil {
			return err
		}
		if err := db.QueryRowContext(ctx, `SELECT revision FROM meta WHERE id = 1`).
			Scan(&out.ServerRevision); err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, `SELECT record_id, data, server_revision FROM records
			WHERE record_type = 'artifact' AND deleted_at IS NULL
			  AND data->>'parent_type' = 'agent_run' AND data->>'parent_id' = ?
			ORDER BY record_id`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			rec := api.Record{RecordType: "artifact"}
			var data string
			if err := rows.Scan(&rec.RecordID, &data, &rec.ServerRevision); err != nil {
				return err
			}
			rec.ID, rec.Data = rec.RecordID, json.RawMessage(data)
			out.Artifacts = append(out.Artifacts, rec)
		}
		return rows.Err()
	})
	if err != nil {
		s.respond(w, "list run artifacts", err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "run "+runID+" not found")
		return
	}
	writeJSON(w, out)
}
