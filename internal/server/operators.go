package server

// Operators: who may act on the service as a whole
// (docs/rfc-0003-elk-issued-credentials.md, Amendment 1; elk-work/ark#94,
// ruled D116 on 2026-08-30).
//
// Two acts sat outside every authorization model this service had. Revoking a
// credential was reachable only by editing auth.db by hand, and listing
// principals was not reachable at all — which made revocation unusable even
// where it existed, because a credential is revoked by an id nobody wrote
// down. Neither is *about* a repository, so Decision 4's three levels had
// nothing to say about them, and Decision 6 confines ARK_BOOTSTRAP_TOKEN to
// one route, so the one service-wide secret could not be the answer either.
//
// **An operator is an ordinary principal with `operator_since` set.** Named,
// holding a credential of its own, and revocable by the same route it uses to
// revoke anybody else's. That is the property the alternative did not have:
// widening the bootstrap token would have made one environment variable the
// whole operator identity, so every service-wide act in the log would have
// been attributed to a string several people hold and nobody owns.
//
// # Why a column and not a kind
//
// The issue offered `principals.kind = 'operator'` or a flag, and this is the
// flag. `kind` answers a different question — *what* holds this credential, a
// human or an agent — and RFC-0003 Decision 5 turns on that answer: an agent
// acts under a delegating human, and `handleCreatePrincipal` validates the
// two values because they mean those two things. Spending that field on
// authority collapses two axes into one, so promoting a person would erase
// the fact that they are a person, and an operator that is a CI agent could
// not be described at all. `operator_since` is also the schema's own idiom
// for a state with a time on it — `disabled_at`, `revoked_at`, `granted_at`
// — and it demotes by going back to NULL, which a repurposed `kind` cannot do
// without remembering what it used to say.
//
// # Where an operator comes from
//
// The first one comes from the bootstrap token, and only while there is no
// operator at all: `POST /v1/principals` promotes the principal it mints when
// the service has none. After that ARK_BOOTSTRAP_TOKEN can still mint — that
// is Decision 6 and it is unchanged — but it cannot promote, so a leaked
// bootstrap secret cannot make itself an authority over the service. Every
// operator after the first is made by an operator, presenting their own
// credential.
//
// The legacy service token is deliberately **not** an operator, even though
// it carries implicit admin on every repository. The point of this file is
// that a service-wide act is attributed to somebody; letting the string the
// whole fleet shares perform one would leave the model built and unused.
// elk-work/ark#54 asks that the legacy break-glass not be removed without a
// replacement, and this is the replacement: the break-glass for a service
// with no operator is ARK_BOOTSTRAP_TOKEN on the route it was already
// confined to.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/elk-work/ark/pkg/api"
)

// errNoSuchCredential is "no credentials row with that id". It is separated
// from a permission refusal so the handler can decide which of the two a
// given caller is allowed to be told apart — see handleRevokeCredential.
var errNoSuchCredential = errors.New("no such credential")

// requireOperator refuses a caller that is not an operator, and writes the
// refusal itself so an operator route is one line at the top of a handler —
// the shape s.allow already has for per-repository levels.
//
// The refusal names what to do next, because the reader cannot fix it: an
// operator promotes them, and on a service that has no operator yet the
// bootstrap token mints the first one.
func (s *Server) requireOperator(w http.ResponseWriter, r *http.Request, act string) (*authenticated, bool) {
	who := principalOf(r)
	if who == nil {
		writeErr(w, http.StatusForbidden, "permission", "this request carries no principal")
		return nil, false
	}
	if who.Operator {
		return who, true
	}
	// The legacy token reaches every repository and no service-wide act. Said
	// plainly, because "admin everywhere" makes the refusal look like a bug.
	if who.Legacy {
		writeErr(w, http.StatusForbidden, "permission",
			"the shared service token is not an operator: "+act+" is attributed to a named principal, "+
				"so it cannot be done with a credential the whole fleet holds. "+
				"Mint an operator with `ark principal create` and the service's ARK_BOOTSTRAP_TOKEN")
		return nil, false
	}
	writeErr(w, http.StatusForbidden, "permission",
		principalLabel(who)+" is not an operator, and "+act+" is an operator act. "+
			"An operator adds another with `ark principal create --operator <email>`; "+
			"on a service with no operator at all, the first is minted with ARK_BOOTSTRAP_TOKEN")
	return nil, false
}

