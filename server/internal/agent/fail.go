package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"blurt/internal/failure"
	"blurt/internal/push"
	"blurt/internal/store"
	"blurt/internal/textutil"
)

// failed is the one place an error lands: a shutdown leaves the message for the
// claimer, a transient failure gets another attempt until the poison pill trips,
// and a permanent one fails now.
func (r *run) failed(ctx context.Context, err error) {
	// A shutdown leaves the message for the claimer; the run's own deadline is
	// an ordinary failure and has to reach the retry/fail path.
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	// The run's deadline cancels ctx, but the failure write still has to land:
	// otherwise the Job is left in flight and loses the attempt it just spent.
	writeCtx := context.WithoutCancel(ctx)
	attempt := r.attempt
	detail := r.trace(failure.Outcome(err))
	detail["error"] = textutil.Truncate(err.Error(), 200)
	if failure.Permanent(err) || attempt >= maxAttempts {
		r.loop.fail(writeCtx, r.job, attempt, detail)
		return
	}
	detail["outcome"] = "retry"
	if err := r.loop.Store.Retry(writeCtx, store.AgentStage, r.job.JobID, attempt, detail); err != nil {
		slog.Error("retry agent job", "job_id", r.job.JobID, "err", err)
		return
	}
	if r.loop.AgentQueue != nil {
		if err := r.loop.AgentQueue.Publish(writeCtx, r.job.JobID, attempt+1); err != nil {
			slog.Error("republish agent job", "job_id", r.job.JobID, "err", err)
		}
	}
}

// fail parks the Job with the same fallback the capture path uses, and keeps it
// visible for recovery.
func (l *AgentLoop) fail(ctx context.Context, job store.Job, attempt int, detail map[string]any) {
	if attempt > maxAttempts {
		attempt = maxAttempts
	}
	detail["attempt"] = attempt
	if err := l.Store.Fail(ctx, store.AgentStage, job.JobID, attempt, FallbackMessage, detail); err != nil {
		slog.Error("fail agent job", "job_id", job.JobID, "err", err)
		return
	}
	l.notify(ctx, job, "Blurt couldn't process that", FallbackMessage)
}

// notify is best effort, like every push Blurt sends: the Job already says the same
// thing in its Conversation, and the push only saves the user a look.
func (l *AgentLoop) notify(ctx context.Context, job store.Job, title, body string) {
	if l.Pusher == nil {
		return
	}
	_, err := l.Pusher.NotifyUser(ctx, job.UserID, push.Message{
		Title:    title,
		Body:     body,
		Tag:      fmt.Sprintf("job-%d", job.JobID),
		Navigate: fmt.Sprintf("/jobs/%d", job.JobID),
	})
	if err != nil {
		slog.Warn("send agent push", "job_id", job.JobID, "err", err)
	}
}
