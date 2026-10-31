package capture

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"blurt/internal/failure"
	"blurt/internal/push"
	"blurt/internal/queue"
	"blurt/internal/store"
	"blurt/internal/stt"
	"blurt/internal/textutil"
)

const (
	// maxAttempts is the PRD's poison-pill limit for one processing stage,
	// shared with the AgentLoop and the claimer.
	maxAttempts = store.MaxAttempts
	// attemptTimeout bounds one STT call so a hung provider cannot pin a Job.
	// It sits just above the client's own timeout so a slow gateway reports as
	// an STT failure rather than as an interrupted attempt.
	attemptTimeout = 150 * time.Second
	// FallbackMessage is the exact copy the PRD wants in the Job Conversation
	// when the clip cannot be understood.
	FallbackMessage = failure.FallbackMessage
)

// TranscriptionWorker consumes transcription messages, transcribes their pending
// clip, and applies the retry/fallback rules.
type TranscriptionWorker struct {
	Jobs  *store.JobStore
	Audio *Store
	STT   stt.Transcriber
	// TranscriptionQueue carries this worker's inputs; AgentQueue receives
	// finished transcripts.
	TranscriptionQueue queue.Stream
	AgentQueue         queue.Stream
	Pusher             push.Notifier
	Model              string
	Poll               time.Duration
	// AttemptTimeout bounds one STT call; zero means attemptTimeout.
	AttemptTimeout time.Duration
}

// Run blocks until ctx is cancelled.
func (w *TranscriptionWorker) Run(ctx context.Context) {
	consumer := w.consumer()
	consumer.Run(ctx)
}

// consumer binds this stage's step to the shared loop, so the attempt and ack rules
// are the same ones the agent stage runs.
func (w *TranscriptionWorker) consumer() queue.Worker {
	return queue.Worker{Stream: w.TranscriptionQueue, Jobs: w.Jobs, Poll: w.Poll, Step: w.process}
}

// processOne runs at most one message; the tests drive one attempt at a time.
func (w *TranscriptionWorker) processOne(ctx context.Context) bool {
	consumer := w.consumer()
	return consumer.ProcessOne(ctx)
}

// sttTimeout is the STT call's own wall; a slow provider trips it and the Job
// retries rather than pinning the worker.
func (w *TranscriptionWorker) sttTimeout() time.Duration {
	if w.AttemptTimeout > 0 {
		return w.AttemptTimeout
	}
	return attemptTimeout
}

func (w *TranscriptionWorker) process(ctx context.Context, job store.Job, attempt int) bool {
	if job.RetryCount >= attempt {
		attempt = job.RetryCount + 1
	}
	audio, ok := job.PendingAudio()
	if !ok {
		w.publish(ctx, w.AgentQueue, job.JobID)
		return true
	}
	if attempt > maxAttempts {
		attempt = maxAttempts
	}

	file, err := w.Audio.Open(audio)
	if err != nil {
		return w.fail(ctx, job, audio, attempt, "audio_missing", map[string]any{"error": textutil.Truncate(err.Error(), 200)})
	}
	defer file.Close()

	started := time.Now()
	attemptCtx, cancel := context.WithTimeout(ctx, w.sttTimeout())
	text, err := w.STT.Transcribe(attemptCtx, file)
	cancel()
	detail := map[string]any{
		"model":      w.Model,
		"attempt":    attempt,
		"durationMs": time.Since(started).Milliseconds(),
	}

	// Shutdown is not the Job's fault: leave the message pending for the claimer.
	if ctx.Err() != nil {
		return false
	}

	if err != nil {
		detail["error"] = textutil.Truncate(err.Error(), 200)
		if failure.Permanent(err) || attempt >= maxAttempts {
			return w.fail(ctx, job, audio, attempt, failure.Outcome(err), detail)
		}
		detail["outcome"] = "retry"
		return queue.Retry(ctx, w.Jobs, w.TranscriptionQueue, store.TranscriptionStage, job.JobID, attempt, detail)
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return w.fail(ctx, job, audio, attempt, "illegible", detail)
	}
	// A blank transcript is a failed transcription, not a verdict: prescreening is what judges
	// wording, and it runs in front of the planner on both capture paths.
	detail["outcome"] = "ok"
	detail["chars"] = len(text)
	job, err = w.Jobs.SaveTranscript(ctx, job.JobID, audio, text, detail)
	if err != nil {
		// Keep the blob: the claimer retries the message.
		slog.Error("save transcript", "job_id", job.JobID, "err", err)
		return false
	}
	if err := w.Audio.Remove(audio); err != nil {
		slog.Warn("remove transcribed audio", "job_id", job.JobID, "err", err)
	}
	if _, pending := job.PendingAudio(); pending {
		w.publish(ctx, w.TranscriptionQueue, job.JobID)
	} else {
		w.publish(ctx, w.AgentQueue, job.JobID)
	}
	slog.Info("transcribed job", "job_id", job.JobID, "chars", len(text))
	return true
}

func (w *TranscriptionWorker) publish(ctx context.Context, stream queue.Stream, jobID int64) {
	if stream == nil {
		return
	}
	if err := stream.Publish(ctx, jobID, 1); err != nil {
		// The sweeper catches an open Job whose publish never landed.
		slog.Error("publish transcribed job", "job_id", jobID, "err", err)
	}
}

// fail parks the Job and drops the blob it could not use. The record, the push and
// the fallback sentence are the ones the agent stage uses; only the cleanup differs.
func (w *TranscriptionWorker) fail(ctx context.Context, job store.Job, audio string, attempt int, outcome string, detail map[string]any) bool {
	if !failure.Park(ctx, w.Jobs, w.Pusher, job, failure.GiveUp{
		Stage: store.TranscriptionStage, Attempt: attempt, Outcome: outcome,
		Title: "Blurt couldn't hear that", Detail: detail,
	}) {
		// Leave the message pending; the claimer retries it rather than losing it.
		return false
	}
	if err := w.Audio.Remove(audio); err != nil {
		slog.Warn("remove failed audio", "job_id", job.JobID, "err", err)
	}
	return true
}