// hasOperator reports whether the service has any operator. It is what makes
// the bootstrap promotion a one-time act rather than a standing entitlement.
func (a *authStore) hasOperator(ctx context.Context) (bool, error) {
	snap, err := a.snapshot(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range snap.principals {
		if p.Operator() {
			return true, nil
		}
	}
	return false, nil
}

// listPrincipals returns every principal the service knows and the
// credentials each holds, in email order — the order a person reads.
//
// It reads the cached snapshot, which already holds both tables whole: the
// roster costs no fetch beyond the one the caller's own bearer paid for.
func (a *authStore) listPrincipals(ctx context.Context) ([]api.PrincipalRecord, error) {
	snap, err := a.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	byPrincipal := map[string][]api.Credential{}
	for _, c := range snap.credentials {
		byPrincipal[c.PrincipalID] = append(byPrincipal[c.PrincipalID], apiCredential(c))
	}
	out := make([]api.PrincipalRecord, 0, len(snap.principals))
	for _, p := range snap.principals {
		creds := byPrincipal[p.ID]
		sortCredentials(creds)
		out = append(out, api.PrincipalRecord{Principal: apiPrincipal(p), Credentials: creds})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := normalizeEmail(out[i].Principal.Email), normalizeEmail(out[j].Principal.Email)
		if a != b {
			return a < b
		}
		return out[i].Principal.ID < out[j].Principal.ID
	})
	return out, nil
}

// credentialsOf returns one principal's credentials. This is the self-service
// half, and it is the half that makes revocation usable without an operator:
// you cannot revoke by an id you never saw.
func (a *authStore) credentialsOf(ctx context.Context, principalID string) ([]api.Credential, error) {
	snap, err := a.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := []api.Credential{}
	for _, c := range snap.credentials {
		if c.PrincipalID == principalID {
			out = append(out, apiCredential(c))
		}
	}
	sortCredentials(out)
	return out, nil
}

// findCredential resolves a credential id to its row, from the cache.
func (a *authStore) findCredential(ctx context.Context, id string) (api.Credential, error) {
	snap, err := a.snapshot(ctx)
	if err != nil {
		return api.Credential{}, err
	}
	for _, c := range snap.credentials {
		if c.ID == id {
			return apiCredential(c), nil
		}
	}
	return api.Credential{}, errNoSuchCredential
}

// revokeCredential sets revoked_at, and records who did it.
//
// Revoking an already-revoked credential is a success that changes nothing:
// the state the caller asked for holds, the same way revoking a grant nobody
// holds is a success (spec §20). `revoked_at IS NULL` in the WHERE clause is
// what makes that true and is also what makes the closure safe to replay
// after a lost compare-and-swap — a replay must not move the timestamp or
// rewrite whose act it was.
func (a *authStore) revokeCredential(ctx context.Context, id, by string) (api.Credential, bool, error) {
	before, err := a.findCredential(ctx, id)
	if err != nil {
		return api.Credential{}, false, err
	}
	if before.RevokedAt != "" {
		return before, true, nil
	}
	at := a.now().UTC().Format(time.RFC3339Nano)
	var out api.Credential
	var already bool
	err = a.update(ctx, func(tx *sql.Tx) error {
		out, already = api.Credential{}, false
		res, err := tx.ExecContext(ctx, `UPDATE credentials SET revoked_at = ?, revoked_by = ?
			WHERE id = ? AND revoked_at IS NULL`, at, by, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		already = n == 0
		var expires, lastUsed, revokedAt, revokedBy string
		err = tx.QueryRowContext(ctx, `SELECT id, principal_id, label, created_at,
			COALESCE(expires_at, ''), COALESCE(last_used_on, ''), COALESCE(revoked_at, ''),
			COALESCE(revoked_by, '') FROM credentials WHERE id = ?`, id).
			Scan(&out.ID, &out.PrincipalID, &out.Label, &out.CreatedAt,
				&expires, &lastUsed, &revokedAt, &revokedBy)
		if errors.Is(err, sql.ErrNoRows) {
			// Gone between the cached read and this transaction. Nothing to
			// revoke is not an error the caller can act on differently.
			return errNoSuchCredential
		}
		if err != nil {
			return err
		}
		out.ExpiresAt, out.LastUsedOn = expires, lastUsed
		out.RevokedAt, out.RevokedBy = revokedAt, revokedBy
		return nil
	})
	if err != nil {
		return api.Credential{}, false, err
	}
	return out, already, nil
}

// apiPrincipal is the wire shape of a principals row. `issuer` and `subject`
// stay behind: they are the identity provider's business and naming them
// would say who signed a person in, on a roster whose purpose is who may act.
func apiPrincipal(p authPrincipal) api.Principal {
	return api.Principal{
		ID:            p.ID,
		Kind:          p.Kind,
		Email:         p.Email,
		DisplayName:   p.DisplayName,
		CreatedAt:     p.CreatedAt,
		OperatorSince: p.OperatorSince,
		DisabledAt:    p.DisabledAt,
	}
}

// apiCredential is the wire shape of a credentials row. The SHA-256 is not on
// it and never will be: it is the only representation of the secret the
// service keeps, and a roster is not a place to publish digests of secrets.
func apiCredential(c authCredential) api.Credential {
	return api.Credential{
		ID:          c.ID,
		PrincipalID: c.PrincipalID,
		Label:       c.Label,
		CreatedAt:   c.CreatedAt,
		ExpiresAt:   c.ExpiresAt,
		LastUsedOn:  c.LastUsedOn,
		RevokedAt:   c.RevokedAt,
		RevokedBy:   c.RevokedBy,
	}
}

// sortCredentials puts the newest first — the one somebody is most likely
// holding, and the one a person scanning for "which of these is my laptop"
// reads first.
func sortCredentials(creds []api.Credential) {
	sort.Slice(creds, func(i, j int) bool {
		if creds[i].CreatedAt != creds[j].CreatedAt {
			return creds[i].CreatedAt > creds[j].CreatedAt
		}
		return creds[i].ID > creds[j].ID
	})
}

// handleListPrincipals answers GET /v1/principals: every principal the
// service knows, and the credentials each of them holds.
//
// Operator-only, for the reason handleListGrants is admin-only: the roster is
// a list of email addresses and of what each person could present, and the
// people who may read one are the people who may change it.
func (s *Server) handleListPrincipals(w http.ResponseWriter, r *http.Request) {
	who, ok := s.requireOperator(w, r, "listing principals")
	if !ok {
		return
	}
	principals, err := s.authStore().listPrincipals(r.Context())
	if err != nil {
		s.internal(w, "list principals", err)
		return
	}
	if s.Log != nil {
		s.Log.Info("principals listed", "operator", who.ID, "operator_email", who.Email,
			"credential", who.CredentialID, "count", len(principals))
	}
	writeJSON(w, api.PrincipalListResponse{Principals: principals})
}

// handleListCredentials answers GET /v1/credentials: the caller's own
// credentials, and nobody else's.
//
// Any authenticated principal, because there is nothing here they do not
// already have — except the ids, which is the point. Revocation names a
// credential by id, and the machine that held one is precisely the machine
// that is lost.
func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	if who == nil {
		writeErr(w, http.StatusForbidden, "permission", "this request carries no principal")
		return
	}
	if who.Legacy {
		// The service token has no principals row, so it holds no credentials
		// to list. Answering with an empty list would read as "you have none".
		writeErr(w, http.StatusForbidden, "permission",
			"the shared service token is not a principal and holds no credentials of its own. "+
				"`ark principal create` exchanges the service's ARK_BOOTSTRAP_TOKEN for one")
		return
	}
	creds, err := s.authStore().credentialsOf(r.Context(), who.ID)
	if err != nil {
		s.internal(w, "list credentials", err)
		return
	}
	writeJSON(w, api.CredentialListResponse{PrincipalID: who.ID, Credentials: creds})
}

