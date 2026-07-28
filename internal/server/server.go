package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/elkproject/ark/internal/records"
	"github.com/elkproject/ark/internal/server/repodb"
	"github.com/elkproject/ark/pkg/api"
)

// Server is the Ark sync service.
type Server struct {
	Repos   *repodb.Manager
	Token   string // single service token (spec §20: V1 begins with one)
	Blobs   BlobStore
	Log     *slog.Logger
	Version string // build stamp, reported unauthenticated on GET /
}

// Handler builds the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeErr(w, http.StatusNotFound, "not_found", "unknown route")
			return
		}
		root := map[string]string{"service": "ark-sync", "api": "v1"}
		if s.Version != "" {
			root["version"] = s.Version
		}
		writeJSON(w, root)
	})
	// Not /healthz: Google Frontend intercepts that path on run.app
	// hostnames and serves its own 404 before the request reaches the
	// container (verified empirically 2026-07-13).
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("POST /v1/repositories", s.auth(s.handleRegisterRepo))
	mux.HandleFunc("POST /v1/sync/push", s.auth(s.handlePush))
	mux.HandleFunc("POST /v1/sync/pull", s.auth(s.handlePull))
	mux.HandleFunc("GET /v1/repositories/{repo}/records/{type}/{id}", s.auth(s.handleGetRecord))
	mux.HandleFunc("POST /v1/pull-requests/{id}/merge", s.auth(s.handleMerge))
	mux.HandleFunc("POST /v1/artifacts/upload-url", s.auth(s.handleUploadURL))
	mux.HandleFunc("POST /v1/artifacts/confirm", s.auth(s.handleConfirmUpload))
	mux.HandleFunc("POST /v1/artifacts/download-url", s.auth(s.handleDownloadURL))
	if local, ok := s.Blobs.(*LocalBlobStore); ok {
		mux.Handle("GET /blobs/", local.Handler())
		mux.Handle("PUT /blobs/", local.Handler())
	}
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "permission", "invalid or missing token")
			return
		}
		next(w, r)
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(api.Error{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&v); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "invalid JSON body: "+err.Error())
		return v, false
	}
	return v, true
}

// respond maps repodb errors onto the API error contract.
func (s *Server) respond(w http.ResponseWriter, what string, err error) {
	switch {
	case err == nil:
	case errors.Is(err, repodb.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "repository not registered")
	case errors.Is(err, repodb.ErrConcurrentWrite):
		writeErr(w, http.StatusConflict, "conflict", "repository is being updated concurrently; retry")
	default:
		s.internal(w, what, err)
	}
}

