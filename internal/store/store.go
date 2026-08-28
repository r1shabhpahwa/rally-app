// Package store owns the SQLite database: connection setup, migrations and all
// queries. Nothing outside this package writes SQL.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO, so the binary stays static
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store is the database handle.
type Store struct {
	db *sql.DB
}

// Open connects to the SQLite file, applies pending migrations and returns a
// ready Store. The connection pool is capped at one: at this scale, serialising
// every query costs nothing and removes last-slot races by construction rather
// than by careful locking.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for maintenance tasks such as backups.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migration (
		name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	applied := map[string]bool{}
	rows, err := s.db.Query(`SELECT name FROM schema_migration`)
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		applied[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migration (name, applied_at) VALUES (?, unixepoch())`, name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// WithTx runs fn inside a transaction, rolling back on error. Callers that
// check capacity and then write must use this so the check and the write cannot
// be interleaved with another signup.
func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Meta reads a value from app_meta.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetMeta writes a value to app_meta.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO app_meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// MetaOrCreate returns the stored value for key, generating and persisting one
// with gen if it is missing. Used for the cookie signing secret so logins
// survive restarts without operators having to set another env var.
func (s *Store) MetaOrCreate(ctx context.Context, key string, gen func() string) (string, error) {
	v, err := s.Meta(ctx, key)
	if err != nil {
		return "", err
	}
	if v != "" {
		return v, nil
	}
	v = gen()
	if err := s.SetMeta(ctx, key, v); err != nil {
		return "", err
	}
	return v, nil
}

// NewToken returns a 256-bit URL-safe random token. These are the only
// credential participants have, so they must be unguessable.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Backup writes a consistent snapshot of the database to path using SQLite's
// own VACUUM INTO, so no external tooling is needed on the VM.
func (s *Store) Backup(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		// VACUUM INTO refuses to overwrite, and a stale same-named file would
		// otherwise fail every run from here on.
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}
