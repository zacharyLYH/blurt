package store

import (
	"context"
	"testing"

	"blurt/internal/db"
)

func newJobStack(t *testing.T) (*JobStore, *db.DB, int64) {
	t.Helper()
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	user, err := NewUserStore(handle.Write, handle.Read).EnsureUser(
		context.Background(), "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	return NewJobStore(handle.Write, handle.Read), handle, user.UserID
}

func TestCreateStartsOpen(t *testing.T) {
	jobs, _, userID := newJobStack(t)
	job, err := jobs.Create(context.Background(), userID,
		Message{Role: "user", Text: "buy glue"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Status != JobOpen {
		t.Fatalf("status = %q, want %q", job.Status, JobOpen)
	}
}

// Every clip gets its own transcript; the Job stays open until the last one is
// done, then the caller sends it to the agent stream.
func TestSaveTranscriptFillsClipsAndResetsAttempts(t *testing.T) {
	jobs, _, userID := newJobStack(t)
	ctx := context.Background()
	job, err := jobs.Create(ctx, userID,
		Message{Role: "user", Audio: "aaaa0000aaaa0000.wav"},
		Message{Role: "user", Audio: "cccc0000cccc0000.wav"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := jobs.Retry(ctx, TranscriptionStage, job.JobID, 2, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	stored, err := jobs.SaveTranscript(ctx, job.JobID, "aaaa0000aaaa0000.wav", "buy glue",
		map[string]any{"outcome": "ok"})
	if err != nil {
		t.Fatalf("save first: %v", err)
	}
	if stored.RetryCount != 0 || stored.Conversation[0].Text != "buy glue" {
		t.Fatalf("stored = %+v", stored)
	}
	if audio, ok := stored.PendingAudio(); !ok || audio != "cccc0000cccc0000.wav" {
		t.Fatalf("pending audio = %q ok=%v", audio, ok)
	}
	stored, err = jobs.SaveTranscript(ctx, job.JobID, "cccc0000cccc0000.wav", "and milk",
		map[string]any{"outcome": "ok"})
	if err != nil {
		t.Fatalf("save second: %v", err)
	}
	if stored.Status != JobOpen {
		t.Fatalf("status = %q, want open", stored.Status)
	}
	if _, ok := stored.PendingAudio(); ok {
		t.Fatal("a fully transcribed Job still has pending audio")
	}
}

func TestAppendTurnRetiresAPendingClip(t *testing.T) {
	jobs, _, userID := newJobStack(t)
	ctx := context.Background()
	job, err := jobs.Create(ctx, userID, Message{Role: "user", Audio: "aaaa0000aaaa0000.wav"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stored, err := jobs.AppendTurn(ctx, userID, job.JobID,
		Message{Text: "buy glue", TZ: "Asia/Singapore"}, nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, pending := stored.PendingAudio(); pending {
		t.Fatal("typed reply kept the stale clip")
	}
}

func TestJobReadIsScopedToOwner(t *testing.T) {
	jobs, _, userID := newJobStack(t)
	ctx := context.Background()
	job, err := jobs.Create(ctx, userID, Message{Role: "user", Text: "mine"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := jobs.ByID(ctx, userID+1, job.JobID); err != ErrNotFound {
		t.Fatalf("foreign read err = %v, want ErrNotFound", err)
	}
}
