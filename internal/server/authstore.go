package server

// auth.db — the credential store behind per-principal authentication
// (docs/rfc-0003-elk-issued-credentials.md, Decision 4 and slices 1-2).
//
// One SQLite database, held in the same repodb.Backend as the repository
// databases and written with the same object-generation compare-and-swap, so
// there is no new storage technology, no new dependency, and it works
// identically in DATA_DIR mode. It is read-mostly and cached in memory with a
// hard TTL, because otherwise every /v1 request would fetch it.
//
// It does not go through repodb.Manager, for two reasons that are the same
// reason twice. The Manager applies the repository schema to everything it
// opens, and auth.db is not a repository — filling it with empty `records` and
// `meta` tables would make the recovery path RFC-0001 established ("it is a
// file in a bucket; download it, open it, fix it") harder to follow rather
// than easier. And the reserved key deliberately contains a dot, which
// repodb's own validRepoID rejects, so no client can ever register a
// repository that collides with it. The CAS loop below is therefore a small
// copy of the Manager's, and it holds the same contract: the closure reruns on
// a lost race, so it must be idempotent.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/pkg/api"
)

// authDBKey is the backend key auth.db lives under: object `repos/ark.auth.db`
// in GCS, file `<dir>/ark.auth.db` locally. The dot is load-bearing — every
// client-driven repository operation goes through repodb.validRepoID, which
// refuses an id containing one, so the reservation is enforced by code rather
// than by this comment.
const authDBKey = "ark.auth"

// credentialPrefix marks an Ark-issued credential. It is what lets the
// verifier tell "a credential we should look up" from "the service token or
// something else entirely" without a datastore round trip per bad request.
const credentialPrefix = "arkc_"

// authTTL bounds how stale a cached auth.db may be. RFC-0003 accepts
// eventually-consistent revocation in exchange for not reading the store on
// every request; a minute is the bound, and at --max-instances 1 it is
// effectively immediate.
const authTTL = 60 * time.Second

// credentialLifetime is how long a minted credential lasts. RFC-0003 Decision
// 2: a laptop closed for a week is not an event, and recovery from expiry is
// logging in again.
const credentialLifetime = 365 * 24 * time.Hour

// authSchema is auth.db's whole shape.
//
// `pending_grants` is the email half of `grants` (RFC-0003 Decision 4). A
// grant is issued to an address, not to a principal id, so that it can be
// issued before its grantee has ever authenticated — which is what keeps a
// credential from being passed person-to-person. It moves into `grants` the
// first time that address resolves to a principal, and is enforced from that
// moment and not before.
const authSchema = `
CREATE TABLE IF NOT EXISTS principals (
	id             TEXT PRIMARY KEY,
	kind           TEXT NOT NULL,
	issuer         TEXT NOT NULL DEFAULT '',
	subject        TEXT NOT NULL DEFAULT '',
	email          TEXT NOT NULL DEFAULT '',
	display_name   TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	disabled_at    TEXT,
	operator_since TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS principals_email ON principals (email) WHERE email <> '';

CREATE TABLE IF NOT EXISTS credentials (
	id           TEXT PRIMARY KEY,
	principal_id TEXT NOT NULL REFERENCES principals (id),
	token_sha256 TEXT NOT NULL UNIQUE,
	label        TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	expires_at   TEXT,
	last_used_on TEXT,
	revoked_at   TEXT,
	revoked_by   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS credentials_principal ON credentials (principal_id);

CREATE TABLE IF NOT EXISTS grants (
	repository_id TEXT NOT NULL,
	principal_id  TEXT NOT NULL REFERENCES principals (id),
	level         TEXT NOT NULL,
	granted_by    TEXT NOT NULL DEFAULT '',
	granted_at    TEXT NOT NULL,
	PRIMARY KEY (repository_id, principal_id)
);

CREATE TABLE IF NOT EXISTS pending_grants (
	repository_id TEXT NOT NULL,
	email         TEXT NOT NULL,
	level         TEXT NOT NULL,
	granted_by    TEXT NOT NULL DEFAULT '',
	granted_at    TEXT NOT NULL,
	PRIMARY KEY (repository_id, email)
);
`

// Why a presented credential was refused. These are separated from "we do not
// know you" because the holder already has the credential: naming the reason
// costs no secrecy and saves a support round.
var (
	errNoCredential      = errors.New("credential not recognised")
	errCredentialRevoked = errors.New("credential revoked")
	errCredentialExpired = errors.New("credential expired")
	errPrincipalDisabled = errors.New("principal disabled")
)

