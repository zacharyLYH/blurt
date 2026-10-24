package store

import (
	"context"
	"testing"

	"blurt/internal/db"
)

// The questionnaire replaces this user's answers and advances the version as one
// unit. To prove the transaction earns its keep, trip a failure on an insert
// after the delete has already run and check the delete was rolled back with it.
func TestSaveQuestionnaireRollsBackOnFailure(t *testing.T) {
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	ctx := context.Background()

	users := NewUserStore(handle.Write, handle.Read)
	user, err := users.EnsureUser(ctx, "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}

	answers := []Answer{{Question: "timezone", Answer: "Asia/Singapore"}, {Question: "home", Answer: "Tampines"}}
	if _, err := users.SaveQuestionnaire(ctx, user.UserID, 1, answers); err != nil {
		t.Fatalf("save questionnaire: %v", err)
	}

	// A plain Memory row the questionnaire will not delete, and a unique index
	// the next insert collides with. Any mid-transaction failure would do; this
	// one lands after the delete, which is the statement worth testing.
	if _, err := handle.Write.Exec(`
		INSERT INTO Memory (UserId, Text, Tags, CreatedAt)
		VALUES (?, 'clash' || char(10) || 'Here', 'note', ?)`,
		user.UserID, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if _, err := handle.Write.Exec(`CREATE UNIQUE INDEX test_clash ON Memory(UserId, Text)`); err != nil {
		t.Fatalf("create index: %v", err)
	}

	clash := []Answer{{Question: "clash", Answer: "Here"}}
	if _, err := users.SaveQuestionnaire(ctx, user.UserID, 2, clash); err == nil {
		t.Fatal("SaveQuestionnaire ignored a failed insert")
	}

	var rows int
	if err := handle.Read.QueryRow(
		`SELECT count(*) FROM Memory WHERE UserId = ? AND Tags = 'questionnaire'`,
		user.UserID).Scan(&rows); err != nil {
		t.Fatalf("count questionnaire memory: %v", err)
	}
	if rows != 2 {
		t.Fatalf("questionnaire memory rows = %d, want the original 2", rows)
	}
	// The FTS index rides along inside the same transaction, so it has to still
	// see what the rolled back delete removed.
	var indexed int
	if err := handle.Read.QueryRow(
		`SELECT count(*) FROM fts_memory WHERE fts_memory MATCH ?`, "timezone").Scan(&indexed); err != nil {
		t.Fatalf("query fts: %v", err)
	}
	if indexed == 0 {
		t.Fatal("fts index lost an answer the rolled back delete had removed")
	}
	reloaded, err := users.ByID(ctx, user.UserID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if reloaded.LastQuestionnaireVersion != 1 {
		t.Fatalf("version = %d, want 1: the failed save advanced it", reloaded.LastQuestionnaireVersion)
	}
}