func (s *Server) handleRegisterRepo(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.RegisterRepositoryRequest](w, r)
	if !ok {
		return
	}
	if req.ID == "" || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "validation", "id and name are required")
		return
	}
	if req.DefaultBranch == "" {
		req.DefaultBranch = "main"
	}
	err := s.Repos.Update(r.Context(), req.ID, true, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO meta (id, repository_id, name, default_branch, git_remote_url, created_at)
			VALUES (1, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name,
				default_branch = excluded.default_branch, git_remote_url = excluded.git_remote_url`,
			req.ID, req.Name, req.DefaultBranch, req.GitRemoteURL, records.Now())
		return err
	})
	if err != nil {
		s.respond(w, "register repository", err)
		return
	}
	writeJSON(w, map[string]string{"id": req.ID})
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.PushRequest](w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var resp api.PushResponse

	err := s.Repos.Update(ctx, req.RepositoryID, false, func(tx *sql.Tx) error {
		// The whole fn reruns on a lost CAS race; reset accumulators so a
		// retry doesn't double-report outcomes.
		resp = api.PushResponse{
			Applied:   []api.MutationOutcome{},
			Rejected:  []api.MutationOutcome{},
			Conflicts: []api.MutationOutcome{},
		}
		// Actors are upserted as records so every client can render names.
		for _, a := range req.Actors {
			if a.ID == "" {
				continue
			}
			if err := upsertActor(ctx, tx, a); err != nil {
				return err
			}
		}
		// Mutations apply in creation order; savepoint handling inside
		// processMutation keeps one bad mutation from poisoning the batch.
		session := sessionRevisions{}
		for _, m := range req.Mutations {
			out := processMutation(ctx, tx, m, session)
			mo := api.MutationOutcome{MutationID: m.ID, Error: out.err,
				Remote: out.remote, ServerRevision: out.revision}
			switch out.status {
			case statusApplied:
				resp.Applied = append(resp.Applied, mo)
			case statusConflict:
				resp.Conflicts = append(resp.Conflicts, mo)
			default:
				resp.Rejected = append(resp.Rejected, mo)
			}
		}
		return tx.QueryRowContext(ctx, `SELECT revision FROM meta WHERE id = 1`).
			Scan(&resp.ServerRevision)
	})
	if err != nil {
		s.respond(w, "push", err)
		return
	}
	writeJSON(w, resp)
}

// upsertActor stores an actor as a record, bumping the revision only for
// new actors so pulls stay quiet on repeat pushes.
func upsertActor(ctx context.Context, tx *sql.Tx, a api.Actor) error {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT true FROM records
		WHERE record_type = 'actor' AND record_id = ?`, a.ID).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	rev, err := nextRevision(ctx, tx)
	if err != nil {
		return err
	}
	now := records.Now()
	_, err = tx.ExecContext(ctx, `INSERT INTO records
		(record_type, record_id, data, server_revision, created_at, updated_at)
		VALUES ('actor', ?, ?, ?, ?, ?)`, a.ID, string(data), rev, now, now)
	return err
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.PullRequest](w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	resp := api.PullResponse{Records: []api.Record{}, Tombstones: []api.Tombstone{}}

	err := s.Repos.View(ctx, req.RepositoryID, func(db *sql.DB) error {
		if err := db.QueryRowContext(ctx, `SELECT revision FROM meta WHERE id = 1`).
			Scan(&resp.ServerRevision); err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, `SELECT record_type, record_id, data, server_revision, deleted_at
			FROM records WHERE server_revision > ? ORDER BY server_revision`, req.AfterRevision)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rec api.Record
			var data string
			var deletedAt sql.NullString
			if err := rows.Scan(&rec.RecordType, &rec.RecordID, &data, &rec.ServerRevision, &deletedAt); err != nil {
				return err
			}
			rec.Data = json.RawMessage(data)
			if deletedAt.Valid {
				resp.Tombstones = append(resp.Tombstones, api.Tombstone{
					RecordType: rec.RecordType, RecordID: rec.RecordID,
					DeletedAt: deletedAt.String, ServerRevision: rec.ServerRevision})
			} else {
				resp.Records = append(resp.Records, rec)
			}
		}
		return rows.Err()
	})
	if err != nil {
		s.respond(w, "pull", err)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) handleGetRecord(w http.ResponseWriter, r *http.Request) {
	repoID, recordType, recordID := r.PathValue("repo"), r.PathValue("type"), r.PathValue("id")
	var rec api.Record
	rec.RecordType, rec.RecordID = recordType, recordID
	found := false
	err := s.Repos.View(r.Context(), repoID, func(db *sql.DB) error {
		var data string
		err := db.QueryRowContext(r.Context(), `SELECT data, server_revision FROM records
			WHERE record_type = ? AND record_id = ? AND deleted_at IS NULL`,
			recordType, recordID).Scan(&data, &rec.ServerRevision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rec.Data = json.RawMessage(data)
		found = true
		return nil
	})
	if err != nil {
		s.respond(w, "load record", err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "record not found")
		return
	}
	writeJSON(w, rec)
}

// handleMerge is the authoritative PR state transition (spec §12 step 12):
// open -> merged exactly once, serialized by the repository database.
func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	prID := r.PathValue("id")
	req, ok := decode[api.MergeRequest](w, r)
	if !ok {
		return
	}
	if req.MergeCommitSHA == "" || req.HeadCommitSHA == "" {
		writeErr(w, http.StatusBadRequest, "validation", "head_commit_sha and merge_commit_sha are required")
		return
	}
	ctx := r.Context()
	var resp api.MergeResponse
	var conflictMsg string

	err := s.Repos.Update(ctx, req.RepositoryID, false, func(tx *sql.Tx) error {
		conflictMsg = ""
		var data string
		err := tx.QueryRowContext(ctx, `SELECT data FROM records
			WHERE record_type = 'pull_request' AND record_id = ? AND deleted_at IS NULL`, prID).Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			conflictMsg = "pull request not found"
			return nil
		}
		if err != nil {
			return err
		}
		var pr map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &pr); err != nil {
			return err
		}
		var status string
		json.Unmarshal(pr["status"], &status)
		if status != "open" {
			conflictMsg = "pull request is " + status
			return nil
		}
		if req.ExpectedHeadSHA != "" {
			var head string
			json.Unmarshal(pr["head_commit_sha"], &head)
			if head != "" && head != req.ExpectedHeadSHA {
				conflictMsg = "pull request head is " + head + ", expected " + req.ExpectedHeadSHA
				return nil
			}
		}

		now := records.Now()
		set := func(key, val string) {
			b, _ := json.Marshal(val)
			pr[key] = b
		}
		set("status", "merged")
		set("merge_commit_sha", req.MergeCommitSHA)
		set("head_commit_sha", req.HeadCommitSHA)
		set("merged_at", now)
		set("updated_at", now)
		merged, _ := json.Marshal(pr)

		rev, err := nextRevision(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE records SET data = ?, server_revision = ?, updated_at = ?
			WHERE record_type = 'pull_request' AND record_id = ?`,
			string(merged), rev, now, prID); err != nil {
			return err
		}
		if err := setFieldRevisions(ctx, tx, "pull_request", prID,
			[]string{"status", "merge_commit_sha", "head_commit_sha", "merged_at"}, rev); err != nil {
			return err
		}
		resp = api.MergeResponse{
			Record:         api.Record{RecordType: "pull_request", RecordID: prID, Data: merged, ServerRevision: rev},
			ServerRevision: rev,
		}
		return nil
	})
	if err != nil {
		s.respond(w, "merge", err)
		return
	}
	if conflictMsg == "pull request not found" {
		writeErr(w, http.StatusNotFound, "not_found", conflictMsg)
		return
	}
	if conflictMsg != "" {
		writeErr(w, http.StatusConflict, "conflict", conflictMsg)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) handleUploadURL(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.UploadURLRequest](w, r)
	if !ok {
		return
	}
	if len(req.SHA256) != 64 || req.SizeBytes < 0 {
		writeErr(w, http.StatusBadRequest, "validation", "sha256 and size_bytes are required")
		return
	}
	ctx := r.Context()
	key := "sha256/" + req.SHA256[:2] + "/" + req.SHA256

	var alreadyStored bool
	err := s.Repos.Update(ctx, req.RepositoryID, false, func(tx *sql.Tx) error {
		var stored bool
		err := tx.QueryRowContext(ctx, `SELECT stored FROM blobs WHERE sha256 = ?`, req.SHA256).Scan(&stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		alreadyStored = stored
		if stored {
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO blobs (sha256, size_bytes, storage_key, created_at)
			VALUES (?, ?, ?, ?) ON CONFLICT (sha256) DO NOTHING`,
			req.SHA256, req.SizeBytes, key, records.Now())
		return err
	})
	if err != nil {
		s.respond(w, "record blob", err)
		return
	}
	if alreadyStored {
		writeJSON(w, api.UploadURLResponse{StorageKey: key, AlreadyStored: true})
		return
	}
	url, err := s.Blobs.SignedPutURL(ctx, key, req.MediaType)
	if err != nil {
		s.internal(w, "sign upload url", err)
		return
	}
	writeJSON(w, api.UploadURLResponse{URL: url, StorageKey: key})
}

