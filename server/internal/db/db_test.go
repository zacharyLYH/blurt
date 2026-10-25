package db

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	handle, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return handle
}

func TestReadsRunAlongsideWriter(t *testing.T) {
	handle := openTestDB(t)
	if got := handle.Write.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("writer max connections = %d, want 1", got)
	}
	if got := handle.Read.Stats().MaxOpenConnections; got != maxReaders {
		t.Fatalf("reader max connections = %d, want %d", got, maxReaders)
	}

	// BEGIN IMMEDIATE holds the write lock. WAL readers must still be able to
	// take snapshots instead of waiting for the transaction to finish.
	tx, err := handle.Write.Begin()
	if err != nil {
		t.Fatalf("begin write tx: %v", err)
	}
	defer tx.Rollback()

	errs := make(chan error, maxReaders)
	for range maxReaders {
		go func() {
			var count int
			errs <- handle.Read.QueryRow(`SELECT count(*) FROM User`).Scan(&count)
		}()
	}
	for range maxReaders {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent read: %v", err)
		}
	}
}

func TestWriterPoolBlocksExtraWrites(t *testing.T) {
	handle := openTestDB(t)
	held, err := handle.Write.Conn(context.Background())
	if err != nil {
		t.Fatalf("hold writer connection: %v", err)
	}
	t.Cleanup(func() { held.Close() })

	baseline := handle.Write.Stats().WaitCount
	done := make(chan error, 1)
	go func() {
		_, err := handle.Write.Exec(
			`INSERT INTO User (Name, Email, CreatedAt) VALUES ('blocked', 'blocked@example.com', '2026-01-01T00:00:00Z')`)
		done <- err
	}()

	waitFor(t, "second write to wait for the writer", func() bool {
		return handle.Write.Stats().WaitCount > baseline
	})
	select {
	case err := <-done:
		t.Fatalf("write completed while the only writer was held: %v", err)
	default:
	}
	if got := handle.Write.Stats().InUse; got != 1 {
		t.Fatalf("writer connections in use = %d, want 1", got)
	}

	held.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write after release: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write did not resume after the writer was released")
	}

	var count int
	if err := handle.Read.QueryRow(`SELECT count(*) FROM User WHERE Email = 'blocked@example.com'`).Scan(&count); err != nil {
		t.Fatalf("read inserted user: %v", err)
	}
	if count != 1 {
		t.Fatalf("inserted users = %d, want 1", count)
	}
}

func TestReaderPoolBlocksExtraReads(t *testing.T) {
	handle := openTestDB(t)
	held := make([]*sql.Conn, 0, maxReaders)
	for range maxReaders {
		conn, err := handle.Read.Conn(context.Background())
		if err != nil {
			t.Fatalf("hold reader connection: %v", err)
		}
		held = append(held, conn)
	}
	t.Cleanup(func() {
		for _, conn := range held {
			conn.Close()
		}
	})

	if got := handle.Read.Stats().InUse; got != maxReaders {
		t.Fatalf("reader connections in use = %d, want %d", got, maxReaders)
	}

	baseline := handle.Read.Stats().WaitCount
	done := make(chan error, 1)
	go func() {
		var count int
		done <- handle.Read.QueryRow(`SELECT count(*) FROM User`).Scan(&count)
	}()

	waitFor(t, "fifth read to wait for a reader", func() bool {
		return handle.Read.Stats().WaitCount > baseline
	})
	select {
	case err := <-done:
		t.Fatalf("read completed while every reader was held: %v", err)
	default:
	}

	held[0].Close()
	held = held[1:]
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read after release: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read did not resume after a reader was released")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSchemaIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	for range 2 {
		handle, err := Open(dir)
		if err != nil {
			t.Fatalf("second Open: %v", err)
		}
		handle.Close()
	}
}

func TestLoginPinsAreNotPersisted(t *testing.T) {
	handle := openTestDB(t)
	var count int
	if err := handle.Read.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'LoginPin'`,
	).Scan(&count); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if count != 0 {
		t.Fatal("LoginPin table exists: PINs belong in the auth service's in-memory map")
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	handle := openTestDB(t)
	_, err := handle.Write.Exec(
		`INSERT INTO Memory (UserId, Text, Tags, CreatedAt) VALUES (999, 'orphan', '', '2026-01-01T00:00:00Z')`,
	)
	if err == nil {
		t.Fatal("expected foreign key violation for unknown UserId")
	}
}

// The queue sweeper scans Job by (Status, queued_at), so both must exist.
// The 4-value Status CHECK is the Redis-Streams simplification: no staging
// statuses leak into SQL.
func TestJobSchemaMatchesTheQueueContract(t *testing.T) {
	handle := openTestDB(t)
	for _, def := range []string{
		`queued_at`, `Status`, `RetryCount`,
	} {
		var count int
		if err := handle.Read.QueryRow(
			`SELECT count(*) FROM pragma_table_info('Job') WHERE name = ?`, def,
		).Scan(&count); err != nil {
			t.Fatalf("read column %s: %v", def, err)
		}
		if count != 1 {
			t.Fatalf("Job.%s column is missing", def)
		}
	}
	var hasQueuedIndex int
	if err := handle.Read.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_job_queued'`,
	).Scan(&hasQueuedIndex); err != nil {
		t.Fatalf("read index: %v", err)
	}
	if hasQueuedIndex != 1 {
		t.Fatal("idx_job_queued index is missing")
	}
}

func TestMemoryFtsStaysInSync(t *testing.T) {
	handle := openTestDB(t)
	now := "2026-01-01T00:00:00Z"
	if _, err := handle.Write.Exec(
		`INSERT INTO User (Name, Email, CreatedAt) VALUES ('Lam', 'lam@example.com', ?)`, now,
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	res, err := handle.Write.Exec(
		`INSERT INTO Memory (UserId, Text, Tags, CreatedAt) VALUES (1, 'milk is cheaper at store A', 'groceries', ?)`, now,
	)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	memoryID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}

	assertHits := func(query string, want int) {
		t.Helper()
		var hits int
		if err := handle.Read.QueryRow(
			`SELECT count(*) FROM fts_memory WHERE fts_memory MATCH ? AND rowid = ?`, query, memoryID,
		).Scan(&hits); err != nil {
			t.Fatalf("fts query %q: %v", query, err)
		}
		if hits != want {
			t.Fatalf("fts query %q: got %d hits, want %d", query, hits, want)
		}
	}

	assertHits("milk", 1)

	if _, err := handle.Write.Exec(`UPDATE Memory SET Text = 'milk is cheaper at store B' WHERE MemoryId = ?`, memoryID); err != nil {
		t.Fatalf("update memory: %v", err)
	}
	assertHits("milk", 1)

	if _, err := handle.Write.Exec(`DELETE FROM Memory WHERE MemoryId = ?`, memoryID); err != nil {
		t.Fatalf("delete memory: %v", err)
	}
	assertHits("milk", 0)
}
