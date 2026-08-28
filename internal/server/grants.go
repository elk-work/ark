package server

// Per-repository authorization: what a principal may do in one repository
// (docs/rfc-0003-elk-issued-credentials.md, Decision 4; elk-work/ark#52).
//
// `grants` has existed since #43 and was read by nothing, so a valid
// credential reached every route the service token does. This file is the
// half that reads it. Three levels, on a repository, and nothing else: `read`
// pulls, `write` pushes, `admin` also grants and corrects metadata. No
// groups, no roles, no matrix — Principle 005 and RFC-0003's non-goals.
//
// **There is no wire change.** Enforcement rides on the bearer a client
// already sends and on the repository id every route already names, and a
// refusal is the `permission` code the error model has always had (spec §22,
// exit 5). No client needs a new version to be governed by a grant, which is
// what makes this deployable against a live fleet.
//
// Two things are deliberately absent and are named where they belong:
// revoking a *credential* and listing every principal are service-wide acts,
// and the model here is per-repository — see `docs/rfc-0003` and #54.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/pkg/api"
)

// The default-grant policy, ARK_DEFAULT_GRANT (RFC-0003, resolved decision 2).
//
// `seeded` is the default and, on a service with no identity provider, is
// indistinguishable from `none`: seeding is something an approver asserts at
// login (#53), so with nobody asserting anything, nothing is seeded. A
// self-hosted deployment therefore gets deny-by-default for free, without
// having to know the setting exists.
const (
	DefaultGrantNone   = "none"
	DefaultGrantRead   = "read"
	DefaultGrantSeeded = "seeded"
)

// DefaultGrantValues is what ARK_DEFAULT_GRANT accepts, for the caller that
// validates it at startup rather than discovering a typo as an outage.
var DefaultGrantValues = []string{DefaultGrantNone, DefaultGrantRead, DefaultGrantSeeded}

// grantRanks orders the levels. A level is "at least" another when its rank
// is not lower; the zero value — no grant at all — is below every level, and
// is what a principal has on a repository nobody has given it.
var grantRanks = map[string]int{
	api.GrantRead:  1,
	api.GrantWrite: 2,
	api.GrantAdmin: 3,
}

// validGrantLevel reports whether a level is one of the three.
func validGrantLevel(level string) bool {
	_, ok := grantRanks[level]
	return ok
}

// atLeast reports whether `have` carries the authority of `want`.
func atLeast(have, want string) bool {
	return grantRanks[have] >= grantRanks[want]
}

// authGrant is one grants (or pending_grants) row.
type authGrant struct {
	RepositoryID string
	PrincipalID  string
	Email        string
	Level        string
	GrantedBy    string
	GrantedAt    string
}

// grantKey and pendingKey index the snapshot's two grant maps. The separator
// cannot appear in a ULID or an email address, so no pair of values can be
// made to collide by choosing them carefully.
func grantKey(repoID, principalID string) string { return repoID + "\x00" + principalID }
func pendingKey(repoID, email string) string     { return repoID + "\x00" + normalizeEmail(email) }

// normalizeEmail is how an email is compared, everywhere.
//
// A grant is keyed on an address a human typed and resolved against one an
// identity provider asserted, and the two will differ in case sooner or
// later. Comparing case-insensitively is the whole of the normalization —
// nothing rewrites what is stored, so `ark repo grants` still shows the
// operator the address they issued.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// loadGrants reads both grant tables into a snapshot. Called from
// authStore.load, inside the same fetch: authorizing a request must not cost
// an object read of its own.
func loadGrants(ctx context.Context, db *sql.DB, snap *authSnapshot) error {
	rows, err := db.QueryContext(ctx, `SELECT g.repository_id, g.principal_id,
		COALESCE(p.email, ''), g.level, g.granted_by, g.granted_at
		FROM grants g LEFT JOIN principals p ON p.id = g.principal_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var g authGrant
		if err := rows.Scan(&g.RepositoryID, &g.PrincipalID, &g.Email,
			&g.Level, &g.GrantedBy, &g.GrantedAt); err != nil {
			rows.Close()
			return err
		}
		snap.grants[grantKey(g.RepositoryID, g.PrincipalID)] = g
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT repository_id, email, level, granted_by, granted_at
		FROM pending_grants`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g authGrant
		if err := rows.Scan(&g.RepositoryID, &g.Email, &g.Level, &g.GrantedBy, &g.GrantedAt); err != nil {
			return err
		}
		snap.pending[pendingKey(g.RepositoryID, g.Email)] = g
	}
	return rows.Err()
}

