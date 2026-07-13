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
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ijroth/ark/internal/server/schema"
	"github.com/ijroth/ark/pkg/api"
)

// Server is the Ark sync service.
type Server struct {
	DB    *sql.DB
	Token string // single service token (spec §20: V1 begins with one)
	Blobs BlobStore
	Log   *slog.Logger
}

// Open connects to Postgres and ensures the schema exists.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema.SQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return db, nil
}

// Handler builds the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeErr(w, http.StatusNotFound, "not_found", "unknown route")
			return
		}
		writeJSON(w, map[string]string{"service": "ark-sync", "api": "v1"})
	})
	// Not /healthz: Google Frontend intercepts that path on run.app
	// hostnames and serves its own 404 before the request reaches the
	// container (verified empirically 2026-07-13).
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "db unreachable", http.StatusServiceUnavailable)
			return
		}
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
	_, err := s.DB.ExecContext(r.Context(), `INSERT INTO repositories
		(id, name, default_branch, git_remote_url) VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name,
			default_branch = EXCLUDED.default_branch, git_remote_url = EXCLUDED.git_remote_url`,
		req.ID, req.Name, req.DefaultBranch, req.GitRemoteURL)
	if err != nil {
		s.internal(w, "register repository", err)
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
	resp := api.PushResponse{
		Applied:   []api.MutationOutcome{},
		Rejected:  []api.MutationOutcome{},
		Conflicts: []api.MutationOutcome{},
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.internal(w, "begin push", err)
		return
	}
	defer tx.Rollback()

	// Lock the repository row: one push per repo at a time keeps revision
	// allocation and number reconciliation serial.
	var repoExists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM repositories WHERE id = $1 FOR UPDATE`,
		req.RepositoryID).Scan(&repoExists)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "repository not registered")
		return
	}
	if err != nil {
		s.internal(w, "lock repository", err)
		return
	}

	// Actors are upserted as records so every client can render names.
	for _, a := range req.Actors {
		if a.ID == "" {
			continue
		}
		if err := s.upsertActor(ctx, tx, req.RepositoryID, a); err != nil {
			s.internal(w, "upsert actor", err)
			return
		}
	}

	// Mutations apply in creation order; savepoint handling inside
	// processMutation keeps one bad mutation from poisoning the batch.
	session := sessionRevisions{}
	for _, m := range req.Mutations {
		out := processMutation(ctx, tx, req.RepositoryID, m, session)
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

	if err := tx.QueryRowContext(ctx, `SELECT revision FROM repositories WHERE id = $1`,
		req.RepositoryID).Scan(&resp.ServerRevision); err != nil {
		s.internal(w, "read revision", err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.internal(w, "commit push", err)
		return
	}
	writeJSON(w, resp)
}

// upsertActor stores an actor as a record, bumping the revision only for
// new actors so pulls stay quiet on repeat pushes.
func (s *Server) upsertActor(ctx context.Context, tx *sql.Tx, repoID string, a api.Actor) error {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT true FROM records
		WHERE repository_id = $1 AND record_type = 'actor' AND record_id = $2`,
		repoID, a.ID).Scan(&exists)
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
	rev, err := nextRevision(ctx, tx, repoID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO records
		(repository_id, record_type, record_id, data, server_revision)
		VALUES ($1, 'actor', $2, $3, $4)`, repoID, a.ID, string(data), rev)
	return err
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.PullRequest](w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	resp := api.PullResponse{Records: []api.Record{}, Tombstones: []api.Tombstone{}}

	err := s.DB.QueryRowContext(ctx, `SELECT revision FROM repositories WHERE id = $1`,
		req.RepositoryID).Scan(&resp.ServerRevision)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "repository not registered")
		return
	}
	if err != nil {
		s.internal(w, "read revision", err)
		return
	}

	rows, err := s.DB.QueryContext(ctx, `SELECT record_type, record_id, data, server_revision,
		to_char(deleted_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
		FROM records WHERE repository_id = $1 AND server_revision > $2
		ORDER BY server_revision`, req.RepositoryID, req.AfterRevision)
	if err != nil {
		s.internal(w, "query records", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var rec api.Record
		var deletedAt sql.NullString
		if err := rows.Scan(&rec.RecordType, &rec.RecordID, &rec.Data, &rec.ServerRevision, &deletedAt); err != nil {
			s.internal(w, "scan record", err)
			return
		}
		if deletedAt.Valid {
			resp.Tombstones = append(resp.Tombstones, api.Tombstone{
				RecordType: rec.RecordType, RecordID: rec.RecordID,
				DeletedAt: deletedAt.String, ServerRevision: rec.ServerRevision})
		} else {
			resp.Records = append(resp.Records, rec)
		}
	}
	if err := rows.Err(); err != nil {
		s.internal(w, "read records", err)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) handleGetRecord(w http.ResponseWriter, r *http.Request) {
	repoID, recordType, recordID := r.PathValue("repo"), r.PathValue("type"), r.PathValue("id")
	var rec api.Record
	rec.RecordType, rec.RecordID = recordType, recordID
	err := s.DB.QueryRowContext(r.Context(), `SELECT data, server_revision FROM records
		WHERE repository_id = $1 AND record_type = $2 AND record_id = $3 AND deleted_at IS NULL`,
		repoID, recordType, recordID).Scan(&rec.Data, &rec.ServerRevision)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "record not found")
		return
	}
	if err != nil {
		s.internal(w, "load record", err)
		return
	}
	writeJSON(w, rec)
}

// handleMerge is the authoritative PR state transition (spec §12 step 12):
// open -> merged exactly once, serialized on the repository row.
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
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.internal(w, "begin merge", err)
		return
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT true FROM repositories WHERE id = $1 FOR UPDATE`,
		req.RepositoryID).Scan(&exists); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "repository not registered")
		return
	}

	var data json.RawMessage
	err = tx.QueryRowContext(ctx, `SELECT data FROM records
		WHERE repository_id = $1 AND record_type = 'pull_request' AND record_id = $2 AND deleted_at IS NULL`,
		req.RepositoryID, prID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "pull request not found")
		return
	}
	if err != nil {
		s.internal(w, "load pull request", err)
		return
	}
	var pr map[string]json.RawMessage
	if err := json.Unmarshal(data, &pr); err != nil {
		s.internal(w, "decode pull request", err)
		return
	}
	var status string
	json.Unmarshal(pr["status"], &status)
	if status != "open" {
		writeErr(w, http.StatusConflict, "conflict", "pull request is "+status)
		return
	}
	if req.ExpectedHeadSHA != "" {
		var head string
		json.Unmarshal(pr["head_commit_sha"], &head)
		if head != "" && head != req.ExpectedHeadSHA {
			writeErr(w, http.StatusConflict, "conflict",
				"pull request head is "+head+", expected "+req.ExpectedHeadSHA)
			return
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
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

	rev, err := nextRevision(ctx, tx, req.RepositoryID)
	if err != nil {
		s.internal(w, "allocate revision", err)
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE records SET data = $3, server_revision = $4, updated_at = now()
		WHERE repository_id = $1 AND record_type = 'pull_request' AND record_id = $2`,
		req.RepositoryID, prID, string(merged), rev); err != nil {
		s.internal(w, "update pull request", err)
		return
	}
	if err := setFieldRevisions(ctx, tx, req.RepositoryID, "pull_request", prID,
		[]string{"status", "merge_commit_sha", "head_commit_sha", "merged_at"}, rev); err != nil {
		s.internal(w, "update field revisions", err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.internal(w, "commit merge", err)
		return
	}
	writeJSON(w, api.MergeResponse{
		Record:         api.Record{RecordType: "pull_request", RecordID: prID, Data: merged, ServerRevision: rev},
		ServerRevision: rev,
	})
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

	var stored bool
	err := s.DB.QueryRowContext(ctx, `SELECT stored FROM blobs
		WHERE repository_id = $1 AND sha256 = $2`, req.RepositoryID, req.SHA256).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.internal(w, "check blob", err)
		return
	}
	if stored {
		writeJSON(w, api.UploadURLResponse{StorageKey: key, AlreadyStored: true})
		return
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO blobs (repository_id, sha256, size_bytes, storage_key)
		VALUES ($1, $2, $3, $4) ON CONFLICT (repository_id, sha256) DO NOTHING`,
		req.RepositoryID, req.SHA256, req.SizeBytes, key); err != nil {
		s.internal(w, "record blob", err)
		return
	}
	url, err := s.Blobs.SignedPutURL(ctx, key, req.MediaType)
	if err != nil {
		s.internal(w, "sign upload url", err)
		return
	}
	writeJSON(w, api.UploadURLResponse{URL: url, StorageKey: key})
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
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.internal(w, "begin confirm", err)
		return
	}
	defer tx.Rollback()
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT true FROM repositories WHERE id = $1 FOR UPDATE`,
		req.RepositoryID).Scan(&locked); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "repository not registered")
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE blobs SET stored = true
		WHERE repository_id = $1 AND sha256 = $2`, req.RepositoryID, req.SHA256); err != nil {
		s.internal(w, "mark blob stored", err)
		return
	}
	rev, err := nextRevision(ctx, tx, req.RepositoryID)
	if err != nil {
		s.internal(w, "allocate revision", err)
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE records
		SET data = jsonb_set(data, '{storage_key}', to_jsonb($3::text)), server_revision = $4, updated_at = now()
		WHERE repository_id = $1 AND record_type = 'artifact' AND data->>'sha256' = $2
		AND COALESCE(data->>'storage_key', '') = ''`,
		req.RepositoryID, req.SHA256, key, rev); err != nil {
		s.internal(w, "stamp storage key", err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.internal(w, "commit confirm", err)
		return
	}
	writeJSON(w, api.UploadURLResponse{StorageKey: key, AlreadyStored: true})
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	if s.Log != nil {
		s.Log.Error(what, "error", err)
	}
	writeErr(w, http.StatusInternalServerError, "internal", what+" failed")
}
