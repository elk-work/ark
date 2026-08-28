package api

// The device-authorization flow (docs/rfc-0003-elk-issued-credentials.md,
// Decision 3; docs/v1-spec.md §20.1). Three routes let a person log in from a
// machine with no browser on it: the client asks for a code, prints it, and
// polls; an identity provider verifies who is at the browser and approves the
// code server-to-server.
//
// The banner types live here too, because the `auth` object on GET / exists
// only to tell a client whether this flow is available — that is the whole
// discovery mechanism, and there is no client configuration behind it.

// DeviceCodeResponse is what POST /v1/device/code returns. It is
// unauthenticated: it hands out a code, not access.
type DeviceCodeResponse struct {
	// DeviceCode is the secret half, seen only by the process that asked for
	// it. Possession of it is what authenticates the poll, so it is never
	// displayed, never logged, and stored only as a SHA-256.
	DeviceCode string `json:"device_code"`
	// UserCode is the half a person reads aloud and types into a browser,
	// rendered XXXX-XXXX. Eight characters of Crockford base32 with the
	// vowels dropped: no confusable pairs and no accidental words.
	UserCode string `json:"user_code"`
	// VerificationURI is where the person goes. VerificationURIComplete is
	// the same page with the code already in the query string, for a client
	// that can open a browser or render a link.
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	// ExpiresIn and Interval are seconds: how long the code lives, and how
	// often the client may poll. Polling faster earns DeviceCodeSlowDown.
	ExpiresIn int `json:"expires_in"`
	Interval  int `json:"interval"`
}

// DeviceTokenRequest polls for the credential. The device code is the only
// authentication the route has, and the only one it needs.
type DeviceTokenRequest struct {
	DeviceCode string `json:"device_code"`
}

// DeviceTokenResponse carries the credential in plaintext exactly once, on
// the one poll that redeems the code. The row is deleted in the same
// transaction, so a replayed poll is answered DeviceCodeExpired rather than
// with a second copy.
type DeviceTokenResponse struct {
	Token     string    `json:"token"`
	Principal Principal `json:"principal"`
}

// Device-flow error codes, carried in Error.Code beside the status. Both
// sides name them from here rather than from string literals that can drift
// apart in silence.
const (
	// DeviceCodePending — 428. Nobody has approved this code yet.
	DeviceCodePending = "pending"
	// DeviceCodeSlowDown — 429. The client polled faster than Interval.
	DeviceCodeSlowDown = "slow_down"
	// DeviceCodeExpired — 410. Past ExpiresIn, already redeemed, or never
	// issued. The three are deliberately one answer: telling them apart
	// would say whether a guessed device code had ever existed.
	DeviceCodeExpired = "expired"
)

// DeviceApproveRequest is the identity provider's assertion, presented
// server-to-server with ARK_IDP_KEY as the bearer. The browser never talks to
// ark-server, so there is no CORS surface and no CSRF surface.
type DeviceApproveRequest struct {
	// UserCode is what the person typed. It is normalised on arrival —
	// case, spacing, hyphen, and Crockford's own I/L→1 and O→0 — so a code
	// read off a terminal and retyped by hand resolves.
	UserCode string `json:"user_code"`
	// Subject is the identity provider's stable id for this person. It is
	// recorded, never matched on: the email is the identity (RFC-0003
	// Decision 4), the same choice RFC-0002 made for the Elk actor map.
	Subject string `json:"subject"`
	// Email is the verified address. Whoever controls it at the identity
	// provider gets that principal's grants; that is the accepted trust
	// model, recorded in the RFC's "Costs accepted".
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	// RepositoryIDs are ark repository ids this principal may read — the
	// seeded grants. The identity provider computes the list on its own
	// side; ark never learns what it was computed from, and nothing on the
	// request path calls back. Seeding runs on every login, only ever adds
	// `read`, and never removes a grant.
	RepositoryIDs []string `json:"repository_ids,omitempty"`
}

// ServiceBanner is the unauthenticated GET / response.
type ServiceBanner struct {
	Service string       `json:"service"`
	API     string       `json:"api"`
	Version string       `json:"version,omitempty"`
	Auth    *ServiceAuth `json:"auth,omitempty"`
}

// ServiceAuth tells a client how it may log in. A service that offers no
// device flow says so here, which is how `ark login` knows to ask for a token
// instead of printing a code nobody can approve.
type ServiceAuth struct {
	DeviceFlow bool `json:"device_flow"`
	// ApprovalURL is where a person approves a device code. Empty when
	// DeviceFlow is false.
	ApprovalURL string `json:"approval_url,omitempty"`
}
