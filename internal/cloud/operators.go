package cloud

import (
	"context"
	"net/http"

	"github.com/elk-work/ark/pkg/api"
)

// The service-wide half of the client: the roster, your own credentials, and
// retiring one (docs/rfc-0003-elk-issued-credentials.md, Amendment 1;
// elk-work/ark#94). All three go through do(), so an operator-only refusal
// arrives as a records.Error of kind permission and lands on exit 5 (spec
// §22) with nothing to keep in step by hand.

// Principals lists every principal the service knows and the credentials each
// holds. Operator-only.
func (c *Client) Principals(ctx context.Context) (*api.PrincipalListResponse, error) {
	var resp api.PrincipalListResponse
	if err := c.do(ctx, http.MethodGet, "/v1/principals", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Credentials lists the caller's own credentials — the ids a revocation
// names, for the holder who never wrote one down.
func (c *Client) Credentials(ctx context.Context) (*api.CredentialListResponse, error) {
	var resp api.CredentialListResponse
	if err := c.do(ctx, http.MethodGet, "/v1/credentials", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RevokeCredential retires one credential by id. Revoking one already retired
// is a success that changes nothing; the response says which happened.
func (c *Client) RevokeCredential(ctx context.Context, id string) (*api.RevokeCredentialResponse, error) {
	var resp api.RevokeCredentialResponse
	if err := c.do(ctx, http.MethodPost, "/v1/credentials/"+id+"/revoke", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
