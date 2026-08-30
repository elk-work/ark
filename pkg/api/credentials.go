package api

// Service-wide acts: listing principals and retiring a credential
// (docs/rfc-0003-elk-issued-credentials.md, Amendment 1 — elk-work/ark#94,
// D116). Both were left out of the per-repository grants of Decision 4
// because neither is *about* a repository, and until D116 there was no
// identity entitled to perform one.
//
// The identity is an **operator**: an ordinary principal carrying
// `operator_since`. It is named, it holds a credential of its own, and that
// credential can be revoked — which is the whole difference between this and
// widening a shared secret.

// Credential is one row of the credentials table, minus the credential. Only
// a SHA-256 is stored, so nothing here can leak one: the id is what a
// revocation names, and recovering a forgotten id is what listing is for.
type Credential struct {
	ID          string `json:"id"`
	PrincipalID string `json:"principal_id"`
	// Label is how the credential was minted — "bootstrap" for the bootstrap
	// route, "device" for a device login. It is a note, not a permission.
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	// LastUsedOn is a date, not a timestamp: usage is recorded at day
	// granularity so that observing it does not put a write on every request.
	LastUsedOn string `json:"last_used_on,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
	// RevokedBy names the principal that retired it — the operator, or the
	// holder revoking their own. Empty on a credential nobody has revoked.
	RevokedBy string `json:"revoked_by,omitempty"`
}

// PrincipalRecord is one principal and the credentials it holds. It is what
// `GET /v1/principals` returns per row, and the reason that route is an
// operator act: a roster of everyone the service knows, and of what each of
// them could present.
type PrincipalRecord struct {
	Principal   Principal    `json:"principal"`
	Credentials []Credential `json:"credentials"`
}

// PrincipalListResponse is the whole roster, in email order.
type PrincipalListResponse struct {
	Principals []PrincipalRecord `json:"principals"`
}

// CredentialListResponse is `GET /v1/credentials`: the credentials of whoever
// is asking, and nobody else's.
//
// It exists so the self-service half of revocation is usable. A credential is
// revoked by id, and a person whose laptop held one has by definition not
// written that id down — so without this, "revoke my own credential" would be
// exactly as unreachable as revocation was before elk-work/ark#94.
type CredentialListResponse struct {
	PrincipalID string       `json:"principal_id"`
	Credentials []Credential `json:"credentials"`
}

// RevokeCredentialResponse answers `POST /v1/credentials/{id}/revoke`.
type RevokeCredentialResponse struct {
	Credential Credential `json:"credential"`
	// AlreadyRevoked reports that the credential was retired before this
	// call. Revoking twice is a success — the state the caller asked for
	// already holds — and saying which of the two happened is what keeps that
	// from being indistinguishable from having just done it.
	AlreadyRevoked bool `json:"already_revoked,omitempty"`
}