// handleConfirmUpload verifies the blob landed in object storage, marks it
// stored, and stamps storage_key onto every artifact record with that hash
// (bumping their revisions so all clients learn the cloud copy exists).
func (s *Server) handleConfirmUpload(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.UploadURLRequest](w, r)
	if !ok {
		return
	}
	if len(req.SHA256) != 64 {
		writeErr(w, http.StatusBadRequest, "validation", "sha256 is required")
		return
	}
	key := "sha256/" + req.SHA256[:2] + "/" + req.SHA256
	exists, err := s.Blobs.Exists(r.Context(), key)
	if err != nil {
		s.internal(w, "check blob", err)
		return
	}
	if !exists {
		writeErr(w, http.StatusConflict, "conflict", "blob not found in object storage")
		return
	}
	ctx := r.Context()
	err = s.Repos.Update(ctx, req.RepositoryID, false, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE blobs SET stored = 1 WHERE sha256 = ?`, req.SHA256); err != nil {
			return err
		}
		var needsStamp bool
		err := tx.QueryRowContext(ctx, `SELECT true FROM records
			WHERE record_type = 'artifact' AND data->>'sha256' = ?
			AND COALESCE(data->>'storage_key', '') = '' LIMIT 1`, req.SHA256).Scan(&needsStamp)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rev, err := nextRevision(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE records
			SET data = json_set(data, '$.storage_key', ?), server_revision = ?, updated_at = ?
			WHERE record_type = 'artifact' AND data->>'sha256' = ?
			AND COALESCE(data->>'storage_key', '') = ''`,
			key, rev, records.Now(), req.SHA256)
		return err
	})
	if err != nil {
		s.respond(w, "confirm upload", err)
		return
	}
	writeJSON(w, api.UploadURLResponse{StorageKey: key, AlreadyStored: true})
}

func (s *Server) handleDownloadURL(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.DownloadURLRequest](w, r)
	if !ok {
		return
	}
	url, err := s.Blobs.SignedGetURL(r.Context(), req.StorageKey)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "blob not available: "+err.Error())
		return
	}
	writeJSON(w, api.DownloadURLResponse{URL: url})
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	if s.Log != nil {
		s.Log.Error(what, "error", err)
	}
	writeErr(w, http.StatusInternalServerError, "internal", what+" failed")
}
