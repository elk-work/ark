// Package servertest provides disposable PostgreSQL databases for tests.
// Point ARK_TEST_PG at a Postgres DSN with createdb rights; without one,
// tests that need Postgres skip.
package servertest

import (
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ijroth/ark/internal/records"
	"github.com/ijroth/ark/internal/server/schema"
)

// Base returns the admin DSN, skipping the test when Postgres is absent.
func Base(t *testing.T) string {
	t.Helper()
	base := os.Getenv("ARK_TEST_PG")
	if base == "" {
		base = "postgres://ark@127.0.0.1:5499/arktest"
	}
	db, err := sql.Open("pgx", base)
	if err == nil {
		err = db.Ping()
		db.Close()
	}
	if err != nil {
		t.Skipf("no test Postgres at %s (set ARK_TEST_PG): %v", base, err)
	}
	return base
}

// NewDB creates a throwaway database with the server schema applied, and
// drops it on cleanup.
func NewDB(t *testing.T) *sql.DB {
	t.Helper()
	base := Base(t)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	name := "arktest_" + strings.ToLower(records.NewID())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if _, err := db.Exec(schema.SQL); err != nil {
		db.Close()
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
