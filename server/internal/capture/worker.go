package capture

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"blurt/internal/decision"
	"blurt/internal/push"
	"blurt/internal/store"
	"blurt/internal/stt"
)

const (
	// maxAttempts is the PRD's poison-pill limit for one processing stage.
	maxAttempts = 3
	// attemptTimeout bounds one STT call so a hung provider cannot pin a Job.
	// It sits just above the client's own timeout so a slow gateway reports as
	// an STT failure rather than as an interrupted attempt.
	attemptTimeout = 150 * time.Second
	// gateTimeout bounds the transcript-quality check so a hung classifier cannot
	// pin a Job; it sits just above the decision client's own timeout.
	gateTimeout = 30 * time.Second
	defaultPoll = time.Second
	// FallbackMessage is the exact copy the PRD wants in the Job Conversation
	// when the clip cannot be understood.
	FallbackMessage = "I couldn't process that. Please say it again or type it."
	// requestLabel and noiseLabel are the criteria the gate's transcript question
	// offers; laya answers with one of the two.
	requestLabel = "request"
	noiseLabel   = "noise"
)

// Notifier is the push fan-out the worker uses after a permanent failure.
type Notifier interface {
	NotifyUser(ctx context.Context, userID int64, msg push.Message) (push.Result, error)
}

// Worker polls queued Jobs, transcribes their pending clip, and applies the
// retry/fallback rules. One goroutine is enough in phase 1: SQLite has one
// writer and Jobs are tiny.
type Worker struct {
	Jobs  *store.JobStore
	Audio *Store
	STT   stt.Transcriber
	// Gate is the transcript-quality filter. Nil is valid: with no decision
	// gateway configured the check is skipped entirely.
	Gate      decision.Decider
	GateModel string
	Pusher    Notifier
	Model     string
	Poll      time.Duration
}

