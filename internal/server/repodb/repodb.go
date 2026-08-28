// Package repodb manages one SQLite database per repository, persisted in a
// blob backend (GCS in production, a local directory in development and
// tests). The database file is the tenant boundary: fetching, snapshotting,
// or deleting a repository is one object operation.
//
// Concurrency model: within a process, a per-repository mutex serializes
// writers. Across processes, Store uses a compare-and-swap on the backend's
// object generation; a lost race returns ErrConcurrentWrite and the caller
// retries against the fresh copy, which is safe because the sync protocol's
// mutations are idempotent.
package repodb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/elk-work/ark/internal/server/schema"
)

// ErrConcurrentWrite reports a lost compare-and-swap against the backend.
var ErrConcurrentWrite = errors.New("repository changed concurrently")

// ErrNotFound reports a repository the service does not hold.
//
// Two shapes of loss arrive here and they are deliberately one answer: the
// object is absent, or the object is present and the database in it holds no
// repository row. SQLite reads a zero-length `repos/<id>.db` as a perfectly
// valid empty database — it opens, the schema applies, and what is missing is
// the repository — so the second shape is a deletion wearing a live object,
// and every route above this package has to say the same thing about both
// (docs/self-hosting.md, elk-work/ark#85).
//
// Answering it here rather than in one handler is the point. While the check
// lived in `handleRegisterRepo` only registration said `404`; pull and push
// fell through to whichever query first noticed the empty table and answered
// `500 {"code":"internal","message":"pull failed"}` — which is the answer for
// "try again later", for a repository that is gone.
var ErrNotFound = errors.New("repository not found")

// ErrNoRepositoryRow is the shape of ErrNotFound that arrives wearing a live
// object. It matches ErrNotFound, because the API contract has exactly one
// answer for a repository the service does not hold; it is its own value so
// that the service log and the message an operator reads can still tell a
// deleted object from a zeroed one. The two want the same restore and not the
// same diagnosis — one says something removed the object, the other says
// something wrote zero bytes over it.
var ErrNoRepositoryRow = fmt.Errorf("%w: its stored database holds no repository row", ErrNotFound)

// ErrCorrupt reports a stored database the service has but cannot use: the
// object is there and the bytes in it will not open as a SQLite database.
//
// It is the third kind this package distinguishes, and it exists for the same
// reason as the other two: the API contract has to be able to say which of
// them happened. Without it the condition arrived at the handler as an
// anonymous error, became `500 {"code":"internal","message":"pull failed"}`,
// and reached the client as exit 6 — offline, retry later — for a state that
// is permanent until an operator restores `repos/<id>.db` from a copy. The
// truth existed in exactly one place, the service's own logs
// (elk-work/ark#65).
//
// A *zero-length* object is deliberately not this. SQLite reads it as a valid
// empty database, so it opens, the schema applies, and what is missing is the
// repository row rather than the bytes — which is a repository the service no
// longer holds, and this package answers that with ErrNoRepositoryRow, so
// every route gives it the 404 it gives an absent object (elk-work/ark#66,
// elk-work/ark#85). Two kinds of damage, two answers, and the difference is
// whether anything can still be read.
var ErrCorrupt = errors.New("stored repository database is unusable")

// CorruptError names the repository whose stored database is unusable. The
// repository is the part worth reporting: the verb that happened to be running
// when it was noticed ("pull failed") is the same for every caller and tells
// nobody anything. None of this is sensitive — it is the operator's own
// storage — so it travels to the client rather than staying in the logs.
type CorruptError struct {
	RepoID string
	// Reason reads as the predicate of a sentence about the repository.
	Reason string
	// Err is SQLite's own complaint, which is the part naming the damage.
	Err error
}

func (e *CorruptError) Error() string {
	msg := fmt.Sprintf("repository %s: %s", e.RepoID, e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *CorruptError) Unwrap() error { return e.Err }

// Is makes every CorruptError match ErrCorrupt, so callers that only need the
// kind can ask for it the way they ask about ErrNotFound.
func (e *CorruptError) Is(target error) bool { return target == ErrCorrupt }

// Backend stores repository database files by ID.
type Backend interface {
	// Fetch downloads the repository database to destPath, returning its
	// generation. ErrNotFound when the repository has no database yet.
	Fetch(ctx context.Context, repoID, destPath string) (generation int64, err error)
	// Store uploads srcPath. ifGeneration is the generation the caller
	// fetched (0 = must not exist); a mismatch returns ErrConcurrentWrite.
	Store(ctx context.Context, repoID, srcPath string, ifGeneration int64) (newGeneration int64, err error)
}

// Manager caches per-repository databases in a scratch directory.
type Manager struct {
	Backend  Backend
	CacheDir string

	mu    sync.Mutex
	repos map[string]*repoState
}

type repoState struct {
	mu         sync.Mutex
	generation int64 // generation the cached file corresponds to; 0 = none
}

func NewManager(backend Backend, cacheDir string) *Manager {
	return &Manager{Backend: backend, CacheDir: cacheDir, repos: map[string]*repoState{}}
}

func (m *Manager) state(repoID string) *repoState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.repos[repoID]
	if !ok {
		st = &repoState{}
		m.repos[repoID] = st
	}
	return st
}

func (m *Manager) dbPath(repoID string) string {
	return filepath.Join(m.CacheDir, repoID+".db")
}

