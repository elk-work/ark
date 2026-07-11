package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/ijroth/ark/internal/records"
)

// Artifact is a generated or attached file, content-addressed by SHA-256.
// See docs/v1-spec.md §6.9.
type Artifact struct {
	ID            string `json:"id"`
	RepositoryID  string `json:"repository_id"`
	ParentType    string `json:"parent_type"`
	ParentID      string `json:"parent_id"`
	Name          string `json:"name"`
	MediaType     string `json:"media_type"`
	SizeBytes     int64  `json:"size_bytes"`
	SHA256        string `json:"sha256"`
	LocalPath     string `json:"local_path,omitempty"`
	StorageKey    string `json:"storage_key,omitempty"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by"`
	CreatedByType string `json:"created_by_type"`
}

// AddArtifact copies the file at srcPath into the content-addressed object
// store (.ark/objects/sha256/<xx>/<hash>) and records its metadata. The file
// write commits before the metadata transaction; an orphaned object is
// harmless and can be garbage-collected later.
func (s *Store) AddArtifact(ctx context.Context, arkDir, srcPath, parentType, parentID, name string) (*Artifact, error) {
	if err := records.OneOf("parent type", parentType, records.ArtifactParents); err != nil {
		return nil, err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return nil, records.NotFoundf("cannot read %s: %v", srcPath, err)
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil || info.IsDir() {
		return nil, records.Validationf("%s is not a regular file", srcPath)
	}

	tmpDir := filepath.Join(arkDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, &records.Error{Kind: records.KindGeneral, Message: "create tmp dir", Err: err}
	}
	tmp, err := os.CreateTemp(tmpDir, "artifact-*")
	if err != nil {
		return nil, &records.Error{Kind: records.KindGeneral, Message: "create temp file", Err: err}
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		tmp.Close()
		return nil, &records.Error{Kind: records.KindGeneral, Message: "copy artifact", Err: err}
	}
	if err := tmp.Close(); err != nil {
		return nil, &records.Error{Kind: records.KindGeneral, Message: "write artifact", Err: err}
	}
	sum := hex.EncodeToString(h.Sum(nil))

	rel := filepath.Join("objects", "sha256", sum[:2], sum)
	dst := filepath.Join(arkDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, &records.Error{Kind: records.KindGeneral, Message: "create object dir", Err: err}
	}
	if _, err := os.Stat(dst); err != nil { // new content; identical content reuses the object
		if err := os.Rename(tmpName, dst); err != nil {
			return nil, &records.Error{Kind: records.KindGeneral, Message: "store artifact", Err: err}
		}
	}

	if name == "" {
		name = filepath.Base(srcPath)
	}
	mediaType := mime.TypeByExtension(filepath.Ext(name))
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	a := &Artifact{
		ID:            records.NewID(),
		RepositoryID:  s.RepoID,
		ParentType:    parentType,
		ParentID:      parentID,
		Name:          name,
		MediaType:     mediaType,
		SizeBytes:     size,
		SHA256:        sum,
		LocalPath:     rel,
		CreatedAt:     records.Now(),
		CreatedBy:     s.Actor.ID,
		CreatedByType: string(s.Actor.Type),
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO artifacts
			(id, repository_id, parent_type, parent_id, name, media_type, size_bytes, sha256,
			 local_path, created_at, created_by, created_by_type, sync_state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'local')`,
			a.ID, a.RepositoryID, a.ParentType, a.ParentID, a.Name, a.MediaType,
			a.SizeBytes, a.SHA256, a.LocalPath, a.CreatedAt, a.CreatedBy, a.CreatedByType); err != nil {
			return records.DBErr("create artifact", err)
		}
		return s.logMutation(tx, records.TypeArtifact, a.ID, "create", 0, a)
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// ResolveArtifact loads an artifact by ULID or prefix.
func (s *Store) ResolveArtifact(ctx context.Context, ref string) (*Artifact, error) {
	id, err := s.resolveID(s.DB, "artifacts", ref)
	if err != nil {
		return nil, err
	}
	var a Artifact
	err = s.DB.QueryRowContext(ctx, `SELECT id, repository_id, parent_type, parent_id, name,
		media_type, size_bytes, sha256, local_path, storage_key, created_at, created_by, created_by_type
		FROM artifacts WHERE id = ?`, id).
		Scan(&a.ID, &a.RepositoryID, &a.ParentType, &a.ParentID, &a.Name, &a.MediaType,
			&a.SizeBytes, &a.SHA256, &a.LocalPath, &a.StorageKey, &a.CreatedAt, &a.CreatedBy, &a.CreatedByType)
	if err != nil {
		return nil, records.DBErr("load artifact", err)
	}
	return &a, nil
}

// ListArtifacts returns artifacts, optionally filtered to one parent.
func (s *Store) ListArtifacts(ctx context.Context, parentType, parentID string) ([]*Artifact, error) {
	q := `SELECT id, repository_id, parent_type, parent_id, name, media_type, size_bytes,
		sha256, local_path, storage_key, created_at, created_by, created_by_type
		FROM artifacts WHERE repository_id = ? AND deleted_at IS NULL`
	args := []any{s.RepoID}
	if parentType != "" {
		q += ` AND parent_type = ? AND parent_id = ?`
		args = append(args, parentType, parentID)
	}
	q += ` ORDER BY created_at`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, records.DBErr("list artifacts", err)
	}
	defer rows.Close()
	var out []*Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.RepositoryID, &a.ParentType, &a.ParentID, &a.Name,
			&a.MediaType, &a.SizeBytes, &a.SHA256, &a.LocalPath, &a.StorageKey,
			&a.CreatedAt, &a.CreatedBy, &a.CreatedByType); err != nil {
			return nil, records.DBErr("scan artifact", err)
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// CopyArtifact writes the stored object to destPath, verifying its checksum.
func (s *Store) CopyArtifact(a *Artifact, arkDir, destPath string) error {
	if a.LocalPath == "" {
		return records.NotFoundf("artifact %s has no local copy (cloud fetch is not available yet)", a.ID)
	}
	src, err := os.Open(filepath.Join(arkDir, a.LocalPath))
	if err != nil {
		return records.NotFoundf("artifact object missing: %v", err)
	}
	defer src.Close()
	dst, err := os.Create(destPath)
	if err != nil {
		return &records.Error{Kind: records.KindGeneral, Message: "create " + destPath, Err: err}
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, h), src); err != nil {
		dst.Close()
		return &records.Error{Kind: records.KindGeneral, Message: "copy artifact", Err: err}
	}
	if err := dst.Close(); err != nil {
		return &records.Error{Kind: records.KindGeneral, Message: "write " + destPath, Err: err}
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != a.SHA256 {
		return fmt.Errorf("checksum mismatch for artifact %s: stored object is corrupt", a.ID)
	}
	return nil
}

// ParseParentRef parses "task:12", "run:01ABC", "pr:3", "review:01XYZ" into
// a (parentType, ref) pair using Ark's canonical parent type names.
func ParseParentRef(ref string) (string, string, error) {
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", "", records.Validationf("parent must look like task:<n>, pr:<n>, run:<id>, or review:<id>")
	}
	switch parts[0] {
	case "task":
		return "task", parts[1], nil
	case "pr", "pull_request":
		return "pull_request", parts[1], nil
	case "run", "agent_run":
		return "agent_run", parts[1], nil
	case "review":
		return "review", parts[1], nil
	}
	return "", "", records.Validationf("unknown parent type %q", parts[0])
}
