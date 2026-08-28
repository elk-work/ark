package api

// Per-principal credentials (docs/rfc-0003-elk-issued-credentials.md, slices
// 1-2). These types describe one route, POST /v1/principals: the bootstrap
// path that gives a deployment its first real credential without an identity
// provider anywhere. Everything else about a credential travels in the
// Authorization header a client already sends.

// CreatePrincipalRequest asks the service to mint a principal and its first
// credential. It is authenticated by the service's ARK_BOOTSTRAP_TOKEN,
// presented as the bearer, and by nothing else.
type CreatePrincipalRequest struct {
	Email string `json:"email"`
	// DisplayName is what a human reads beside the principal. Optional; the
	// email is the identity.
	DisplayName string `json:"display_name,omitempty"`
	// Kind is "human" (the default) or "agent". It records what is holding
	// the credential; it confers nothing on its own.
	Kind string `json:"kind,omitempty"`
}

// Principal is who the service believes is making a request. Grants live
// beside it and are enforced separately (elk-work/ark#52).
type Principal struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// CreatePrincipalResponse carries the credential in plaintext exactly once.
// The service stores only its SHA-256, so a lost credential is reissued, not
// recovered.
type CreatePrincipalResponse struct {
	Principal Principal `json:"principal"`
	// Created distinguishes a new principal from a fresh credential minted
	// for one that already held the email — the break-glass case, which the
	// bootstrap token is entitled to either way.
	Created bool `json:"created"`
	// Token is the `arkc_…` credential. It is never returned again, never
	// stored, and never logged.
	Token string `json:"token"`
	// CredentialID identifies the credential row without revealing the
	// credential: it is what a revocation names.
	CredentialID string `json:"credential_id"`
	ExpiresAt    string `json:"expires_at"`
}