// authPrincipal is a principals row.
type authPrincipal struct {
	ID          string
	Kind        string
	Email       string
	DisplayName string
	CreatedAt   string
	DisabledAt  string
	// OperatorSince is set on an operator and empty on everybody else. It is
	// its own column rather than a value of Kind because Kind describes what
	// holds the credential — a human or an agent — and describes it for a
	// reason Decision 5 depends on. See operators.go.
	OperatorSince string
}

// Operator reports whether this principal may perform a service-wide act.
func (p authPrincipal) Operator() bool { return p.OperatorSince != "" }

// authCredential is a credentials row, minus the credential: only its
// SHA-256 is ever stored, so this struct cannot leak one.
type authCredential struct {
	ID          string
	PrincipalID string
	Label       string
	CreatedAt   string
	ExpiresAt   string
	LastUsedOn  string
	RevokedAt   string
	RevokedBy   string
}

// authSnapshot is one read of auth.db, held in memory for up to authTTL.
type authSnapshot struct {
	generation  int64
	principals  map[string]authPrincipal
	credentials map[string]authCredential // keyed by token_sha256
	// grants is every resolved grant, keyed by grantKey. It rides in the
	// same snapshot as the credentials because authorizing a request must
	// not cost a second fetch of an object the request has already read —
	// the whole reason auth.db is cached at all (RFC-0003, "Caching").
	grants map[string]authGrant
	// pending is the email half, keyed by pendingKey. Nothing on the
	// request path reads it: it is consulted when a principal is created
	// and listed by `ark repo grants`.
	pending map[string]authGrant
}

// emptySnapshot is what a deployment that has never bootstrapped looks like.
func emptySnapshot() *authSnapshot {
	return &authSnapshot{
		principals:  map[string]authPrincipal{},
		credentials: map[string]authCredential{},
		grants:      map[string]authGrant{},
		pending:     map[string]authGrant{},
	}
}

// authStore reads and writes auth.db.
type authStore struct {
	backend  repodb.Backend
	cacheDir string
	log      *slog.Logger

	// ttl and now are fields rather than constants so a test can pin the
	// revocation bound instead of waiting a minute for it.
	ttl time.Duration
	now func() time.Time

	// writeMu serialises this process's writers, the way repodb's
	// per-repository mutex does. Cross-process serialisation is the CAS.
	writeMu sync.Mutex

	mu       sync.Mutex
	snap     *authSnapshot
	loadedAt time.Time
	// usage is last_used_on, batched. RFC-0003 makes this best-effort and
	// coarse on purpose: the migration needs to know who has moved without
	// putting a write on every request, and a lost flush only means a
	// credential's clock did not advance.
	usage map[string]string
}

func newAuthStore(backend repodb.Backend, cacheDir string, log *slog.Logger) *authStore {
	return &authStore{
		backend:  backend,
		cacheDir: cacheDir,
		log:      log,
		ttl:      authTTL,
		now:      time.Now,
		usage:    map[string]string{},
	}
}

// hashCredential is the only representation of a credential the service keeps.
func hashCredential(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// mintCredential returns a new credential and the hash to store against it.
// 32 random bytes, base64url, prefixed — RFC-0003 Decision 2.
func mintCredential() (token, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("mint credential: %w", err)
	}
	token = credentialPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, hashCredential(token), nil
}

// openAuthDB opens auth.db at path and applies the schema.
func openAuthDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// deviceSchema is applied beside authSchema rather than folded into it:
	// pending device codes are one table added by one slice (device.go), and
	// keeping the statements apart is what lets the slices land in any order.
	if _, err := db.Exec(authSchema + deviceSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply auth schema: %w", err)
	}
	// `CREATE TABLE IF NOT EXISTS` adds columns to a table that does not
	// exist yet and to no other, so a store created before a column was
	// declared never grows it. Every auth.db in existence predates
	// elk-work/ark#94; addColumns is what brings those forward, and it is the
	// whole of the upgrade — there is no deploy step and no downtime, because
	// it runs on the next read or write like the schema above it.
	if err := addColumns(db, authColumns); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply auth schema: %w", err)
	}
	return db, nil
}

// authColumns is every column added to auth.db after it first shipped, as a
// table name and the column clause that defines it.
//
// New columns must also appear in authSchema, for a store being created
// today, and here, for one that already exists. Both, always: the first
// covers a fresh deployment and the second covers every deployment there is.
var authColumns = [][2]string{
	// elk-work/ark#94: who may act on the service as a whole, and who
	// retired a credential. See operators.go.
	{"principals", "operator_since TEXT"},
	{"credentials", "revoked_by TEXT NOT NULL DEFAULT ''"},
}

