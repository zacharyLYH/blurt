package queue

import (
	"context"
	"log/slog"
	"time"

	"blurt/internal/store"
)

const (
	sweepInterval = time.Minute
	sweepStale    = 2 * time.Minute
)

// Sweeper republishes open Jobs whose SQL commit never reached Redis.
type Sweeper struct {
	Jobs               *store.JobStore
	TranscriptionQueue Stream
	AgentQueue         Stream
}

func (s *Sweeper) Run(ctx context.Context) {
	Every(ctx, sweepInterval, s.sweep)
}

func (s *Sweeper) sweep(ctx context.Context) {
	before := time.Now().Add(-sweepStale).UTC().Format(time.RFC3339)
	jobs, err := s.Jobs.StaleOpenJobs(ctx, before)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("sweep stale jobs", "err", err)
		}
		return
	}
	for _, job := range jobs {
		stream := s.AgentQueue
		if _, pending := job.PendingAudio(); pending {
			stream = s.TranscriptionQueue
		}
		if err := stream.Publish(ctx, job.JobID, job.RetryCount+1); err != nil {
			slog.Error("sweep publish job", "job_id", job.JobID, "err", err)
			continue
		}
		slog.Warn("swept stranded job", "job_id", job.JobID)
	}
}
