package cloud

import (
	"context"
	"net/http"

	"github.com/elk-work/ark/pkg/api"
)

// The repository-metadata half of the client: reading the service's copy of
// a repository's name, default branch and Git remote, and correcting it.
// Both go through do(), so the service's error codes reach the CLI as
// records.Error kinds and land on the exit codes in spec §22 — a validation
// fault as 2, an unregistered repository as 3 — with nothing to keep in step
// by hand.

// RepositoryMetadata reads the service's copy of a repository's metadata.
func (c *Client) RepositoryMetadata(ctx context.Context, repoID string) (*api.RepositoryMetadata, error) {
	var meta api.RepositoryMetadata
	if err := c.do(ctx, http.MethodGet, "/v1/repositories/"+repoID, nil, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// SetRepositoryMetadata corrects one or more metadata fields. Fields left nil
// in the request are not asserted and keep whatever the service holds.
func (c *Client) SetRepositoryMetadata(ctx context.Context, repoID string, req api.SetRepositoryMetadataRequest) (*api.RepositoryResponse, error) {
	var resp api.RepositoryResponse
	if err := c.do(ctx, http.MethodPost, "/v1/repositories/"+repoID+"/metadata", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
