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
// It is the only route that accepts ARK_BOOTSTRAP_TOKEN, and it is what makes
// per-principal credentials work with no identity provider anywhere: a
// self-hoster sets one random string, exactly as they set ARK_API_TOKEN today,
// and gets a real credential out. It is also the break-glass, which is why its
// authentication deliberately does not depend on auth.db being readable.
func (s *Server) handleCreatePrincipal(w http.ResponseWriter, r *http.Request) {
	if s.BootstrapToken == "" {
		writeErr(w, http.StatusUnauthorized, "permission",
			"bootstrap is not enabled on this service (ARK_BOOTSTRAP_TOKEN is unset)")
		return
	}
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(presented), []byte(s.BootstrapToken)) != 1 {
		writeErr(w, http.StatusUnauthorized, "permission", "invalid or missing bootstrap token")
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

	minted, err := s.authStore().createPrincipal(r.Context(), req)
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
		// The principal, never the credential (spec §21).
		s.Log.Info("principal credential issued", "principal", minted.Principal.ID,
			"credential", minted.CredentialID, "created", minted.Created)
	}
	writeJSON(w, api.CreatePrincipalResponse{
		Principal: api.Principal{
			ID:          minted.Principal.ID,
			Kind:        minted.Principal.Kind,
			Email:       minted.Principal.Email,
			DisplayName: minted.Principal.DisplayName,
			CreatedAt:   minted.Principal.CreatedAt,
		},
		Created:      minted.Created,
		Token:        minted.Token,
		CredentialID: minted.CredentialID,
		ExpiresAt:    minted.ExpiresAt,
	})
}
