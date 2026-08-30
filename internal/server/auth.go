package server

// The request-path half of per-principal credentials
// (docs/rfc-0003-elk-issued-credentials.md, slices 1-2): a dual-path verifier
// and the one route that mints the first credential.
//
// Stage 1 of the RFC's migration is "dual verification, no behaviour change",
// and that is the bar this file has to clear. The legacy service token is
// tried first, is compared exactly as it was before, and never touches
// auth.db — so a fleet running on ARK_API_TOKEN keeps working even if the
// credential store is absent, stale, corrupt, or unreachable. Only a bearer
// carrying the `arkc_` prefix reaches the store at all.

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/elk-work/ark/pkg/api"
)

// legacyPrincipalID names the synthesized principal behind the shared service
// token. It is not a row in auth.db and never will be: it exists so every
// route downstream can ask "who is this?" and get an answer during the
// migration, and so the answer is greppable in the logs when elk-work/ark#54
// asks who has not moved yet.
const legacyPrincipalID = "legacy"

// legacyPrincipalKind is the kind reported for that principal.
const legacyPrincipalKind = "legacy"

// authenticated is the identity the service resolved for one request.
type authenticated struct {
	ID           string
	Kind         string
	Email        string
	CredentialID string
	// Operator is set on a principal entitled to the two service-wide acts:
	// listing principals and revoking any credential (elk-work/ark#94, D116).
	// It is never set for the legacy service token — see operators.go for why
	// the one string the whole fleet holds is deliberately not an authority
	// over the service.
	Operator bool
	// Legacy marks the shared service token. It carries implicit write on
	// every repository, which is exactly what the token does today; when
	// grants are enforced (elk-work/ark#52) this is the flag that says "skip
	// the grant lookup", and when the token retires (#54) the branch that
	// sets it is simply not registered.
	Legacy bool
}

type principalCtxKey struct{}

// withPrincipal attaches the authenticated identity to a request context.
func withPrincipal(ctx context.Context, who *authenticated) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, who)
}

// principalFrom returns the identity behind a request. Every handler wrapped
// in s.auth has one, and grants.go is what decides with it.
func principalFrom(ctx context.Context) (*authenticated, bool) {
	who, ok := ctx.Value(principalCtxKey{}).(*authenticated)
	return who, ok
}

// principalOf is principalFrom for the handlers that have already been
// through s.allow, which refuses a request carrying no principal. It can
// still answer nil — for a route registered without s.auth, which would be a
// bug — and every caller treats nil as "bind nothing, own nothing" rather
// than as authority.
func principalOf(r *http.Request) *authenticated {
	who, _ := principalFrom(r.Context())
	return who
}

// authStore returns the credential store, opening it on first use. Server is
// built as a struct literal everywhere, so there is no constructor to do this
// in — and a deployment that never mints a principal should never pay for it.
func (s *Server) authStore() *authStore {
	s.authOnce.Do(func() {
		s.auths = newAuthStore(s.Repos.Backend, s.Repos.CacheDir, s.Log)
	})
	return s.auths
}

// authenticate resolves the bearer on a request, in the order that keeps the
// fleet working: legacy token, then credential, then no.
func (s *Server) authenticate(r *http.Request) (*authenticated, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	// The legacy comparison first, and without reading auth.db. Every client
	// in the fleet presents this string, and the credential store is a new
	// single point of contention (RFC-0003 "Costs accepted") — it must not
	// become one for the path that already worked.
	if s.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) == 1 {
		return &authenticated{ID: legacyPrincipalID, Kind: legacyPrincipalKind, Legacy: true}, nil
	}
	// Anything without the prefix is not a credential this service minted, so
	// there is nothing to look up. This is what keeps a stream of bad bearers
	// from turning into a stream of object fetches.
	if !strings.HasPrefix(tok, credentialPrefix) {
		return nil, errNoCredential
	}
	return s.authStore().verify(r.Context(), tok)
}

// auth wraps a handler in authentication. The wire contract it enforces is
// unchanged: 401 with a `permission` error code, which the client maps to exit
// code 5 (spec §22).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, err := s.authenticate(r)
		if err != nil {
			// A store that cannot be read is not a bad credential, and saying
			// so would send the holder to rotate a token that is fine.
			if !isAuthRejection(err) {
				s.internal(w, "authenticate", err)
				return
			}
			writeErr(w, http.StatusUnauthorized, "permission", rejectionMessage(err))
			return
		}
		if s.Log != nil {
			// One line per authenticated request, naming the principal. This
			// is the observability elk-work/ark#54 retires the legacy token
			// on: `last_used_on` says who has moved, and a stream of
			// principal=legacy says who has not.
			s.Log.Info("authenticated", "principal", who.ID, "kind", who.Kind,
				"method", r.Method, "path", r.URL.Path)
		}
		next(w, r.WithContext(withPrincipal(r.Context(), who)))
	}
}

// isAuthRejection separates "we are refusing this caller" from "we could not
// find out".
func isAuthRejection(err error) bool {
	return errors.Is(err, errNoCredential) ||
		errors.Is(err, errCredentialRevoked) ||
		errors.Is(err, errCredentialExpired) ||
		errors.Is(err, errPrincipalDisabled)
}

// rejectionMessage keeps V1's wording for an unrecognised bearer — clients
// have been reading it since the service shipped, and the client appends its
// own "run `ark login`" guidance to it — while naming the cause when the
// service does recognise the credential and is refusing it anyway.
func rejectionMessage(err error) string {
	switch {
	case errors.Is(err, errCredentialRevoked):
		return "credential revoked"
	case errors.Is(err, errCredentialExpired):
		return "credential expired"
	case errors.Is(err, errPrincipalDisabled):
		return "principal disabled"
	default:
		return "invalid or missing token"
	}
}

