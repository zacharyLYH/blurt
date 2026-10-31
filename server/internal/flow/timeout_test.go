package flow

import (
	"strings"
	"testing"
	"time"

	"blurt/internal/agent"
	"blurt/internal/capture"
	"blurt/internal/store"
)

// A provider that hangs past the worker's own deadline retries, and at the
// poison pill the Job fails with the fallback and a push, leaving nothing queued.
func TestPipelineSlowTranscriptionRetriesThenFails(t *testing.T) {
	p := newPump(t)
	p.captureWorker.STT = blockingTranscriber{}
	p.captureWorker.AttemptTimeout = 20 * time.Millisecond
	p.start()
	job := p.voice(t, "Asia/Singapore")

	failed := p.settle(t, job.JobID, status(store.JobFailed), "the poison pill")
	if failed.RetryCount != store.MaxAttempts {
		t.Fatalf("retry count = %d, want %d", failed.RetryCount, store.MaxAttempts)
	}
	if last := failed.Conversation[len(failed.Conversation)-1]; last.Role != "assistant" || last.Text != capture.FallbackMessage {
		t.Fatalf("fallback = %+v", last)
	}
	if calls, _, body := p.pusher.sent(); calls != 1 || body != capture.FallbackMessage {
		t.Fatalf("pushes = %d %q, want one fallback", calls, body)
	}
	// Two retries and then the failure, one Trace row each.
	if got := strings.Join(p.outcomes(t, job.JobID, store.TranscriptionStage), ","); got != "retry,retry,error" {
		t.Fatalf("outcomes = %s, want retry,retry,error", got)
	}
	// A clip that never transcribes never reaches the agent: three failed attempts
	// cost the model nothing.
	p.wantAsks(t, 0, 0, 0, 0)
	p.drain(t, p.transcription, "transcription queue")
}

// A model that never answers is bounded by the run's own deadline: it retries,
// and at the poison pill the Job fails with the fallback and a push, leaving no
// attempt in flight.
func TestPipelineSlowModelRetriesThenFails(t *testing.T) {
	p := newPump(t)
	p.model.blocks = true
	p.loop.Timeout = 20 * time.Millisecond
	p.start()
	job := p.text(t, "buy glue tomorrow at 6pm", "Asia/Singapore")

	failed := p.settle(t, job.JobID, status(store.JobFailed), "the poison pill")
	if failed.RetryCount != store.MaxAttempts {
		t.Fatalf("retry count = %d, want %d", failed.RetryCount, store.MaxAttempts)
	}
	if last := failed.Conversation[len(failed.Conversation)-1]; last.Text != agent.FallbackMessage {
		t.Fatalf("fallback = %+v", last)
	}
	if calls, _, body := p.pusher.sent(); calls != 1 || body != agent.FallbackMessage {
		t.Fatalf("pushes = %d %q, want one fallback", calls, body)
	}
	// Two retries and then the failure, one Trace row each.
	if got := strings.Join(p.outcomes(t, job.JobID, store.AgentStage), ","); got != "retry,retry,error" {
		t.Fatalf("outcomes = %s, want retry,retry,error", got)
	}
	// Each attempt screens and then hangs in the planner: three of each, and the
	// checker and tagger were never reached.
	p.wantAsks(t, 3, 3, 0, 0)
	p.drain(t, p.agent, "agent queue")
}
