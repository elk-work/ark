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

	"github.com/elkproject/ark/internal/server/schema"
)

// ErrConcurrentWrite reports a lost compare-and-swap against the backend.
var ErrConcurrentWrite = errors.New("repository changed concurrently")

// ErrNotFound reports a repository with no stored database.
var ErrNotFound = errors.New("repository not found")

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
		return err
	}
	defer db.Close()
	return fn(db)
}

// Update fetches the current database, runs fn inside one transaction, and
// persists the result with a compare-and-swap. On a lost race it refetches
// and reruns fn (which must be idempotent), up to three attempts.
// create=true initializes a database when none exists.
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

		if err := m.applyAndStore(ctx, repoID, st, gen, fn); err != nil {
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

func (m *Manager) applyAndStore(ctx context.Context, repoID string, st *repoState, gen int64, fn func(tx *sql.Tx) error) error {
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
