package queue

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"blurt/internal/store"
)

const claimInterval = 30 * time.Second

// Claimer reclaims messages whose consumer died before acknowledging them.
type Claimer struct {
	Stream   Claiming
	Jobs     *store.JobStore
	Stage    string
	Fallback string
	MinIdle  time.Duration
}

func (c *Claimer) Run(ctx context.Context) {
	Every(ctx, claimInterval, c.reclaim)
}

func (c *Claimer) reclaim(ctx context.Context) {
	messages, err := c.Stream.Claim(ctx, c.MinIdle)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("claim queue messages", "stage", c.Stage, "err", err)
		}
		return
	}
	for _, message := range messages {
		if !c.recover(ctx, message) {
			return
		}
	}
}

func (c *Claimer) recover(ctx context.Context, message Message) bool {
	job, err := c.Jobs.Get(ctx, message.JobID)
	if errors.Is(err, store.ErrNotFound) {
		c.ack(ctx, message.ID)
		return true
	}
	if err != nil {
		slog.Error("load claimed job", "job_id", message.JobID, "err", err)
		return false
	}
	if job.Status != store.JobOpen {
		c.ack(ctx, message.ID)
		return true
	}

	// The SQL counter is the durable ceiling; the message attempt may lag it.
	attempt := message.Attempt
	if job.RetryCount > attempt {
		attempt = job.RetryCount
	}
	detail := map[string]any{"outcome": "requeue", "attempt": attempt}
	if attempt >= store.MaxAttempts {
		detail["outcome"] = "failed"
		if err := c.Jobs.Fail(ctx, c.Stage, message.JobID, attempt, c.Fallback, detail); err != nil {
			slog.Error("fail reclaimed job", "job_id", message.JobID, "err", err)
			return false
		}
		slog.Warn("claimed job failed at poison pill", "job_id", message.JobID, "stage", c.Stage)
	} else {
		if err := c.Jobs.Retry(ctx, c.Stage, message.JobID, attempt, detail); err != nil {
			slog.Error("retry reclaimed job", "job_id", message.JobID, "err", err)
			return false
		}
		if err := c.Stream.Publish(ctx, message.JobID, attempt+1); err != nil {
			slog.Error("republish reclaimed job", "job_id", message.JobID, "err", err)
			return false
		}
		slog.Warn("reclaimed job", "job_id", message.JobID, "stage", c.Stage, "attempt", attempt)
	}
	c.ack(ctx, message.ID)
	return true
}

func (c *Claimer) ack(ctx context.Context, msgID string) {
	if err := c.Stream.Ack(ctx, msgID); err != nil {
		slog.Error("ack claimed message", "msg_id", msgID, "err", err)
	}
}
