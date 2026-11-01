package agent

import (
	"context"
	"errors"

	"blurt/internal/failure"
	"blurt/internal/queue"
	"blurt/internal/store"
	"blurt/internal/textutil"
)

// failed is the one place an error lands: a shutdown leaves the message for the
// claimer, a transient failure gets another attempt until the poison pill trips,
// and a permanent one fails now.
func (r *jobRun) failed(ctx context.Context, err error) {
	// A shutdown leaves the message for the claimer; the run's own deadline is
	// an ordinary failure and has to reach the retry/fail path.
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	// The run's deadline cancels ctx, but the failure record still has to land:
	// otherwise the Job is left in flight and loses the attempt it just spent.
	writeCtx := context.WithoutCancel(ctx)
	attempt := r.attempt
	detail := r.trace(failure.Outcome(err))
	detail["error"] = textutil.Truncate(err.Error(), 200)
	if failure.Permanent(err) || attempt >= maxAttempts {
		r.loop.fail(writeCtx, r.job, attempt, failure.Outcome(err), detail)
		return
	}
	detail["outcome"] = "retry"
	queue.Retry(writeCtx, r.loop.Store, r.loop.AgentQueue, store.AgentStage, r.job.JobID, attempt, detail)
}

// RefusedError is a plan the server's own checks would not accept twice. A retry would
// spend another whole budget on the same conversation and be refused again, so the Job fails
// and the user is told rather than asked.
type RefusedError struct {
	Message string
}

func (e *RefusedError) Error() string { return "agent: " + e.Message }

// Retryable is false: the same answer comes back until the model reads the faults, and it
// has already been given the chance to.
func (e *RefusedError) Retryable() bool { return false }

// fail parks the Job with the same fallback and push the capture path uses, and
// keeps it visible for recovery.
func (l *AgentLoop) fail(ctx context.Context, job store.Job, attempt int, outcome string, detail map[string]any) {
	failure.Park(ctx, l.Store, l.Pusher, job, failure.GiveUp{
		Stage: store.AgentStage, Attempt: attempt, Outcome: outcome,
		Title: "Blurt couldn't process that", Detail: detail,
	})
}