// Run blocks until ctx is cancelled. A crash leaves a Job in processing; the
// first pass requeues it and counts that as a failed attempt.
func (w *Worker) Run(ctx context.Context) {
	if w.Poll <= 0 {
		w.Poll = defaultPoll
	}
	if count, err := w.Jobs.RequeueInterrupted(ctx); err != nil {
		slog.Error("requeue interrupted jobs", "err", err)
	} else if count > 0 {
		slog.Warn("requeued interrupted jobs", "count", count)
	}

	ticker := time.NewTicker(w.Poll)
	defer ticker.Stop()
	for {
		w.processOne(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) processOne(ctx context.Context) bool {
	job, ok, err := w.Jobs.ClaimTranscription(ctx)
	if err != nil {
		slog.Error("claim transcription", "err", err)
		return false
	}
	if !ok {
		return false
	}
	w.process(ctx, job)
	return true
}

func (w *Worker) process(ctx context.Context, job store.Job) {
	audio, ok := job.PendingAudio()
	if !ok {
		return
	}
	if job.RetryCount >= maxAttempts {
		w.fail(ctx, job, audio, maxAttempts, "attempts_exhausted", nil)
		return
	}

	file, err := w.Audio.Open(audio)
	if err != nil {
		w.fail(ctx, job, audio, job.RetryCount+1, "audio_missing", map[string]any{"error": truncate(err.Error(), 200)})
		return
	}
	defer file.Close()

	started := time.Now()
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	text, err := w.STT.Transcribe(attemptCtx, file)
	cancel()
	detail := map[string]any{
		"model":      w.Model,
		"attempt":    job.RetryCount + 1,
		"durationMs": time.Since(started).Milliseconds(),
	}

	// Shutdown is not the Job's fault: leave it in processing so the next
	// boot's requeue gives it a fresh attempt.
	if ctx.Err() != nil {
		return
	}

	if err != nil {
		detail["error"] = truncate(err.Error(), 200)
		if permanent(err) || job.RetryCount+1 >= maxAttempts {
			w.fail(ctx, job, audio, job.RetryCount+1, outcomeFor(err), detail)
			return
		}
		detail["outcome"] = "retry"
		if err := w.Jobs.RetryTranscription(ctx, job.JobID, job.RetryCount+1, detail); err != nil {
			slog.Error("retry transcription", "job_id", job.JobID, "err", err)
		}
		return
	}

	text = strings.TrimSpace(text)
	if text == "" {
		w.fail(ctx, job, audio, job.RetryCount+1, "illegible", detail)
		return
	}
	if w.Gate != nil {
		noise, err := w.transcriptNoise(ctx, text, detail)
		if err != nil {
			// The filter is advisory: a classifier that is down must not block
			// a capture. The trace keeps the reason.
			slog.Warn("transcript gate", "job_id", job.JobID, "err", err)
		} else if noise {
			w.fail(ctx, job, audio, job.RetryCount+1, "noise", detail)
			return
		}
	}
	detail["outcome"] = "ok"
	detail["chars"] = len(text)
	if err := w.Jobs.SaveTranscript(ctx, job.JobID, audio, text, detail); err != nil {
		// Keep the blob: a restart requeues the Job and tries again.
		slog.Error("save transcript", "job_id", job.JobID, "err", err)
		return
	}
	if err := w.Audio.Remove(audio); err != nil {
		slog.Warn("remove transcribed audio", "job_id", job.JobID, "err", err)
	}
	slog.Info("transcribed job", "job_id", job.JobID, "chars", len(text))
}

// transcriptNoise asks the decision gateway whether a transcript is a real
// request or unintelligible noise, and reports true only for noise. The label
// lands in the trace either way. Retrying the same clip cannot turn noise into a
// request, so a noise verdict is permanent; a classifier error is not, and the
// transcript passes through with the reason recorded.
//
// It is a forced choice, not a bool. laya answers a bare yes/no with a strong
// "no" bias: asked whether each transcript was a real request, it said no to
// every canonical capture, the PRD's own "Remember that milk is cheaper at store
// A than store B" included. The choice separates real captures from syllable
// noise cleanly.
func (w *Worker) transcriptNoise(ctx context.Context, text string, detail map[string]any) (bool, error) {
	gateCtx, cancel := context.WithTimeout(ctx, gateTimeout)
	defer cancel()
	answers, err := w.Gate.Decide(gateCtx, text, decision.Questions{
		"transcript": {Kind: decision.Choice, Instructions: "Which is this transcript?",
			Criteria: map[string]string{
				requestLabel: "a real request about something to do or remember",
				noiseLabel:   "unintelligible noise that means nothing",
			}},
	})
	if err != nil {
		detail["gate"] = "error"
		detail["gateError"] = truncate(err.Error(), 200)
		return false, err
	}
	verdict, ok := answers["transcript"]
	if !ok {
		err := errors.New("decision: no answer for transcript")
		detail["gate"] = "error"
		detail["gateError"] = err.Error()
		return false, err
	}
	detail["gate"] = "ok"
	detail["gateModel"] = w.GateModel
	detail["label"] = verdict.Choice
	detail["gateProbability"] = verdict.Probabilities[verdict.Choice]
	return verdict.Choice == noiseLabel, nil
}

// fail parks the Job, removes the blob, and sends the best-effort notification
// the PRD asks for.
func (w *Worker) fail(ctx context.Context, job store.Job, audio string, attempt int, outcome string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	if attempt > maxAttempts {
		attempt = maxAttempts
	}
	detail["outcome"] = outcome
	detail["attempt"] = attempt
	if err := w.Jobs.FailTranscription(ctx, job.JobID, attempt, FallbackMessage, detail); err != nil {
		// Leave the row alone; a restart requeues it rather than losing it.
		slog.Error("fail transcription", "job_id", job.JobID, "err", err)
		return
	}
	if err := w.Audio.Remove(audio); err != nil {
		slog.Warn("remove failed audio", "job_id", job.JobID, "err", err)
	}

	if w.Pusher == nil {
		return
	}
	_, err := w.Pusher.NotifyUser(ctx, job.UserID, push.Message{
		Title:    "Blurt couldn't hear that",
		Body:     FallbackMessage,
		Tag:      fmt.Sprintf("job-%d", job.JobID),
		Navigate: "/home",
	})
	if err != nil {
		// Push is best effort; the Job already shows the fallback.
		slog.Warn("send transcription fallback push", "job_id", job.JobID, "err", err)
	}
}

func permanent(err error) bool {
	var refusal *stt.Error
	return errors.As(err, &refusal) && !refusal.Retryable()
}

func outcomeFor(err error) string {
	if permanent(err) {
		return "rejected"
	}
	return "error"
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