// levelFor returns the level a principal holds on a repository, or "" for
// none. It reads the cached snapshot, so it costs nothing beyond the read the
// bearer already paid for.
func (a *authStore) levelFor(ctx context.Context, repoID, principalID string) (string, error) {
	snap, err := a.snapshot(ctx)
	if err != nil {
		return "", err
	}
	return snap.grants[grantKey(repoID, principalID)].Level, nil
}

// setGrant writes one grant, resolving the email to a principal if the
// service already knows it and recording it as pending if not.
//
// Both halves are one statement each and both are last-write-wins on
// (repository, grantee): re-granting at a different level is a correction,
// not a second grant, and the closure is safe to replay after a lost CAS.
func (a *authStore) setGrant(ctx context.Context, repoID, email, level, grantedBy string) (api.Grant, error) {
	var out api.Grant
	at := a.now().UTC().Format(time.RFC3339Nano)
	err := a.update(ctx, func(tx *sql.Tx) error {
		out = api.Grant{Email: strings.TrimSpace(email), Level: level,
			GrantedBy: grantedBy, GrantedAt: at}

		principalID, err := principalByEmail(ctx, tx, email)
		if err != nil {
			return err
		}
		if principalID == "" {
			// Nobody holds this address yet. The grant waits for them,
			// which is the point of keying on an email: the invitation is
			// issued before the invitee has ever authenticated, and no
			// credential is passed person-to-person.
			out.Pending = true
			_, err := tx.ExecContext(ctx, `INSERT INTO pending_grants
				(repository_id, email, level, granted_by, granted_at) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (repository_id, email) DO UPDATE SET
					level = excluded.level, granted_by = excluded.granted_by,
					granted_at = excluded.granted_at`,
				repoID, out.Email, level, grantedBy, at)
			return err
		}
		out.PrincipalID = principalID
		if _, err := tx.ExecContext(ctx, `INSERT INTO grants
			(repository_id, principal_id, level, granted_by, granted_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (repository_id, principal_id) DO UPDATE SET
				level = excluded.level, granted_by = excluded.granted_by,
				granted_at = excluded.granted_at`,
			repoID, principalID, level, grantedBy, at); err != nil {
			return err
		}
		// A pending row for the same address would otherwise be claimed at
		// the grantee's next login and overwrite this one with a stale level.
		_, err = tx.ExecContext(ctx, `DELETE FROM pending_grants
			WHERE repository_id = ? AND lower(email) = ?`, repoID, normalizeEmail(email))
		return err
	})
	if err != nil {
		return api.Grant{}, err
	}
	return out, nil
}

// revokeGrant removes whatever an email holds on a repository, resolved or
// pending. It reports whether anything was there: revoking a grant nobody
// holds is a correct answer and not an error, the way `ark logout` is
// idempotent on a machine that never logged in (spec §20).
func (a *authStore) revokeGrant(ctx context.Context, repoID, email string) (bool, error) {
	removed := false
	err := a.update(ctx, func(tx *sql.Tx) error {
		removed = false
		res, err := tx.ExecContext(ctx, `DELETE FROM pending_grants
			WHERE repository_id = ? AND lower(email) = ?`, repoID, normalizeEmail(email))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		removed = n > 0

		principalID, err := principalByEmail(ctx, tx, email)
		if err != nil || principalID == "" {
			return err
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM grants
			WHERE repository_id = ? AND principal_id = ?`, repoID, principalID)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		if err != nil {
			return err
		}
		removed = removed || n > 0
		return nil
	})
	return removed, err
}

// listGrants returns every grant on a repository, resolved and pending
// alike, in email order — the order a person reads.
func (a *authStore) listGrants(ctx context.Context, repoID string) ([]api.Grant, error) {
	snap, err := a.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := []api.Grant{}
	for _, g := range snap.grants {
		if g.RepositoryID != repoID {
			continue
		}
		out = append(out, api.Grant{Email: g.Email, Level: g.Level, PrincipalID: g.PrincipalID,
			GrantedBy: g.GrantedBy, GrantedAt: g.GrantedAt})
	}
	for _, g := range snap.pending {
		if g.RepositoryID != repoID {
			continue
		}
		out = append(out, api.Grant{Email: g.Email, Level: g.Level,
			GrantedBy: g.GrantedBy, GrantedAt: g.GrantedAt, Pending: true})
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := normalizeEmail(out[i].Email), normalizeEmail(out[j].Email); a != b {
			return a < b
		}
		return out[i].PrincipalID < out[j].PrincipalID
	})
	return out, nil
}

// grantOnCreate gives the principal that created a repository `admin` on it —
// the bootstrap rule, first-writer-registers (RFC-0003 Decision 4). Everyone
// else has no access until granted.
//
// It runs after the repository database is committed, because the two live in
// different objects under different compare-and-swaps and cannot share a
// transaction. The failure it leaves behind is a repository nobody
// administers, which is why it is loud: the recovery is a grant issued by the
// service token, which carries implicit admin everywhere until #54 retires it.
func (a *authStore) grantOnCreate(ctx context.Context, repoID, principalID string) error {
	at := a.now().UTC().Format(time.RFC3339Nano)
	return a.update(ctx, func(tx *sql.Tx) error {
		return addGrant(ctx, tx, repoID, principalID, api.GrantAdmin, principalID, at)
	})
}

// addGrant writes a grant **only where none exists**, and is the one place a
// grant is written without an admin having asked for that exact level.
//
// `ON CONFLICT DO NOTHING` is not a detail here, it is the whole rule, and
// two callers depend on it for two different reasons:
//
//   - **Seeding** (device.go). RFC-0003's resolved decision 2: an approval may
//     add `read` on the repositories the identity provider says a person may
//     see, and seeding runs on *every* login. Leaving an existing grant
//     exactly as it stands is what makes that safe — it can neither downgrade
//     somebody who was given `write` nor resurrect a level an admin took away,
//     so losing workspace membership never silently revokes Ark access and
//     revocation stays an explicit act.
//   - **First-writer-registers** (grantOnCreate). A bootstrap, not a
//     correction: the closure it runs in reruns after a lost compare-and-swap,
//     and a replay must not undo a grant somebody has since changed.
//
// An admin issuing a grant does *not* come through here — `setGrant` upserts,
// because that is a deliberate statement about a level and must win.
func addGrant(ctx context.Context, tx *sql.Tx, repoID, principalID, level, grantedBy, at string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO grants
		(repository_id, principal_id, level, granted_by, granted_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (repository_id, principal_id) DO NOTHING`,
		repoID, principalID, level, grantedBy, at)
	return err
}

// principalByEmail resolves an address to a principal id, case-insensitively,
// or returns "" when nobody holds it. Disabled principals resolve like any
// other: a grant on a disabled principal is still that principal's grant, and
// the disabling is what refuses the request.
//
// `ORDER BY id` because the unique index on `principals.email` is
// case-*sensitive*, so `Alice@…` and `alice@…` can both exist as separate
// principals while this lookup matches both. One of them has to win, and an
// authorization lookup must not pick arbitrarily — the lowest ULID is the
// first registration, the same tie-break `resolveWriter` and
// `store.FindAgentActor` use.
func principalByEmail(ctx context.Context, tx *sql.Tx, email string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM principals WHERE lower(email) = ?
		ORDER BY id LIMIT 1`, normalizeEmail(email)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// claimPendingGrants turns every grant issued to an address into a grant held
// by the principal that now holds it.
//
// RFC-0003 has this happen at login, and this is the login the service has
// today: `ark principal create` is how a principal comes into existence
// without an identity provider. The device flow (#53) resolves the same way
// by calling this from its own approval path.
//
// It only ever *adds*: an existing grant at a different level wins, because
// it was issued deliberately about a principal that already existed, and a
// pending row is an older statement about somebody who did not.
func claimPendingGrants(ctx context.Context, tx *sql.Tx, principalID, email string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO grants
		(repository_id, principal_id, level, granted_by, granted_at)
		SELECT repository_id, ?, level, granted_by, granted_at FROM pending_grants
		WHERE lower(email) = ?
		ON CONFLICT (repository_id, principal_id) DO NOTHING`,
		principalID, normalizeEmail(email)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM pending_grants WHERE lower(email) = ?`,
		normalizeEmail(email))
	return err
}

// faultPermission is a refusal of an authenticated caller: we know who you
// are and you may not do this. It is `403` rather than `401`, which is the
// HTTP distinction between "authenticate" and "authorization will not help" —
// and both map to the same `permission` code and exit 5 (spec §22), so no
// client had to learn anything for this to land.
func faultPermission(msg string) *writeFault {
	return &writeFault{http.StatusForbidden, "permission", msg}
}

// levelOf is the level a request's principal holds on a repository, after the
// two rules that do not come from a grants row.
func (s *Server) levelOf(ctx context.Context, who *authenticated, repoID string) (string, error) {
	// The legacy service token carries implicit admin on every repository,
	// exactly as it does today. It is the whole fleet's credential and it
	// identifies nobody, so there is no grant it could be checked against;
	// #54 removes this branch along with the token itself.
	if who.Legacy {
		return api.GrantAdmin, nil
	}
	level, err := s.authStore().levelFor(ctx, repoID, who.ID)
	if err != nil {
		return "", err
	}
	// ARK_DEFAULT_GRANT=read makes every authenticated principal a reader of
	// every repository — closer to what the service token already provides,
	// and a deliberate setting rather than a discovered one.
	if s.DefaultGrant == DefaultGrantRead && !atLeast(level, api.GrantRead) {
		return api.GrantRead, nil
	}
	return level, nil
}

// allow is the check every route makes about the repository it is about to
// touch. It writes the refusal itself and reports whether the handler may
// carry on, so enforcing a level is one line at the top of a handler and is
// greppable across the package.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, repoID, want string) bool {
	fault := s.authorize(r, repoID, want)
	if fault == nil {
		return true
	}
	if fault.code == "internal" {
		// A store nobody can read is not a refusal, and saying so would send
		// the holder to ask for a grant they already have.
		s.internal(w, "authorize", errors.New(fault.msg))
		return false
	}
	writeErr(w, fault.status, fault.code, fault.msg)
	return false
}

// authorize is allow's decision half, returning the fault rather than writing
// it — for the handlers that make the decision inside a transaction.
func (s *Server) authorize(r *http.Request, repoID, want string) *writeFault {
	who, ok := principalFrom(r.Context())
	if !ok {
		// Every /v1 route is wrapped in s.auth, so this is unreachable
		// except by a route registered without it. Refusing is the half of
		// that mistake that does not hand out access.
		return faultPermission("this request carries no principal")
	}
	level, err := s.levelOf(r.Context(), who, repoID)
	if err != nil {
		return &writeFault{http.StatusInternalServerError, "internal",
			fmt.Sprintf("read grants for %s: %v", repoID, err)}
	}
	if atLeast(level, want) {
		return nil
	}
	// A repository this service does not hold is not an authorization
	// question, and answering it as one would be wrong twice over. `ark
	// login` verifies a credential by pulling a repository id that cannot
	// exist and reading the 404 as proof it authenticated — a brand-new
	// principal holds no grants at all, so refusing there would make every
	// first login report the credential as rejected. And a repository the
	// service has *lost* answers `not_found` on exactly these routes, which
	// is the only clean signal a client has that its history is gone
	// (§19, elk-work/ark#66); turning that into a permission error for
	// whoever had not been granted it yet would hide the loss behind a
	// plausible-looking refusal.
	//
	// Only a definite absence takes this path. Any other failure to look
	// refuses, because a probe that could not tell must not be the reason a
	// caller gets in.
	if err := s.Repos.View(r.Context(), repoID, func(*sql.DB) error { return nil }); errors.Is(err, repodb.ErrNotFound) {
		return nil
	}
	return faultPermission(refusal(who, repoID, level, want))
}

// refusal words a denial so the reader knows which of the two things went
// wrong — you hold nothing here, or you hold something and it is not enough —
// and what to do about either. Both name the command that fixes it, because
// the person reading this cannot fix it themselves.
func refusal(who *authenticated, repoID, have, want string) string {
	if have == "" {
		return fmt.Sprintf("%s has no grant on repository %s: %s access is required. "+
			"An admin of that repository issues one with `ark repo grant %s --%s`",
			principalLabel(who), repoID, want, principalLabel(who), want)
	}
	return fmt.Sprintf("%s holds %s on repository %s, and %s access is required. "+
		"An admin of that repository raises it with `ark repo grant %s --%s`",
		principalLabel(who), have, repoID, want, principalLabel(who), want)
}

// principalLabel names a principal the way its holder would recognise it.
func principalLabel(who *authenticated) string {
	if who.Email != "" {
		return who.Email
	}
	return "principal " + who.ID
}

// handleSetGrant issues or revokes one grant. `admin` on the repository, and
// nothing less: `write` pushes and cannot grant.
func (s *Server) handleSetGrant(w http.ResponseWriter, r *http.Request) {
	repoID := r.PathValue("repo")
	if !s.allow(w, r, repoID, api.GrantAdmin) {
		return
	}
	req, ok := decode[api.SetGrantRequest](w, r)
	if !ok {
		return
	}
	email := strings.TrimSpace(req.Email)
	if email == "" {
		writeErr(w, http.StatusBadRequest, "validation",
			"email is required: a grant is issued to an address, so that it can be issued before its grantee has ever logged in")
		return
	}
	if !req.Revoke && !validGrantLevel(req.Level) {
		writeErr(w, http.StatusBadRequest, "validation",
			"level must be read, write, or admin")
		return
	}
	// The repository has to exist before it can be administered. Without
	// this, a typo in a ULID would be answered with a grant on nothing and
	// look exactly like success.
	if err := s.Repos.View(r.Context(), repoID, func(*sql.DB) error { return nil }); err != nil {
		s.respond(w, "load repository", err)
		return
	}

	who, _ := principalFrom(r.Context())
	if req.Revoke {
		removed, err := s.authStore().revokeGrant(r.Context(), repoID, email)
		if err != nil {
			s.internal(w, "revoke grant", err)
			return
		}
		if s.Log != nil {
			s.Log.Info("grant revoked", "repository_id", repoID, "email", email,
				"by", who.ID, "held_one", removed)
		}
		writeJSON(w, api.GrantResponse{Grant: api.Grant{Email: email}, Revoked: true})
		return
	}

	grant, err := s.authStore().setGrant(r.Context(), repoID, email, req.Level, who.ID)
	if err != nil {
		s.internal(w, "set grant", err)
		return
	}
	if s.Log != nil {
		s.Log.Info("grant issued", "repository_id", repoID, "email", email,
			"level", req.Level, "by", who.ID, "pending", grant.Pending)
	}
	writeJSON(w, api.GrantResponse{Grant: grant})
}

// handleListGrants shows who holds what on one repository.
//
// It is `admin`, not `read`. The list is a roster of email addresses, and the
// people who can see one are the people who can change it — which is also why
// there is no route that lists every principal the service knows (#54).
func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	repoID := r.PathValue("repo")
	if !s.allow(w, r, repoID, api.GrantAdmin) {
		return
	}
	if err := s.Repos.View(r.Context(), repoID, func(*sql.DB) error { return nil }); err != nil {
		s.respond(w, "load repository", err)
		return
	}
	grants, err := s.authStore().listGrants(r.Context(), repoID)
	if err != nil {
		s.internal(w, "list grants", err)
		return
	}
	writeJSON(w, api.GrantListResponse{RepositoryID: repoID, Grants: grants})
}
