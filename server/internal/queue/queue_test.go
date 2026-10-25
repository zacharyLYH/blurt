package queue

import (
	"context"
	"testing"
	"time"

	"blurt/internal/db"
	"blurt/internal/store"
)

func newStack(t *testing.T) (*store.JobStore, *db.DB, int64) {
	t.Helper()
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	user, err := store.NewUserStore(handle.Write, handle.Read).EnsureUser(
		context.Background(), "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	return store.NewJobStore(handle.Write, handle.Read), handle, user.UserID
}

func consume(t *testing.T, stream Stream) (int64, int) {
	t.Helper()
	jobID, attempt, _, err := stream.Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	return jobID, attempt
}

func TestClaimerRetriesThenFailsAtThePoisonPill(t *testing.T) {
	jobs, _, userID := newStack(t)
	ctx := context.Background()
	job, err := jobs.Create(ctx, userID, store.Message{Role: "user", Text: "buy glue"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	stream := NewMock()
	if err := stream.Publish(ctx, job.JobID, 1); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, attempt := consume(t, stream); attempt != 1 {
		t.Fatalf("attempt = %d, want 1", attempt)
	}
	claimer := &Claimer{Stream: stream, Jobs: jobs, Stage: store.AgentStage, Fallback: "fallback"}
	claimer.reclaim(ctx)
	if stored, _ := jobs.Get(ctx, job.JobID); stored.Status != store.JobOpen || stored.RetryCount != 1 {
		t.Fatalf("stored = %+v, want a retry", stored)
	}
	if _, attempt := consume(t, stream); attempt != 2 {
		t.Fatalf("republished attempt = %d, want 2", attempt)
	}
	claimer.reclaim(ctx)
	if _, attempt := consume(t, stream); attempt != 3 {
		t.Fatalf("republished attempt = %d, want 3", attempt)
	}

	// A third death trips the poison pill and leaves no message.
	claimer.reclaim(ctx)
	if stored, _ := jobs.Get(ctx, job.JobID); stored.Status != store.JobFailed || stored.RetryCount != 3 {
		t.Fatalf("stored = %+v, want failed", stored)
	}
	if _, _, _, err := stream.Consume(ctx); err != ErrEmpty {
		t.Fatalf("consume after poison pill = %v, want ErrEmpty", err)
	}
}

func TestSweeperRepublishesStaleJobsToTheirStage(t *testing.T) {
	jobs, handle, userID := newStack(t)
	ctx := context.Background()
	text, err := jobs.Create(ctx, userID, store.Message{Role: "user", Text: "buy glue"})
	if err != nil {
		t.Fatalf("create text: %v", err)
	}
	clip, err := jobs.Create(ctx, userID, store.Message{Role: "user", Audio: "aaaa0000aaaa0000.wav"})
	if err != nil {
		t.Fatalf("create clip: %v", err)
	}
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, id := range []int64{text.JobID, clip.JobID} {
		if _, err := handle.Write.Exec(`UPDATE Job SET queued_at = ? WHERE JobId = ?`, old, id); err != nil {
			t.Fatalf("age job: %v", err)
		}
	}

	transcription, agent := NewMock(), NewMock()
	sweeper := &Sweeper{Jobs: jobs, TranscriptionQueue: transcription, AgentQueue: agent}
	sweeper.sweep(ctx)

	if id, attempt := consume(t, agent); id != text.JobID || attempt != 1 {
		t.Fatalf("agent message = %d/%d, want %d/1", id, attempt, text.JobID)
	}
	if id, attempt := consume(t, transcription); id != clip.JobID || attempt != 1 {
		t.Fatalf("transcription message = %d/%d, want %d/1", id, attempt, clip.JobID)
	}
}

// A message whose Job has settled, or whose Job no longer exists, is consumed
// and acknowledged rather than handed to a worker: the status is the guard.
func TestNextAcksJobsThatAreNotOpen(t *testing.T) {
	jobs, _, userID := newStack(t)
	ctx := context.Background()
	job, err := jobs.Create(ctx, userID, store.Message{Role: "user", Text: "buy glue"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, store.AgentStep{
		Reply: store.Message{Text: "Reminder set."}, Status: store.JobComplete}); err != nil {
		t.Fatalf("close job: %v", err)
	}

	stream := NewMock()
	for _, id := range []int64{job.JobID, job.JobID + 1000} {
		if err := stream.Publish(ctx, id, 1); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if _, _, _, ok := Next(ctx, stream, jobs); ok {
			t.Fatalf("Next claimed Job %d, want it skipped", id)
		}
	}

	// Skipped messages are gone rather than left pending for the claimer.
	if messages, err := stream.Claim(ctx, 0); err != nil || len(messages) != 0 {
		t.Fatalf("pending = %+v, %v, want none", messages, err)
	}
}
