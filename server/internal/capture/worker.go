package capture

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"blurt/internal/decision"
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
	// gateTimeout bounds the transcript-quality check so a hung classifier cannot
	// pin a Job; it sits just above the decision client's own timeout.
	gateTimeout = 30 * time.Second
	defaultPoll = time.Second
	// FallbackMessage is the exact copy the PRD wants in the Job Conversation
	// when the clip cannot be understood.
	FallbackMessage = failure.FallbackMessage
	// requestLabel and noiseLabel are the criteria the gate's transcript question
	// offers; laya answers with one of the two.
	requestLabel = "request"
	noiseLabel   = "noise"
)

// Notifier is the push fan-out the worker uses after a permanent failure.
type Notifier interface {
	NotifyUser(ctx context.Context, userID int64, msg push.Message) (push.Result, error)
}

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
	// Gate is the transcript-quality filter. Nil is valid: with no decision
	// gateway configured the check is skipped entirely.
	Gate      decision.Decider
	GateModel string
	Pusher    Notifier
	Model     string
	Poll      time.Duration
	// AttemptTimeout bounds one STT call; zero means attemptTimeout.
	AttemptTimeout time.Duration
	// GateTimeout bounds the transcript-quality check; zero means gateTimeout.
	GateTimeout time.Duration
}

// Run blocks until ctx is cancelled.
func (w *TranscriptionWorker) Run(ctx context.Context) {
	if w.Poll <= 0 {
		w.Poll = defaultPoll
	}
	queue.Every(ctx, w.Poll, func(ctx context.Context) { w.processOne(ctx) })
}

// sttTimeout is the STT call's own wall; a slow provider trips it and the Job
// retries rather than pinning the worker.
func (w *TranscriptionWorker) sttTimeout() time.Duration {
	if w.AttemptTimeout > 0 {
		return w.AttemptTimeout
	}
	return attemptTimeout
}

// gateDeadline bounds the transcript-quality check; it sits just above the
// decision client's own timeout so a hung classifier cannot pin a Job.
func (w *TranscriptionWorker) gateDeadline() time.Duration {
	if w.GateTimeout > 0 {
		return w.GateTimeout
	}
	return gateTimeout
}

func (w *TranscriptionWorker) processOne(ctx context.Context) bool {
	job, attempt, msgID, ok := queue.Next(ctx, w.TranscriptionQueue, w.Jobs)
	if !ok {
		return false
	}
	if w.process(ctx, job, attempt) {
		w.ack(ctx, msgID)
	}
	return true
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
		if err := w.Jobs.Retry(ctx, store.TranscriptionStage, job.JobID, attempt, detail); err != nil {
			slog.Error("retry transcription", "job_id", job.JobID, "err", err)
			return false
		}
		if err := w.TranscriptionQueue.Publish(ctx, job.JobID, attempt+1); err != nil {
			slog.Error("republish transcription", "job_id", job.JobID, "err", err)
			return false
		}
		return true
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return w.fail(ctx, job, audio, attempt, "illegible", detail)
	}
	if w.Gate != nil {
		noise, err := w.transcriptNoise(ctx, text, detail)
		if err != nil {
			// The filter is advisory: a classifier that is down must not block
			// a capture. The trace keeps the reason.
			slog.Warn("transcript gate", "job_id", job.JobID, "err", err)
		} else if noise {
			return w.fail(ctx, job, audio, attempt, "noise", detail)
		}
	}
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

func (w *TranscriptionWorker) ack(ctx context.Context, msgID string) {
	if err := w.TranscriptionQueue.Ack(ctx, msgID); err != nil {
		slog.Error("ack transcription", "msg_id", msgID, "err", err)
	}
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
func (w *TranscriptionWorker) transcriptNoise(ctx context.Context, text string, detail map[string]any) (bool, error) {
	gateCtx, cancel := context.WithTimeout(ctx, w.gateDeadline())
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
		detail["gateError"] = textutil.Truncate(err.Error(), 200)
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
func (w *TranscriptionWorker) fail(ctx context.Context, job store.Job, audio string, attempt int, outcome string, detail map[string]any) bool {
	if detail == nil {
		detail = map[string]any{}
	}
	if attempt > maxAttempts {
		attempt = maxAttempts
	}
	detail["outcome"] = outcome
	detail["attempt"] = attempt
	if err := w.Jobs.Fail(ctx, store.TranscriptionStage, job.JobID, attempt, FallbackMessage, detail); err != nil {
		// Leave the message pending; the claimer retries it rather than losing it.
		slog.Error("fail transcription", "job_id", job.JobID, "err", err)
		return false
	}
	if err := w.Audio.Remove(audio); err != nil {
		slog.Warn("remove failed audio", "job_id", job.JobID, "err", err)
	}

	if w.Pusher != nil {
		_, err := w.Pusher.NotifyUser(ctx, job.UserID, push.Message{
			Title:    "Blurt couldn't hear that",
			Body:     FallbackMessage,
			Tag:      fmt.Sprintf("job-%d", job.JobID),
			Navigate: fmt.Sprintf("/jobs/%d", job.JobID),
		})
		if err != nil {
			// Push is best effort; the Job already shows the fallback.
			slog.Warn("send transcription fallback push", "job_id", job.JobID, "err", err)
		}
	}
	return true
}
