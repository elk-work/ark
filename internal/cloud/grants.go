package cloud

import (
	"context"
	"net/http"

	"github.com/elk-work/ark/pkg/api"
)

// The grants half of the client: who may read, write, or administer a
// repository on the sync service (docs/rfc-0003-elk-issued-credentials.md,
// Decision 4). Both calls go through do(), so a refusal arrives as a
// records.Error of kind permission and lands on exit 5 (spec §22) with
// nothing to keep in step by hand — which is also how these two commands get
// the same treatment as every other route when the caller is not an admin.

// SetGrant issues or revokes one grant, keyed on the grantee's email.
func (c *Client) SetGrant(ctx context.Context, repoID string, req api.SetGrantRequest) (*api.GrantResponse, error) {
	var resp api.GrantResponse
	if err := c.do(ctx, http.MethodPost, "/v1/repositories/"+repoID+"/grants", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Grants lists every grant on a repository, resolved and pending alike.
func (c *Client) Grants(ctx context.Context, repoID string) (*api.GrantListResponse, error) {
	var resp api.GrantListResponse
	if err := c.do(ctx, http.MethodGet, "/v1/repositories/"+repoID+"/grants", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
