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

// Retrier is the durable half of a retry.
type Retrier interface {
	Retry(ctx context.Context, stage string, jobID int64, attempt int, detail map[string]any) error
}

// Retry records one failed attempt and announces the next one, in that order: the SQL
// counter is what the attempt ceilings and the claimer read, so it has to land before
// the Job can be picked up again. False means the record did not land, which leaves
// the message pending for the claimer; a publish that fails is logged because the
// claimer and the sweeper both find the Job anyway.
func Retry(ctx context.Context, jobs Retrier, stream Stream, stage string, jobID int64, attempt int, detail map[string]any) bool {
	if err := jobs.Retry(ctx, stage, jobID, attempt, detail); err != nil {
		slog.Error("retry job", "stage", stage, "job_id", jobID, "err", err)
		return false
	}
	if stream == nil {
		return true
	}
	if err := stream.Publish(ctx, jobID, attempt+1); err != nil {
		slog.Error("republish job", "stage", stage, "job_id", jobID, "err", err)
		return false
	}
	return true
}

func ack(ctx context.Context, stream Stream, msgID string) {
	if err := stream.Ack(ctx, msgID); err != nil {
		slog.Error("ack queued job", "msg_id", msgID, "err", err)
	}
}
