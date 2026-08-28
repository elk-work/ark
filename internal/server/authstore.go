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

// authSchema is auth.db's whole shape. `grants` is created here and enforced
// in elk-work/ark#52 — shipping the table with the others avoids a second
// migration for one table.
const authSchema = `
CREATE TABLE IF NOT EXISTS principals (
	id           TEXT PRIMARY KEY,
	kind         TEXT NOT NULL,
	issuer       TEXT NOT NULL DEFAULT '',
	subject      TEXT NOT NULL DEFAULT '',
	email        TEXT NOT NULL DEFAULT '',
	display_name TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	disabled_at  TEXT
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
	revoked_at   TEXT
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
}

// authCredential is a credentials row, minus the credential: only its
// SHA-256 is ever stored, so this struct cannot leak one.
type authCredential struct {
	ID          string
	PrincipalID string
	ExpiresAt   string
	LastUsedOn  string
	RevokedAt   string
}

// authSnapshot is one read of auth.db, held in memory for up to authTTL.
type authSnapshot struct {
	generation  int64
	principals  map[string]authPrincipal
	credentials map[string]authCredential // keyed by token_sha256
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
	if _, err := db.Exec(authSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply auth schema: %w", err)
	}
	return db, nil
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
		return &authSnapshot{
			principals:  map[string]authPrincipal{},
			credentials: map[string]authCredential{},
		}, nil
	}
	if err != nil {
		return nil, err
	}

	db, err := openAuthDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	snap := &authSnapshot{
		generation:  gen,
		principals:  map[string]authPrincipal{},
		credentials: map[string]authCredential{},
	}
	rows, err := db.QueryContext(ctx, `SELECT id, kind, email, display_name, created_at,
		COALESCE(disabled_at, '') FROM principals`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p authPrincipal
		if err := rows.Scan(&p.ID, &p.Kind, &p.Email, &p.DisplayName, &p.CreatedAt, &p.DisabledAt); err != nil {
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

	rows, err = db.QueryContext(ctx, `SELECT id, principal_id, token_sha256,
		COALESCE(expires_at, ''), COALESCE(last_used_on, ''), COALESCE(revoked_at, '') FROM credentials`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c authCredential
		var hash string
		if err := rows.Scan(&c.ID, &c.PrincipalID, &hash, &c.ExpiresAt, &c.LastUsedOn, &c.RevokedAt); err != nil {
			return nil, err
		}
		snap.credentials[hash] = c
	}
	return snap, rows.Err()
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
	Principal    authPrincipal
	Created      bool
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
func (a *authStore) createPrincipal(ctx context.Context, req api.CreatePrincipalRequest) (*mintedCredential, error) {
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
			COALESCE(disabled_at, '') FROM principals WHERE email = ?`, req.Email).
			Scan(&p.ID, &p.Kind, &p.DisplayName, &p.CreatedAt, &p.DisabledAt)
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

		if _, err := tx.ExecContext(ctx, `INSERT INTO credentials
			(id, principal_id, token_sha256, label, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
			credentialID, p.ID, hash, "bootstrap", createdAt, expiresAt); err != nil {
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