func validRepoID(repoID string) error {
	if repoID == "" || strings.ContainsAny(repoID, "/\\.") {
		return fmt.Errorf("invalid repository id %q", repoID)
	}
	return nil
}

// openAt opens a SQLite database file and applies the schema.
func openAt(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema.SQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return db, nil
}

// rowQuerier is satisfied by both *sql.DB and *sql.Tx, so the one question
// that decides whether a stored database holds a repository is asked the same
// way on the read path and inside the write transaction.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// holdsRepository reports whether an opened database holds the repository row.
// openAt has just applied the schema, so `meta` is always there and this is
// never a missing-table error; the question is whether anything ever
// registered into it. One indexed single-row count on a one-row table, on a
// file that is already open — the cost is not the reason it lived in a handler.
func holdsRepository(q rowQuerier) (bool, error) {
	var n int
	if err := q.QueryRow(`SELECT count(*) FROM meta WHERE id = 1`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// refresh ensures the cached file matches the backend's current generation.
// Caller holds the repo lock. Returns the current generation.
func (m *Manager) refresh(ctx context.Context, repoID string, st *repoState) (int64, error) {
	path := m.dbPath(repoID)
	gen, err := m.Backend.Fetch(ctx, repoID, path+".fetch")
	if errors.Is(err, ErrNotFound) {
		os.Remove(path)
		st.generation = 0
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if gen == st.generation {
		os.Remove(path + ".fetch")
		return gen, nil
	}
	if err := os.Rename(path+".fetch", path); err != nil {
		return 0, err
	}
	st.generation = gen
	return gen, nil
}

// View runs fn against a read-only snapshot of the repository database.
// ErrNotFound when the service does not hold the repository — either shape.
func (m *Manager) View(ctx context.Context, repoID string, fn func(db *sql.DB) error) error {
	if err := validRepoID(repoID); err != nil {
		return err
	}
	st := m.state(repoID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, err := m.refresh(ctx, repoID, st); err != nil {
		return err
	}
	db, err := openAt(m.dbPath(repoID))
	if err != nil {
		// refresh has already established the object exists, so a file that
		// will not open is stored damage rather than a missing repository.
		return &CorruptError{RepoID: repoID, Reason: "its stored database will not open", Err: err}
	}
	defer db.Close()
	// The object opened; whether it holds a repository is a separate question,
	// and a reader has no business seeing the inside of a database that does
	// not. There is no create argument here because every caller of View is
	// reading a repository that is supposed to already exist.
	held, err := holdsRepository(db)
	if err != nil {
		return err
	}
	if !held {
		return ErrNoRepositoryRow
	}
	return fn(db)
}

// Update fetches the current database, runs fn inside one transaction, and
// persists the result with a compare-and-swap. On a lost race it refetches
// and reruns fn (which must be idempotent), up to three attempts.
//
// create=true initializes a database when none exists, and is also what lets
// fn write into a stored database that holds no repository row — the two are
// one condition (see ErrNotFound), so they take one argument. With create
// false, both answer ErrNotFound before fn runs.
func (m *Manager) Update(ctx context.Context, repoID string, create bool, fn func(tx *sql.Tx) error) error {
	if err := validRepoID(repoID); err != nil {
		return err
	}
	st := m.state(repoID)
	st.mu.Lock()
	defer st.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		gen, err := m.refresh(ctx, repoID, st)
		if errors.Is(err, ErrNotFound) {
			if !create {
				return ErrNotFound
			}
			gen = 0
		} else if err != nil {
			return err
		}

		if err := m.applyAndStore(ctx, repoID, st, gen, create, fn); err != nil {
			if errors.Is(err, ErrConcurrentWrite) {
				// Another process won; invalidate the cache and replay.
				st.generation = 0
				lastErr = err
				continue
			}
			return err
		}
		return nil
	}
	return lastErr
}

func (m *Manager) applyAndStore(ctx context.Context, repoID string, st *repoState, gen int64, create bool, fn func(tx *sql.Tx) error) error {
	// Work on a copy so a failed transaction or lost race never corrupts
	// the cached base file.
	base := m.dbPath(repoID)
	work := base + ".work"
	if gen == 0 {
		os.Remove(work)
	} else if err := copyFile(base, work); err != nil {
		return err
	}
	defer os.Remove(work)

	db, err := openAt(work)
	if err != nil {
		if gen == 0 {
			// Nothing was fetched; this file is one we just created in the
			// cache directory, so a failure here is local and ordinary.
			return err
		}
		return &CorruptError{RepoID: repoID, Reason: "its stored database will not open", Err: err}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		return err
	}
	// A writer that was not allowed to create one is looking at a repository
	// the service does not hold, whether the object was absent (refresh
	// answered that already) or is there and empty. Asked inside the
	// transaction, so it sees exactly what fn would have seen; skipped when
	// create is set, because then fn is the thing about to write the row.
	if !create {
		held, err := holdsRepository(tx)
		if err != nil {
			tx.Rollback()
			db.Close()
			return err
		}
		if !held {
			tx.Rollback()
			db.Close()
			return ErrNoRepositoryRow
		}
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
	if err := db.Close(); err != nil {
		return err
	}

	newGen, err := m.Backend.Store(ctx, repoID, work, gen)
	if err != nil {
		return err
	}
	if err := os.Rename(work, base); err != nil {
		st.generation = 0 // cache unknown; next use refetches
		return nil
	}
	st.generation = newGen
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
