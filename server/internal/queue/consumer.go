package queue

import (
	"context"
	"errors"
	"log/slog"

	"blurt/internal/store"
)

// Next returns the next open Job from stream. Messages for a missing or settled
// Job are acknowledged and skipped; the bool reports whether a Job is ready.
func Next(ctx context.Context, stream Stream, jobs *store.JobStore) (store.Job, int, string, bool) {
	jobID, attempt, msgID, err := stream.Consume(ctx)
	if errors.Is(err, ErrEmpty) || ctx.Err() != nil {
		return store.Job{}, 0, "", false
	}
	if err != nil {
		slog.Error("consume queued job", "err", err)
		return store.Job{}, 0, "", false
	}
	job, err := jobs.Get(ctx, jobID)
	if errors.Is(err, store.ErrNotFound) {
		ack(ctx, stream, msgID)
		return store.Job{}, 0, "", false
	}
	if err != nil {
		slog.Error("load queued job", "job_id", jobID, "err", err)
		return store.Job{}, 0, "", false
	}
	if job.Status != store.JobOpen {
		ack(ctx, stream, msgID)
		return store.Job{}, 0, "", false
	}
	return job, attempt, msgID, true
}

func ack(ctx context.Context, stream Stream, msgID string) {
	if err := stream.Ack(ctx, msgID); err != nil {
		slog.Error("ack queued job", "msg_id", msgID, "err", err)
	}
}
