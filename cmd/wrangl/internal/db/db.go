// Package db owns the controller's SQLite database: embedded goose migrations, sqlc
// queries (generated into dbgen) and connection setup.
//
// Regenerate dbgen with `task sqlc` after editing migrations/ or queries/.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // pure-Go driver "sqlite"; keeps CGO_ENABLED=0

	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB is a single-connection writer plus a reader pool over one WAL-mode file.
// Funnelling every write through one connection avoids SQLITE_BUSY between writers.
type DB struct {
	W  *sql.DB
	R  *sql.DB
	Q  *dbgen.Queries // bound to W
	RQ *dbgen.Queries // bound to R
}

func dsn(path string, write bool) string {
	q := url.Values{}
	for _, p := range []string{"foreign_keys(1)", "journal_mode(WAL)", "busy_timeout(5000)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	if write {
		// BEGIN IMMEDIATE: take the write lock up front instead of failing on upgrade.
		q.Set("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

func provider(conn *sql.DB) (*goose.Provider, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectSQLite3, conn, sub)
}

// Open opens (creating if needed) the database at path and applies all migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	p, err := provider(w)
	if err != nil {
		w.Close()
		return nil, err
	}
	if _, err := p.Up(ctx); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	r, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		w.Close()
		return nil, err
	}
	if err := r.PingContext(ctx); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	return &DB{W: w, R: r, Q: dbgen.New(w), RQ: dbgen.New(r)}, nil
}

// Tx runs fn in a write transaction, committing if it returns nil.
func (d *DB) Tx(ctx context.Context, fn func(*dbgen.Queries) error) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(d.Q.WithTx(tx)); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

// Close closes both pools.
func (d *DB) Close() error { return errors.Join(d.R.Close(), d.W.Close()) }

// Migrate runs a goose operation against path: "up" (all pending), "down" (one
// step) or "status" (printed to out).
func Migrate(ctx context.Context, path, direction string, out io.Writer) error {
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return err
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	p, err := provider(w)
	if err != nil {
		return err
	}
	switch direction {
	case "up":
		_, err = p.Up(ctx)
	case "down":
		_, err = p.Down(ctx)
	case "status":
		var st []*goose.MigrationStatus
		if st, err = p.Status(ctx); err == nil {
			for _, s := range st {
				applied := "pending"
				if s.State == goose.StateApplied {
					applied = "applied " + s.AppliedAt.UTC().Format("2006-01-02T15:04:05Z")
				}
				fmt.Fprintf(out, "%05d  %-40s %s\n", s.Source.Version, s.Source.Path, applied)
			}
		}
	default:
		return fmt.Errorf("unknown migrate direction %q: want up, down or status", direction)
	}
	return err
}
