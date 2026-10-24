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

func TestClaimTranscriptionPipeline(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()

	first, err := jobs.Create(ctx, userID, Message{Role: "user", Audio: "aaaa0000aaaa0000.wav"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := jobs.Create(ctx, userID, Message{Role: "user", Audio: "bbbb0000bbbb0000.wav"})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := jobs.Create(ctx, userID, Message{Role: "user", Text: "typed"}); err != nil {
		t.Fatalf("create text: %v", err)
	}

	claimed, ok, err := jobs.ClaimTranscription(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.JobID != first.JobID || claimed.Status != JobProcessing {
		t.Fatalf("claimed = %+v", claimed)
	}
	// One Job per user: the second waits even though its clip is ready.
	if _, ok, err := jobs.ClaimTranscription(ctx); err != nil || ok {
		t.Fatalf("second claim: ok=%v err=%v", ok, err)
	}

	if err := jobs.SaveTranscript(ctx, first.JobID, "aaaa0000aaaa0000.wav", "buy glue",
		map[string]any{"outcome": "ok"}); err != nil {
		t.Fatalf("save transcript: %v", err)
	}
	stored, err := jobs.ByID(ctx, userID, first.JobID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.Status != JobQueued || stored.Conversation[0].Text != "buy glue" {
		t.Fatalf("stored = %+v", stored)
	}
	// The second audio Job is claimable now; the typed Job never is, and the
	// transcribed Job must not be claimed again (the agent poller owns it).
	claimed, ok, err = jobs.ClaimTranscription(ctx)
	if err != nil || !ok {
		t.Fatalf("claim second: ok=%v err=%v", ok, err)
	}
	if claimed.JobID != second.JobID {
		t.Fatalf("claimed job = %d, want %d", claimed.JobID, second.JobID)
	}
	if _, ok, err := jobs.ClaimTranscription(ctx); err != nil || ok {
		t.Fatalf("claim with nothing left: ok=%v err=%v", ok, err)
	}

	var traces int
	if err := handle.Read.QueryRow(`SELECT count(*) FROM Trace WHERE JobId = ?`, first.JobID).Scan(&traces); err != nil {
		t.Fatalf("count traces: %v", err)
	}
	if traces != 1 {
		t.Fatalf("traces = %d, want 1", traces)
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
