package api

// Per-repository grants (docs/rfc-0003-elk-issued-credentials.md, Decision 4,
// slice 3). A grant says what one principal may do in one repository:
// `read` pulls, `write` pushes, `admin` also grants and corrects metadata.
//
// Nothing here is on the sync path. Enforcement rides entirely on the bearer
// a client already sends and on `Mutation.CreatedBy`, an existing field the
// service had simply never read — so the sync protocol is unchanged and no
// client needs a new version to be governed by a grant.

// GrantLevel values, in ascending order of authority. They are the whole
// vocabulary: three levels on a repository, not a matrix (Principle 005).
const (
	GrantRead  = "read"
	GrantWrite = "write"
	GrantAdmin = "admin"
)

// SetGrantRequest is the body of POST /v1/repositories/{repo}/grants.
//
// It is keyed on **email**, never on a principal id, so a grant can be
// issued to somebody who has never authenticated: the row is resolved to a
// principal the first time that address logs in. It is the same choice
// RFC-0002 made for the Elk actor map, and it is what keeps a credential
// from ever being passed person-to-person.
type SetGrantRequest struct {
	Email string `json:"email"`
	// Level is read, write, or admin. Empty with Revoke set removes the
	// grant instead.
	Level string `json:"level,omitempty"`
	// Revoke removes whatever the email holds on this repository. It is a
	// field rather than a DELETE route because the pending half of a grant
	// is addressed by email too, and a URL cannot carry one safely.
	Revoke bool `json:"revoke,omitempty"`
}

// Grant is one row of the answer: who holds what on a repository.
type Grant struct {
	Email string `json:"email"`
	Level string `json:"level"`
	// PrincipalID is empty while the grant is still keyed only on the
	// email — issued to somebody who has not logged in yet.
	PrincipalID string `json:"principal_id,omitempty"`
	GrantedBy   string `json:"granted_by,omitempty"`
	GrantedAt   string `json:"granted_at,omitempty"`
	// Pending reports a grant waiting for its grantee's first login. It is
	// enforced from the moment it resolves and not before, so a caller that
	// prints a grant should say which of the two it is looking at.
	Pending bool `json:"pending,omitempty"`
}

// GrantResponse is what setting or revoking one grant answers with.
type GrantResponse struct {
	Grant Grant `json:"grant"`
	// Revoked reports that the grant was removed rather than written.
	Revoked bool `json:"revoked,omitempty"`
}

// GrantListResponse lists every grant on one repository, resolved and
// pending alike, in email order.
type GrantListResponse struct {
	RepositoryID string  `json:"repository_id"`
	Grants       []Grant `json:"grants"`
}
