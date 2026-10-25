package agent

import (
	"context"
	"log/slog"
	"time"

	"blurt/internal/queue"
	"blurt/internal/store"
)

// defaultPoll is how often the consumer retries an empty stream.
const defaultPoll = time.Second

// AgentWorker consumes the agent queue and runs each Job's AgentLoop.
type AgentWorker struct {
	Jobs       *store.JobStore
	Loop       *AgentLoop
	AgentQueue queue.Stream
	// Poll, when zero, is defaultPoll.
	Poll time.Duration
}

// Run blocks until ctx is cancelled.
func (w *AgentWorker) Run(ctx context.Context) {
	if w.Poll <= 0 {
		w.Poll = defaultPoll
	}
	queue.Every(ctx, w.Poll, func(ctx context.Context) { w.processOne(ctx) })
}

func (w *AgentWorker) processOne(ctx context.Context) bool {
	job, attempt, msgID, ok := queue.Next(ctx, w.AgentQueue, w.Jobs)
	if !ok {
		return false
	}
	// The SQL counter is the durable ceiling; the message attempt may lag it.
	if job.RetryCount >= attempt {
		attempt = job.RetryCount + 1
	}
	w.Loop.Run(ctx, job, attempt)
	// A shutdown leaves the message pending for the claimer.
	if ctx.Err() == nil {
		w.ack(ctx, msgID)
	}
	return true
}

func (w *AgentWorker) ack(ctx context.Context, msgID string) {
	if err := w.AgentQueue.Ack(ctx, msgID); err != nil {
		slog.Error("ack agent job", "msg_id", msgID, "err", err)
	}
}
