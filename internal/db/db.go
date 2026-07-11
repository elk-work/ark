// Package db owns the SQLite connection, migrations, and transactions.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ijroth/ark/internal/records"
	"github.com/ijroth/ark/migrations"
)

// Open opens (creating if needed) the Ark database at path and applies
// pending migrations.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", path)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, records.DBErr("open database", err)
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY
	// surprises inside a CLI process.
	d.SetMaxOpenConns(1)
	if err := Migrate(d); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// Migrate applies embedded migrations that have not yet run, in filename
// order, each inside its own transaction. Forward-only.
func Migrate(d *sql.DB) error {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		number TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return records.DBErr("create schema_migrations", err)
	}

	applied := map[string]bool{}
	rows, err := d.Query(`SELECT number FROM schema_migrations`)
	if err != nil {
		return records.DBErr("read schema_migrations", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return records.DBErr("scan schema_migrations", err)
		}
		applied[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return records.DBErr("read schema_migrations", err)
	}

	names, err := migrationNames(migrations.FS)
	if err != nil {
		return err
	}
	for _, name := range names {
		num := strings.SplitN(name, "_", 2)[0]
		if applied[num] {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return records.DBErr("read migration "+name, err)
		}
		tx, err := d.Begin()
		if err != nil {
			return records.DBErr("begin migration "+name, err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return records.DBErr("apply migration "+name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (number, applied_at) VALUES (?, ?)`,
			num, records.Now()); err != nil {
			tx.Rollback()
			return records.DBErr("record migration "+name, err)
		}
		if err := tx.Commit(); err != nil {
			return records.DBErr("commit migration "+name, err)
		}
	}
	return nil
}

func migrationNames(fsys embed.FS) ([]string, error) {
	entries, err := fsys.ReadDir(".")
	if err != nil {
		return nil, records.DBErr("list migrations", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// InTx runs fn inside a transaction, committing on nil and rolling back on
// error. Every state-mutating command uses exactly one InTx call so the
// record change and its mutation land atomically.
func InTx(ctx context.Context, d *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return records.DBErr("begin transaction", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return records.DBErr("commit transaction", err)
	}
	return nil
}