// handleRevokeCredential answers POST /v1/credentials/{id}/revoke.
//
// An operator may revoke any credential; anybody may revoke one of their own.
// The second half is there because it is the case that cannot wait: a laptop
// goes missing on a Saturday and its owner should not have to find an
// operator before the credential on it stops working.
//
// Revocation lands within `authTTL` — RFC-0003 accepts eventual consistency
// bounded at a minute in exchange for not reading auth.db on every request,
// and the write drops this instance's cache so it is immediate here.
func (s *Server) handleRevokeCredential(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	if who == nil {
		writeErr(w, http.StatusForbidden, "permission", "this request carries no principal")
		return
	}
	id := r.PathValue("id")

	cred, err := s.authStore().findCredential(r.Context(), id)
	if err != nil && !errors.Is(err, errNoSuchCredential) {
		s.internal(w, "find credential", err)
		return
	}
	// A caller who is not an operator learns the same thing about a
	// credential that is not theirs and about one that does not exist:
	// otherwise this route would answer "does this id exist" for anybody
	// holding any credential at all.
	mine := err == nil && cred.PrincipalID == who.ID && !who.Legacy
	if !mine && !who.Operator {
		if who.Legacy {
			writeErr(w, http.StatusForbidden, "permission",
				"the shared service token is not an operator: revoking a credential is attributed to a "+
					"named principal, so it cannot be done with a credential the whole fleet holds. "+
					"Mint an operator with `ark principal create` and the service's ARK_BOOTSTRAP_TOKEN")
			return
		}
		writeErr(w, http.StatusNotFound, "not_found",
			"no credential of yours has id "+id+". `ark credential list` shows the ones you hold; "+
				"retiring somebody else's is an operator act")
		return
	}
	if errors.Is(err, errNoSuchCredential) {
		writeErr(w, http.StatusNotFound, "not_found", "no credential has id "+id)
		return
	}

	revoked, already, err := s.authStore().revokeCredential(r.Context(), id, who.ID)
	if errors.Is(err, errNoSuchCredential) {
		writeErr(w, http.StatusNotFound, "not_found", "no credential has id "+id)
		return
	}
	if err != nil {
		s.internal(w, "revoke credential", err)
		return
	}
	if s.Log != nil && !already {
		// The act, the credential, and the named principal behind it — never
		// the credential itself (spec §21). This line is the audit record:
		// RFC-0003's non-goals rule out an audit-log export, so attribution
		// lives in the log and in `credentials.revoked_by` beside it.
		s.Log.Info("credential revoked", "credential", revoked.ID,
			"principal", revoked.PrincipalID, "by", who.ID, "by_email", who.Email,
			"self", revoked.PrincipalID == who.ID, "operator", who.Operator)
	}
	writeJSON(w, api.RevokeCredentialResponse{Credential: revoked, AlreadyRevoked: already})
}
