// Package sqlite is the SQLite-backed implementation of storage.Store.
// It uses modernc.org/sqlite (pure Go, no CGO) so cross-compilation
// works without a C toolchain.
//
// Migrations are embedded via go:embed and applied with goose on Open.
// New migrations land in migrations/ as goose-format .sql files; sqlc
// regenerates gen/ from queries/.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"

	_ "modernc.org/sqlite"

	"github.com/atterpac/orca/internal/storage"
	"github.com/atterpac/orca/internal/storage/sqlite/gen"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is the SQLite-backed storage.Store.
type Store struct {
	db    *sql.DB
	q     *gen.Queries
	tasks *taskRepo
}

// Open opens (or creates) the sqlite database at path, applies any
// pending goose migrations, and returns a Store. The directory
// containing path is created by the caller — Open does not mkdir.
func Open(path string) (*Store, error) {
	// ?_pragma=foreign_keys(1) turns on FK enforcement; ?_journal=WAL
	// lets readers proceed during writes which matters when the HTTP
	// API reads while the supervisor persists state.
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	q := gen.New(db)
	s := &Store{
		db: db,
		q:  q,
	}
	s.tasks = &taskRepo{q: q}
	return s, nil
}

func migrate(db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	// Silence goose's default log writer; migration failures still
	// surface as returned errors. Daemon log stays focused on orca
	// events rather than schema bookkeeping.
	goose.SetLogger(goose.NopLogger())
	if err := goose.UpContext(context.Background(), db, "migrations"); err != nil {
		return fmt.Errorf("goose migrate: %w", err)
	}
	return nil
}

// Tasks returns the task repository.
func (s *Store) Tasks() storage.TaskRepo { return s.tasks }

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }
