package flow

import (
	"strings"
	"testing"
	"time"

	"blurt/internal/agent"
	"blurt/internal/capture"
	"blurt/internal/store"
	"blurt/internal/stt"
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
	p.drain(t, p.transcription, "transcription queue")
}

// The transcript gate is advisory: a classifier that hangs past its own deadline
// must not stop a capture. The transcript is still saved, and the reason lands
// in the Trace.
func TestPipelineSlowClassifierStillSavesTheTranscript(t *testing.T) {
	// A Memory needs no clock, so the capture can commit on the transcript the
	// default fake returns even though that transcript names no time.
	p := newPump(t, agent.Turn{Say: "Noted.", Writes: []agent.Write{{
		Kind: agent.KindMemory, Text: "buy glue"}}})
	p.captureWorker.Gate = blockingDecider{}
	p.captureWorker.GateTimeout = 20 * time.Millisecond
	p.start()
	job := p.voice(t, "Asia/Singapore")

	done := p.settle(t, job.JobID, status(store.JobComplete), "the capture to survive the slow classifier")
	if first := done.Conversation[0]; first.Text != stt.FakeTranscript {
		t.Fatalf("clip turn = %+v, want the transcript saved", first)
	}
	saved := false
	for _, detail := range p.traces(t, job.JobID, store.TranscriptionStage) {
		if detail["gate"] == "error" && detail["outcome"] == "ok" {
			saved = true
		}
	}
	if !saved {
		t.Fatal("no trace recorded the gate error beside a saved transcript")
	}
}

// A model that never answers is bounded by the run's own deadline: it retries,
// and at the poison pill the Job fails with the fallback and a push, leaving no
// attempt in flight.
func TestPipelineSlowModelRetriesThenFails(t *testing.T) {
	p := newPump(t)
	p.model.blocks = true
	p.agentWorker.Loop.Timeout = 20 * time.Millisecond
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
	p.drain(t, p.agent, "agent queue")
}
