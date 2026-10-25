package flow

import (
	"context"
	"testing"
	"time"

	"blurt/internal/queue"
	"blurt/internal/store"
)

// A capture worker killed mid-transcription leaves its message pending, and the
// claimer hands it back: the Job is retried, not lost. The unit tests pin the
// claimer against a Mock; here both stages, the claimer and the restart are real.
func TestPipelineClaimerRecoversAfterCaptureWorkerDiesMidTranscription(t *testing.T) {
	// The clip commits once it carries a resolvable time.
	p := newPump(t, reminderTurn("buy glue", "tomorrow at 6pm"))
	p.captureWorker.STT = blockingTranscriber{}
	p.start()
	job := p.voice(t, "Asia/Singapore")

	// The worker is inside Transcribe, holding the message and never acking it.
	p.soon(t, func() bool { return len(p.pending(t, p.transcription)) == 1 }, "the worker to hold the message")
	p.stopCapture()
	// A shutdown is not the Job's fault, so nothing was acknowledged.
	held := p.pending(t, p.transcription)
	if len(held) != 1 || held[0].JobID != job.JobID {
		t.Fatalf("pending = %+v, want the held message", held)
	}

	p.startClaimers()
	// A reclaim counts as a failed attempt and republishes, so the Job comes back
	// with a fresh message for the next worker.
	retried := p.settle(t, job.JobID, func(j store.Job) bool { return j.RetryCount == 1 }, "the reclaim")
	if retried.Status != store.JobOpen {
		t.Fatalf("status = %q, want the Job back in the queue", retried.Status)
	}

	p.captureWorker.STT = scriptedTranscriber{text: "buy glue tomorrow at 6pm"}
	p.startCapture()
	p.settle(t, job.JobID, status(store.JobComplete), "the recovered Job")
	p.stopClaimers()
}

// The same recovery on the other stage: a worker killed mid-model-call leaves a
// pending message the agent claimer reclaims.
func TestPipelineClaimerRecoversAfterAgentWorkerDiesMidModel(t *testing.T) {
	p := newPump(t, reminderTurn("buy glue", "tomorrow at 6pm"))
	p.model.blocks = true
	p.start()
	job := p.text(t, "buy glue tomorrow at 6pm", "Asia/Singapore")

	p.soon(t, func() bool { return len(p.pending(t, p.agent)) == 1 }, "the model to hold the message")
	p.stopAgent()
	if held := p.pending(t, p.agent); len(held) != 1 || held[0].JobID != job.JobID {
		t.Fatalf("pending = %+v, want the held message", held)
	}

	p.startClaimers()
	retried := p.settle(t, job.JobID, func(j store.Job) bool { return j.RetryCount == 1 }, "the reclaim")
	if retried.Status != store.JobOpen {
		t.Fatalf("status = %q, want the Job back in the queue", retried.Status)
	}

	p.model.blocks = false
	p.startAgent()
	p.settle(t, job.JobID, status(store.JobComplete), "the recovered Job")
	p.stopClaimers()
}

// A Job that committed to SQL but never reached Redis is stranded; the sweeper
// republishes each to the stage its shape calls for, and the workers finish it.
func TestPipelineSweeperRepublishesStrandedJobs(t *testing.T) {
	p := newPump(t).
		answer("buy glue tomorrow at 6pm", reminderTurn("buy glue", "tomorrow at 6pm"))
	p.captureWorker.STT = scriptedTranscriber{text: "buy glue tomorrow at 6pm"}
	p.start()

	// Nothing is published for either Job: only the sweeper can find them.
	typed := p.strand(t, store.Message{Role: "user", Text: "buy glue tomorrow at 6pm", TZ: "Asia/Singapore"})
	voiced := p.strand(t, store.Message{Role: "user", Audio: p.clip(t), TZ: "Asia/Singapore"})
	for _, job := range []store.Job{typed, voiced} {
		p.age(t, job.JobID, time.Now().Add(-time.Hour))
	}

	go (&queue.Sweeper{Jobs: p.jobs, TranscriptionQueue: p.transcription, AgentQueue: p.agent}).Run(p.ensureRunCtx())

	p.settle(t, typed.JobID, status(store.JobComplete), "the swept typed Job")
	// The voiced Job can only complete through transcription, so settling on it
	// is what proves its routing to that stream.
	p.settle(t, voiced.JobID, status(store.JobComplete), "the swept voiced Job")
}

// A message for a Job that has already settled is consumed and acknowledged,
// never planned again. The queue is FIFO, so a live Job behind the stale message
// proves the stale one was passed over rather than left pending.
func TestPipelineRunSkipsAMessageForASettledJob(t *testing.T) {
	p := newPump(t).
		answer("buy glue tomorrow at 6pm", reminderTurn("buy glue", "tomorrow at 6pm")).
		answer("call mum tomorrow at 7pm", reminderTurn("call mum", "tomorrow at 7pm"))
	p.start()

	settled := p.text(t, "buy glue tomorrow at 6pm", "Asia/Singapore")
	settled = p.settle(t, settled.JobID, status(store.JobComplete), "the completed Job")
	traces := len(p.traces(t, settled.JobID, store.AgentStage))

	if err := p.agent.Publish(context.Background(), settled.JobID, 1); err != nil {
		t.Fatalf("publish stale: %v", err)
	}
	live := p.text(t, "call mum tomorrow at 7pm", "Asia/Singapore")
	p.settle(t, live.JobID, status(store.JobComplete), "the Job behind the stale message")

	if got := p.load(t, settled.JobID); got.Status != store.JobComplete || len(got.Conversation) != len(settled.Conversation) {
		t.Fatalf("settled job = %+v, want it untouched", got)
	}
	if got := len(p.traces(t, settled.JobID, store.AgentStage)); got != traces {
		t.Fatalf("agent traces = %d, want %d: the stale message was planned", got, traces)
	}
	if got := p.pending(t, p.agent); len(got) != 0 {
		t.Fatalf("pending = %+v, want the stale message acknowledged", got)
	}
}
