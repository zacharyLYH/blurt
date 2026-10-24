// Package store is Blurt's SQLite data access, one store per table. Cross
// table work (questionnaire progress) lives in the store of the table that
// owns the lifecycle and documents why.
package store

import (
	"errors"
	"time"
)

// ErrNotFound means the row (or session) does not exist or has expired.
var ErrNotFound = errors.New("not found")

// timestamp is the single text format used for every UTC column.
func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func now() string {
	return timestamp(time.Now())
}
