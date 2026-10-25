// Package store is Blurt's SQLite data access, one store per table. Cross
// table work (questionnaire progress) lives in the store of the table that
// owns the lifecycle and documents why.
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound means the row (or session) does not exist or has expired.
var ErrNotFound = errors.New("not found")

// execer is what a transaction and a connection both satisfy, so the row
// builders a Job's transaction needs can run either inside it or on their own.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// nullID keeps a zero id out of a foreign key column; SQLite has no row 0, so
// an unset reference has to be NULL rather than 0.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// timestamp is the single text format used for every UTC column.
func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func now() string {
	return timestamp(time.Now())
}
