// Package sync orchestrates a client synchronization: register, push
// pending mutations, upload artifact blobs, and pull accepted records.
// See docs/v1-spec.md §9.
package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/elkproject/ark/internal/app"
	"github.com/elkproject/ark/internal/cloud"
	"github.com/elkproject/ark/internal/records"
	"github.com/elkproject/ark/internal/store"
	"github.com/elkproject/ark/pkg/api"
)

// Issue is one rejected or conflicted mutation, for display.
type Issue struct {
	MutationID string `json:"mutation_id"`
	RecordType string `json:"record_type"`
	RecordID   string `json:"record_id"`
	Status     string `json:"status"` // rejected|conflict
	Error      string `json:"error"`
}

// Result summarizes one sync.
type Result struct {
	Pushed            int     `json:"pushed"`
	Applied           int     `json:"applied"`
	Rejected          int     `json:"rejected"`
	Conflicts         int     `json:"conflicts"`
	ArtifactsUploaded int     `json:"artifacts_uploaded"`
	PulledRecords     int     `json:"pulled_records"`
	PulledTombstones  int     `json:"pulled_tombstones"`
	ServerRevision    int64   `json:"server_revision"`
	Issues            []Issue `json:"issues,omitempty"`

	// SkippedRecords counts pulled records this build cannot represent,
	// broken out by record type. Server and client versions skew by design,
	// so this is information, not an error — but it is data the operator
	// cannot see locally, so it is reported rather than dropped silently.
	SkippedRecords map[string]int `json:"skipped_records,omitempty"`
}

// Client builds the cloud client for a repository, or an offline error when
// no remote is configured.
func Client(a *app.Context) (*cloud.Client, error) {
	if a.Config.Remote == "" {
		return nil, records.Offlinef("no Ark remote configured (run `ark remote set <url>`)")
	}
	return cloud.New(a.Config.Remote)
}

// Run performs a full synchronization.
func Run(ctx context.Context, a *app.Context) (*Result, error) {
	client, err := Client(a)
	if err != nil {
		return nil, err
	}
	res := &Result{Issues: []Issue{}}

	// Registration is idempotent and cheap; it also validates the token.
	if err := client.RegisterRepo(ctx, api.RegisterRepositoryRequest{
		ID:            a.Config.RepositoryID,
		Name:          filepath.Base(a.Root),
		DefaultBranch: a.Git.DefaultBranch(ctx),
		GitRemoteURL:  a.Git.RemoteURL(ctx, "origin"),
	}); err != nil {
		return nil, err
	}

	if err := Push(ctx, a, client, res); err != nil {
		return nil, err
	}
	if err := uploadArtifacts(ctx, a, client, res); err != nil {
		return nil, err
	}
	if err := Pull(ctx, a, client, res); err != nil {
		return nil, err
	}
	return res, nil
}

// Push sends the pending mutation queue and applies the server's verdicts.
func Push(ctx context.Context, a *app.Context, client *cloud.Client, res *Result) error {
	muts, err := a.Store.PendingMutationRows(ctx)
	if err != nil {
		return err
	}
	if len(muts) == 0 {
		return nil
	}
	actors, err := a.Store.AllActors(ctx)
	if err != nil {
		return err
	}
	clientID, _, err := a.Store.SyncCursor(ctx)
	if err != nil {
		return err
	}

	resp, err := client.Push(ctx, api.PushRequest{
		RepositoryID: a.Config.RepositoryID,
		ClientID:     clientID,
		Actors:       actors,
		Mutations:    muts,
	})
	if err != nil {
		return err
	}
	res.Pushed += len(muts)
	res.ServerRevision = resp.ServerRevision

	byID := map[string]api.Mutation{}
	for _, m := range muts {
		byID[m.ID] = m
	}
	for _, out := range resp.Applied {
		if err := a.Store.MarkMutation(ctx, out.MutationID, "applied", ""); err != nil {
			return err
		}
		if m, ok := byID[out.MutationID]; ok && out.ServerRevision > 0 {
			if err := a.Store.SetRecordRevision(ctx, m.RecordType, m.RecordID, out.ServerRevision); err != nil {
				return err
			}
		}
		res.Applied++
	}
	for _, out := range resp.Rejected {
		if err := a.Store.MarkMutation(ctx, out.MutationID, "rejected", out.Error); err != nil {
			return err
		}
		res.Rejected++
		if m, ok := byID[out.MutationID]; ok {
			res.Issues = append(res.Issues, Issue{MutationID: out.MutationID,
				RecordType: m.RecordType, RecordID: m.RecordID, Status: "rejected", Error: out.Error})
		}
	}
	for _, out := range resp.Conflicts {
		if err := a.Store.MarkMutation(ctx, out.MutationID, "conflict", out.Error); err != nil {
			return err
		}
		if m, ok := byID[out.MutationID]; ok {
			if err := a.Store.RecordConflict(ctx, m, out.Remote); err != nil {
				return err
			}
			res.Issues = append(res.Issues, Issue{MutationID: out.MutationID,
				RecordType: m.RecordType, RecordID: m.RecordID, Status: "conflict", Error: out.Error})
		}
		res.Conflicts++
	}
	return nil
}

// Pull fetches records after the local cursor and applies them atomically.
func Pull(ctx context.Context, a *app.Context, client *cloud.Client, res *Result) error {
	_, after, err := a.Store.SyncCursor(ctx)
	if err != nil {
		return err
	}
	resp, err := client.Pull(ctx, api.PullRequest{
		RepositoryID:  a.Config.RepositoryID,
		AfterRevision: after,
	})
	if err != nil {
		return err
	}
	skips, err := a.Store.ApplyPull(ctx, resp)
	if err != nil {
		return err
	}
	for recordType, n := range skips {
		if res.SkippedRecords == nil {
			res.SkippedRecords = map[string]int{}
		}
		res.SkippedRecords[recordType] += n
	}
	res.PulledRecords += len(resp.Records)
	res.PulledTombstones += len(resp.Tombstones)
	res.ServerRevision = resp.ServerRevision
	return nil
}

// uploadArtifacts sends local artifact blobs the server has not stored.
func uploadArtifacts(ctx context.Context, a *app.Context, client *cloud.Client, res *Result) error {
	arts, err := a.Store.ListArtifacts(ctx, "", "")
	if err != nil {
		return err
	}
	for _, art := range arts {
		if art.LocalPath == "" || art.StorageKey != "" {
			continue
		}
		path := filepath.Join(a.ArkDir, art.LocalPath)
		if _, err := os.Stat(path); err != nil {
			continue // object missing locally; another client may hold it
		}
		req := api.UploadURLRequest{
			RepositoryID: a.Config.RepositoryID,
			SHA256:       art.SHA256,
			SizeBytes:    art.SizeBytes,
			MediaType:    art.MediaType,
		}
		urlResp, err := client.UploadURL(ctx, req)
		if err != nil {
			return fmt.Errorf("upload url for %s: %w", art.Name, err)
		}
		if !urlResp.AlreadyStored {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			err = client.PutBlob(ctx, urlResp.URL, art.MediaType, f, art.SizeBytes)
			f.Close()
			if err != nil {
				return fmt.Errorf("upload %s: %w", art.Name, err)
			}
		}
		if err := client.ConfirmUpload(ctx, req); err != nil {
			return fmt.Errorf("confirm %s: %w", art.Name, err)
		}
		res.ArtifactsUploaded++
	}
	return nil
}

// FetchArtifact downloads an artifact blob into the local object store and
// returns its local path.
func FetchArtifact(ctx context.Context, a *app.Context, art *store.Artifact) (string, error) {
	if art.StorageKey == "" {
		return "", records.NotFoundf("artifact %s has no cloud copy yet", art.ID)
	}
	client, err := Client(a)
	if err != nil {
		return "", err
	}
	urlResp, err := client.DownloadURL(ctx, api.DownloadURLRequest{
		RepositoryID: a.Config.RepositoryID,
		StorageKey:   art.StorageKey,
	})
	if err != nil {
		return "", err
	}

	rel := filepath.Join("objects", "sha256", art.SHA256[:2], art.SHA256)
	dst := filepath.Join(a.ArkDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Join(a.ArkDir, "tmp"), "download-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := client.GetBlob(ctx, urlResp.URL, tmp); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	if err := a.Store.SetArtifactLocalPath(ctx, art.ID, rel); err != nil {
		return "", err
	}
	art.LocalPath = rel
	return rel, nil
}
