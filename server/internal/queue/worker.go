package queue

import (
	"context"
	"time"

	"blurt/internal/store"
)

// defaultPoll is how often a Worker retries an empty stream.
const defaultPoll = time.Second

// Worker is the consume loop every processing stage shares: poll the stream, load
// the Job, take the durable retry counter as the attempt, run the stage's step, and
// acknowledge only when the step says the message is finished with. A stage supplies
// Step and nothing else, so the rules that decide an attempt and an acknowledgement
// exist once rather than once per stage.
type Worker struct {
	Stream Stream
	Jobs   *store.JobStore
	// Poll, when zero, is one second.
	Poll time.Duration
	// Step runs one Job's attempt and reports whether its message may be acked. False
	// means a shutdown or an unrecorded failure: the message stays pending and the
	// claimer hands it back.
	Step func(ctx context.Context, job store.Job, attempt int) bool
}

// Run blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	poll := w.Poll
	if poll <= 0 {
		poll = defaultPoll
	}
	Every(ctx, poll, func(ctx context.Context) { w.ProcessOne(ctx) })
}

// ProcessOne runs at most one message and reports whether one was there. Run loops
// it; a test drives it to take exactly one attempt.
func (w *Worker) ProcessOne(ctx context.Context) bool {
	job, attempt, msgID, ok := Next(ctx, w.Stream, w.Jobs)
	if !ok {
		return false
	}
	// The SQL counter is the durable ceiling; the message attempt may lag it.
	if job.RetryCount >= attempt {
		attempt = job.RetryCount + 1
	}
	if w.Step(ctx, job, attempt) {
		ack(ctx, w.Stream, msgID)
	}
	return true
}
