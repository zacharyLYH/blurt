// Package db owns the SQLite file: opening it with the pragmas Blurt relies
// on (WAL, foreign keys, busy timeout) and applying the schema on boot.
package db

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// FileName is the database file created inside the data directory.
const FileName = "blurt.db"

// maxReaders bounds the read pool. One device writes little; four concurrent
// readers is plenty until background workers land.
const maxReaders = 4

// DB is one SQLite file behind two pools. SQLite has a single writer but, in
// WAL mode, many concurrent readers: writes take the one-connection pool and
// reads spread across the rest. This keeps a slow read from blocking auth or
// health checks.
type DB struct {
	Read  *sql.DB
	Write *sql.DB
}

func (d *DB) Close() error {
	return errors.Join(d.Read.Close(), d.Write.Close())
}

// Open creates the data directory if needed, opens both pools, and applies the
// schema through the writer. Every failure is fatal for the caller.
func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dataDir, err)
	}

	path := filepath.Join(dataDir, FileName)
	pragmas := "?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)"

	// BEGIN IMMEDIATE makes write transactions take the lock up front. Without
	// it, two deferred transactions can both read then deadlock on upgrade,
	// which busy_timeout cannot fix.
	write, err := openFile("file:"+path+pragmas+"&_pragma=synchronous(NORMAL)&_txlock=immediate", 1)
	if err != nil {
		return nil, err
	}
	if err := migrate(write); err != nil {
		write.Close()
		return nil, err
	}

	// WAL is stored in the file, so the reader inherits it from the writer.
	// query_only keeps a mistakenly routed write from ever reaching this pool.
	read, err := openFile("file:"+path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)", maxReaders)
	if err != nil {
		write.Close()
		return nil, err
	}
	return &DB{Read: read, Write: write}, nil
}

func openFile(dsn string, maxConns int) (*sql.DB, error) {
	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	handle.SetMaxOpenConns(maxConns)
	handle.SetConnMaxIdleTime(5 * time.Minute)
	if err := handle.Ping(); err != nil {
		handle.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return handle, nil
}

func migrate(handle *sql.DB) error {
	if _, err := handle.Exec(schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS cannot add columns to a volume that already
	// has the table, so new columns get one explicit step each.
	return addColumnIfMissing(handle, "User", "LastQuestionnaireVersion",
		"LastQuestionnaireVersion INTEGER NOT NULL DEFAULT 0")
}

func addColumnIfMissing(handle *sql.DB, table, column, definition string) error {
	var present int
	query := fmt.Sprintf("SELECT count(*) FROM pragma_table_info('%s') WHERE name = ?", table)
	if err := handle.QueryRow(query, column).Scan(&present); err != nil {
		return fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	if present > 0 {
		return nil
	}
	if _, err := handle.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", table, definition)); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}