// addColumns applies each column, tolerating the one it has already applied.
//
// SQLite has no `ADD COLUMN IF NOT EXISTS`, and the alternative — reading
// `pragma table_info` and diffing — is more code to get wrong for the same
// answer. A duplicate-column error is the success case on the second run, and
// it is matched on its message because modernc.org/sqlite reports it as a
// generic error rather than as a distinguishable code.
func addColumns(db *sql.DB, columns [][2]string) error {
	for _, c := range columns {
		table, clause := c[0], c[1]
		_, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", table, clause))
		if err == nil || strings.Contains(err.Error(), "duplicate column name") {
			continue
		}
		return fmt.Errorf("add %s.%s: %w", table, clause, err)
	}
	return nil
}

// snapshot returns a view of auth.db no older than the TTL.
func (a *authStore) snapshot(ctx context.Context) (*authSnapshot, error) {
	a.mu.Lock()
	if snap := a.freshLocked(); snap != nil {
		a.mu.Unlock()
		return snap, nil
	}
	a.mu.Unlock()

	// The TTL has passed, so this request is already paying for a fetch:
	// spend the write first and let the refetch pick it up. This is the "or a
	// timer" half of RFC-0003's lazy flush, without a goroutine to leak.
	if err := a.flushUsage(ctx); err != nil && a.log != nil {
		a.log.Warn("flush credential usage", "error", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Another request may have refreshed while this one was unlocked.
	if snap := a.freshLocked(); snap != nil {
		return snap, nil
	}
	snap, err := a.load(ctx)
	if err != nil {
		return nil, err
	}
	a.snap, a.loadedAt = snap, a.now()
	return snap, nil
}

// freshLocked returns the cached snapshot if it is still inside the TTL.
// Caller holds a.mu.
func (a *authStore) freshLocked() *authSnapshot {
	if a.snap != nil && a.now().Sub(a.loadedAt) < a.ttl {
		return a.snap
	}
	return nil
}

// load fetches auth.db and reads it whole. It is small, read-mostly, and read
// once a minute, so reading all of it is cheaper than holding the file open.
//
// A store that does not exist yet is not an error: it is every deployment that
// has not run `ark principal create`, and their legacy bearer must keep
// working without one.
func (a *authStore) load(ctx context.Context) (*authSnapshot, error) {
	if err := os.MkdirAll(a.cacheDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(a.cacheDir, authDBKey+".fetch.db")
	os.Remove(path)
	defer os.Remove(path)

	gen, err := a.backend.Fetch(ctx, authDBKey, path)
	if errors.Is(err, repodb.ErrNotFound) {
		return emptySnapshot(), nil
	}
	if err != nil {
		return nil, err
	}

	db, err := openAuthDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	snap := emptySnapshot()
	snap.generation = gen
	rows, err := db.QueryContext(ctx, `SELECT id, kind, email, display_name, created_at,
		COALESCE(disabled_at, ''), COALESCE(operator_since, '') FROM principals`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p authPrincipal
		if err := rows.Scan(&p.ID, &p.Kind, &p.Email, &p.DisplayName, &p.CreatedAt,
			&p.DisabledAt, &p.OperatorSince); err != nil {
			rows.Close()
			return nil, err
		}
		snap.principals[p.ID] = p
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT id, principal_id, token_sha256, label, created_at,
		COALESCE(expires_at, ''), COALESCE(last_used_on, ''), COALESCE(revoked_at, ''),
		COALESCE(revoked_by, '') FROM credentials`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c authCredential
		var hash string
		if err := rows.Scan(&c.ID, &c.PrincipalID, &hash, &c.Label, &c.CreatedAt,
			&c.ExpiresAt, &c.LastUsedOn, &c.RevokedAt, &c.RevokedBy); err != nil {
			rows.Close()
			return nil, err
		}
		snap.credentials[hash] = c
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if err := loadGrants(ctx, db, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// update runs fn inside one transaction on a fresh copy of auth.db and stores
// it with a compare-and-swap. On a lost race it refetches and reruns fn, which
// must therefore be idempotent — the same contract repodb.Manager.Update
// carries, and for the same reason.
func (a *authStore) update(ctx context.Context, fn func(tx *sql.Tx) error) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	if err := os.MkdirAll(a.cacheDir, 0o755); err != nil {
		return err
	}
	work := filepath.Join(a.cacheDir, authDBKey+".work.db")
	defer os.Remove(work)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		os.Remove(work)
		gen, err := a.backend.Fetch(ctx, authDBKey, work)
		if errors.Is(err, repodb.ErrNotFound) {
			gen = 0
		} else if err != nil {
			return err
		}
		if err := applyAuthTx(ctx, work, fn); err != nil {
			return err
		}
		if _, err := a.backend.Store(ctx, authDBKey, work, gen); err != nil {
			if errors.Is(err, repodb.ErrConcurrentWrite) {
				// Someone else wrote between the fetch and the store. Their
				// copy is now the base; replay against it.
				lastErr = err
				continue
			}
			return err
		}
		// The cached read is now behind this write; drop it rather than wait
		// out the TTL, so a revoke issued through this instance is immediate.
		a.mu.Lock()
		a.snap = nil
		a.mu.Unlock()
		return nil
	}
	return lastErr
}

// applyAuthTx opens (creating if needed) the working copy and runs fn in one
// transaction.
func applyAuthTx(ctx context.Context, path string, fn func(tx *sql.Tx) error) error {
	db, err := openAuthDB(path)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		db.Close()
		return err
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return err
	}
	return db.Close()
}

// verify resolves a presented credential to its principal, or says why not.
//
// The credential is hashed before anything is compared, so the lookup is over
// a digest rather than over the secret: there is no partial-match timing to
// leak, which is what subtle.ConstantTimeCompare buys on the legacy path where
// the secret itself is the comparand.
func (a *authStore) verify(ctx context.Context, presented string) (*authenticated, error) {
	snap, err := a.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	cred, ok := snap.credentials[hashCredential(presented)]
	if !ok {
		return nil, errNoCredential
	}
	if cred.RevokedAt != "" {
		return nil, errCredentialRevoked
	}
	if cred.ExpiresAt != "" {
		exp, err := time.Parse(time.RFC3339, cred.ExpiresAt)
		if err != nil {
			// An expiry nobody can read is not a credential anybody should
			// keep honouring; refusing is the safe half of the ambiguity.
			if a.log != nil {
				a.log.Error("unparseable credential expiry", "credential", cred.ID, "error", err)
			}
			return nil, errCredentialExpired
		}
		if !a.now().Before(exp) {
			return nil, errCredentialExpired
		}
	}
	p, ok := snap.principals[cred.PrincipalID]
	if !ok {
		// A credential whose principal is gone names nobody.
		return nil, errNoCredential
	}
	if p.DisabledAt != "" {
		return nil, errPrincipalDisabled
	}
	a.markUsed(cred)
	return &authenticated{
		ID:           p.ID,
		Kind:         p.Kind,
		Email:        p.Email,
		CredentialID: cred.ID,
		Operator:     p.Operator(),
	}, nil
}

// markUsed queues today against a credential, at day granularity, and only
// when the day has actually moved. Most requests therefore queue nothing.
func (a *authStore) markUsed(cred authCredential) {
	day := a.now().UTC().Format(time.DateOnly)
	if cred.LastUsedOn >= day {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usage[cred.ID] < day {
		a.usage[cred.ID] = day
	}
}

// flushUsage writes queued last_used_on values. Failure is not fatal anywhere:
// the queue is put back and the next flush carries it.
func (a *authStore) flushUsage(ctx context.Context) error {
	a.mu.Lock()
	if len(a.usage) == 0 {
		a.mu.Unlock()
		return nil
	}
	pending := a.usage
	a.usage = map[string]string{}
	a.mu.Unlock()

	err := a.update(ctx, func(tx *sql.Tx) error {
		for id, day := range pending {
			// Never moves the day backwards, which is what makes the closure
			// safe to replay after a lost CAS race.
			if _, err := tx.ExecContext(ctx, `UPDATE credentials SET last_used_on = ?
				WHERE id = ? AND COALESCE(last_used_on, '') < ?`, day, id, day); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		a.mu.Lock()
		for id, day := range pending {
			if a.usage[id] < day {
				a.usage[id] = day
			}
		}
		a.mu.Unlock()
	}
	return err
}

// mintedCredential is what createPrincipal hands back: the plaintext exactly
// once, and enough about the principal to print a useful line.
type mintedCredential struct {
	Principal authPrincipal
	Created   bool
	// Promoted reports that this call made the principal an operator, as
	// against finding one that already was. It is what the log line and the
	// CLI say out loud, because becoming an operator is the one thing here
	// that changes what somebody may do to the whole service.
	Promoted     bool
	Token        string
	CredentialID string
	ExpiresAt    string
}

// createPrincipal mints a principal for an email, or a fresh credential for
// one that already holds it, and returns the credential in the clear once.
//
// Reissuing for a known email is deliberate. Whoever holds the bootstrap token
// could create a principal for any address anyway, so refusing would remove a
// working break-glass — the one recovery path that does not depend on auth.db
// being readable — without withholding anything. A *disabled* principal is the
// exception: re-crediting it would undo the disabling.
//
// `req.Operator` promotes, and `promoteIfFirst` is the bootstrap rule: on a
// service with no operator at all, the principal minted here becomes the
// first one. Both are evaluated inside the transaction, because "is there an
// operator yet" is a question about the copy of auth.db this attempt is
// actually writing — the closure reruns after a lost compare-and-swap, and on
// that rerun somebody else may have become the first operator.
func (a *authStore) createPrincipal(ctx context.Context, req api.CreatePrincipalRequest, promoteIfFirst bool) (*mintedCredential, error) {
	token, hash, err := mintCredential()
	if err != nil {
		return nil, err
	}
	// Minted outside the closure on purpose: the closure reruns on a lost CAS
	// race, and a replay that minted a second credential would hand back a
	// token the store does not hold.
	var (
		newPrincipalID = records.NewID()
		credentialID   = records.NewID()
		createdAt      = a.now().UTC().Format(time.RFC3339Nano)
		expiresAt      = a.now().UTC().Add(credentialLifetime).Format(time.RFC3339Nano)
	)

	var out mintedCredential
	err = a.update(ctx, func(tx *sql.Tx) error {
		out = mintedCredential{} // reset: this closure may be replayed

		p := authPrincipal{
			ID:          newPrincipalID,
			Kind:        req.Kind,
			Email:       req.Email,
			DisplayName: req.DisplayName,
			CreatedAt:   createdAt,
		}
		err := tx.QueryRowContext(ctx, `SELECT id, kind, display_name, created_at,
			COALESCE(disabled_at, ''), COALESCE(operator_since, '') FROM principals WHERE email = ?`, req.Email).
			Scan(&p.ID, &p.Kind, &p.DisplayName, &p.CreatedAt, &p.DisabledAt, &p.OperatorSince)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO principals
				(id, kind, email, display_name, created_at) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (id) DO NOTHING`,
				p.ID, p.Kind, p.Email, p.DisplayName, p.CreatedAt); err != nil {
				return err
			}
			out.Created = true
		case err != nil:
			return err
		case p.DisabledAt != "":
			return errPrincipalDisabled
		}

		// Promotion, and the bootstrap rule that seeds the first operator.
		//
		// `promoteIfFirst` fires only while the service has no operator at
		// all, which is what keeps ARK_BOOTSTRAP_TOKEN from being an operator
		// identity: it can mint principals forever, and it can hand out the
		// authority exactly once, before anybody holds it. After that the
		// only way to make an operator is to already be one.
		if !p.Operator() && (req.Operator || promoteIfFirst) {
			promote := req.Operator
			if !promote {
				var operators int
				if err := tx.QueryRowContext(ctx,
					`SELECT count(*) FROM principals WHERE operator_since IS NOT NULL`).
					Scan(&operators); err != nil {
					return err
				}
				promote = operators == 0
			}
			if promote {
				if _, err := tx.ExecContext(ctx, `UPDATE principals SET operator_since = ?
					WHERE id = ? AND operator_since IS NULL`, createdAt, p.ID); err != nil {
					return err
				}
				p.OperatorSince = createdAt
				out.Promoted = true
			}
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO credentials
			(id, principal_id, token_sha256, label, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
			credentialID, p.ID, hash, "bootstrap", createdAt, expiresAt); err != nil {
			return err
		}
		// A grant issued to this address before anybody held it becomes a
		// grant this principal holds. This is the "resolved at that person's
		// first login" half of RFC-0003 Decision 4, and `ark principal
		// create` is the login a service with no identity provider has; the
		// device flow (#53) resolves the same way through the same call.
		if err := claimPendingGrants(ctx, tx, p.ID, p.Email); err != nil {
			return err
		}
		out.Principal = p
		out.Token = token
		out.CredentialID = credentialID
		out.ExpiresAt = expiresAt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