// handleCreatePrincipal mints a principal and its first credential.
//
// It is the only route that accepts ARK_BOOTSTRAP_TOKEN — Decision 6, and
// unchanged by elk-work/ark#94 — and it is what makes per-principal
// credentials work with no identity provider anywhere: a self-hoster sets one
// random string, exactly as they set ARK_API_TOKEN today, and gets a real
// credential out. It is also the break-glass, which is why the bootstrap
// branch deliberately does not depend on auth.db being readable.
//
// Since #94 it accepts a second bearer: an **operator's own credential**.
// That is not a change to Decision 6, which says where the bootstrap token is
// accepted and not what else that route may accept — and it is what lets
// operators be made by operators rather than by whoever holds an environment
// variable. Every other bearer is refused here exactly as before.
func (s *Server) handleCreatePrincipal(w http.ResponseWriter, r *http.Request) {
	who, viaBootstrap, ok := s.authenticateMint(w, r)
	if !ok {
		return
	}

	req, ok := decode[api.CreatePrincipalRequest](w, r)
	if !ok {
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" {
		writeErr(w, http.StatusBadRequest, "validation", "email is required")
		return
	}
	if req.Kind == "" {
		req.Kind = "human"
	}
	if req.Kind != "human" && req.Kind != "agent" {
		writeErr(w, http.StatusBadRequest, "validation", `kind must be "human" or "agent"`)
		return
	}
	if req.Operator && viaBootstrap {
		// Refused rather than quietly ignored, and the message states the
		// rule it is enforcing: the bootstrap token seeds the first operator
		// automatically and cannot appoint another, which is the whole reason
		// a shared secret is not an operator identity (operators.go).
		writeErr(w, http.StatusForbidden, "permission",
			"the bootstrap token cannot appoint an operator. The first principal on a service with no "+
				"operator becomes one automatically; after that an operator adds another with their own "+
				"credential (`ark principal create --operator`)")
		return
	}

	// Bootstrap promotes only into a vacuum. Asked here as well as inside the
	// transaction so the log line can say which rule fired; the transaction is
	// what actually decides, because it is the copy being written.
	promoteIfFirst := false
	if viaBootstrap {
		has, err := s.authStore().hasOperator(r.Context())
		if err != nil {
			s.internal(w, "read operators", err)
			return
		}
		promoteIfFirst = !has
	}

	minted, err := s.authStore().createPrincipal(r.Context(), req, promoteIfFirst)
	if errors.Is(err, errPrincipalDisabled) {
		writeErr(w, http.StatusConflict, "conflict",
			"a disabled principal already holds that email; re-enable it rather than reissuing")
		return
	}
	if err != nil {
		s.internal(w, "create principal", err)
		return
	}
	if s.Log != nil {
		// The principal, never the credential (spec §21). `issued_by` is the
		// named operator where there was one and the bootstrap token where
		// there was not — the second only ever appears on a service that had
		// no operator to name.
		issuedBy := "bootstrap-token"
		if !viaBootstrap {
			issuedBy = who.ID
		}
		s.Log.Info("principal credential issued", "principal", minted.Principal.ID,
			"credential", minted.CredentialID, "created", minted.Created,
			"issued_by", issuedBy, "promoted_to_operator", minted.Promoted,
			"first_operator", minted.Promoted && viaBootstrap)
	}
	writeJSON(w, api.CreatePrincipalResponse{
		Principal:    apiPrincipal(minted.Principal),
		Created:      minted.Created,
		Token:        minted.Token,
		CredentialID: minted.CredentialID,
		ExpiresAt:    minted.ExpiresAt,
	})
}

// authenticateMint resolves who may mint on POST /v1/principals: the
// bootstrap token, or an operator's credential. It writes its own refusal.
//
// The bootstrap comparison comes first and is unchanged, so a deployment
// whose auth.db is absent, stale or unreachable can still reach a credential
// — the property that makes this route the break-glass.
func (s *Server) authenticateMint(w http.ResponseWriter, r *http.Request) (*authenticated, bool, bool) {
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.BootstrapToken != "" &&
		subtle.ConstantTimeCompare([]byte(presented), []byte(s.BootstrapToken)) == 1 {
		return nil, true, true
	}
	// Only an `arkc_` bearer can be an operator's credential, so nothing else
	// costs a read of the credential store — the same rule s.authenticate
	// follows, and for the same reason.
	if strings.HasPrefix(presented, credentialPrefix) {
		who, err := s.authStore().verify(r.Context(), presented)
		switch {
		case err == nil && who.Operator:
			return who, false, true
		case err == nil:
			writeErr(w, http.StatusForbidden, "permission",
				principalLabel(who)+" is not an operator, and minting a principal is an operator act. "+
					"The service's ARK_BOOTSTRAP_TOKEN mints one without an operator")
			return nil, false, false
		case !isAuthRejection(err):
			s.internal(w, "authenticate", err)
			return nil, false, false
		default:
			writeErr(w, http.StatusUnauthorized, "permission", rejectionMessage(err))
			return nil, false, false
		}
	}
	if s.BootstrapToken == "" {
		writeErr(w, http.StatusUnauthorized, "permission",
			"bootstrap is not enabled on this service (ARK_BOOTSTRAP_TOKEN is unset)")
		return nil, false, false
	}
	writeErr(w, http.StatusUnauthorized, "permission", "invalid or missing bootstrap token")
	return nil, false, false
}
