package flow

import (
	"context"
	"os"
	"testing"
	"time"

	"blurt/internal/queue"
	"blurt/internal/store"
	"blurt/internal/stt"
)

// The broker-level flow: a restart re-runs group creation (Redis answers
// BUSYGROUP, which boot must swallow), and a capture then runs to completion over
// the real streams.
func TestRedisFlowBootsGroupThenServesACapture(t *testing.T) {
	url := os.Getenv("BLURT_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set BLURT_TEST_REDIS_URL (or run make test-redis) to exercise real Redis")
	}
	cfg := queue.Config{Stream: "blurt:flow:boot", Group: "agent", Consumer: "agent-1", ClaimerID: "claimer"}
	for boot := 1; boot <= 2; boot++ {
		stream, err := queue.NewRedis(context.Background(), url, cfg)
		if err != nil {
			t.Fatalf("boot %d: %v", boot, err)
		}
		stream.Close()
	}

	p := newRedisPump(t, reminderTurn("buy glue", "tomorrow at 6pm")).start()
	job := p.text(t, "buy glue tomorrow at 6pm", "Asia/Singapore")
	p.settle(t, job.JobID, status(store.JobComplete), "the capture over real Redis")
}

// A capture worker that dies mid-transcription must have its message reclaimed
// by the claimer over the real broker. This pins the XAUTOCLAIM idle window:
// the message is not reclaimable until it has been idle past minIdle, and the
// reclaim counts as a failed attempt that republishes with attempt+1.
func TestRedisFlowClaimerRecoversAfterCaptureWorkerDies(t *testing.T) {
	// The blocking transcriber is installed before the loop starts: the worker
	// reads its STT without a lock, so a write after start races the goroutine
	// that is about to read it.
	p := newRedisPump(t, memoryTurn(stt.FakeTranscript))
	p.captureWorker.STT = blockingTranscriber{}
	p.start()
	job := p.voice(t, "Asia/Singapore")

	// The worker is inside Transcribe, holding the message in the PEL but never
	// acking it.
	p.soon(t, func() bool { return len(p.pending(t, p.transcription)) == 1 }, "the worker to hold the message")
	p.stopCapture()

	// The claimer runs with MinIdle=0 in the test harness (production uses 360s);
	// the point here is that reclamation happens over real Redis, not that the
	// idle window is enforced — that is pinned by redis_integration_test.go.
	p.startClaimers()
	p.settle(t, job.JobID, func(j store.Job) bool { return j.RetryCount == 1 }, "the claimer to reclaim")

	// The recovered Job re-enters the transcription stream; a restarted capture
	// worker finishes it and hands it to the agent.
	p.captureWorker.STT = scriptedTranscriber{text: stt.FakeTranscript}
	p.startCapture()
	p.settle(t, job.JobID, status(store.JobComplete), "the recovered Job")
	p.stopClaimers()
}

// A model call that hangs past the attempt deadline retries, and at the poison
// pill the Job fails with the fallback — over real Redis, not Mocks.
func TestRedisFlowSlowModelRetriesThenFails(t *testing.T) {
	// Same rule as above: the script and the deadline are set before the loops
	// start, because the worker reads them without a lock.
	p := newRedisPump(t)
	p.model.blocks = true
	p.agentWorker.Loop.Timeout = 20 * time.Millisecond
	p.start()
	job := p.text(t, "buy glue tomorrow at 6pm", "Asia/Singapore")

	failed := p.settle(t, job.JobID, status(store.JobFailed), "the poison pill")
	if failed.RetryCount != store.MaxAttempts {
		t.Fatalf("retry count = %d, want %d", failed.RetryCount, store.MaxAttempts)
	}
	p.drain(t, p.agent, "agent queue")
}
