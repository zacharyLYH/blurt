package queue

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"blurt/internal/store"
)

// The Redis tests are opt-in: the PEL, XAUTOCLAIM, XDEL and BUSYGROUP behaviour
// they pin cannot be faked faithfully. `make test-redis` starts the throwaway
// server and sets BLURT_TEST_REDIS_URL; without it these skip.
func redisURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("BLURT_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set BLURT_TEST_REDIS_URL (or run make test-redis) to exercise real Redis")
	}
	return url
}

// openRedis hands back a Stream on an empty key, so a reused test Redis repeats
// cleanly.
func openRedis(t *testing.T, cfg Config) *Redis {
	t.Helper()
	url := redisURL(t)
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	client := redis.NewClient(options)
	if err := client.Del(context.Background(), cfg.Stream).Err(); err != nil {
		client.Close()
		t.Fatalf("flush stream: %v", err)
	}
	client.Close()

	stream, err := NewRedis(context.Background(), url, cfg)
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	t.Cleanup(func() { stream.Close() })
	return stream
}

// A restart re-runs group creation; Redis answers BUSYGROUP and boot must not
// fail on it.
func TestRedisNewRedisIsIdempotentAcrossRestarts(t *testing.T) {
	url := redisURL(t)
	cfg := Config{Stream: "blurt:test:boot", Group: "capture", Consumer: "c1", ClaimerID: "claimer"}
	first, err := NewRedis(context.Background(), url, cfg)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	defer first.Close()
	second, err := NewRedis(context.Background(), url, cfg)
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	second.Close()
}

// The visibility window is what tells a slow worker from a dead one: a message
// is reclaimable only once it has been idle past minIdle.
func TestRedisClaimHonoursTheIdleWindow(t *testing.T) {
	stream := openRedis(t, Config{Stream: "blurt:test:reclaim", Group: "agent", Consumer: "worker", ClaimerID: "claimer"})
	ctx := context.Background()
	if err := stream.Publish(ctx, 7, 1); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, _, _, err := stream.Consume(ctx); err != nil {
		t.Fatalf("consume: %v", err)
	}

	if messages, err := stream.Claim(ctx, time.Hour); err != nil || len(messages) != 0 {
		t.Fatalf("claim inside the window = %+v, %v, want none", messages, err)
	}
	messages, err := stream.Claim(ctx, 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(messages) != 1 || messages[0].JobID != 7 || messages[0].Attempt != 1 {
		t.Fatalf("claimed = %+v, want job 7 attempt 1", messages)
	}
}

// Acking removes the entry from both the PEL and the stream, so a later claim
// has nothing to resurrect and the stream does not accumulate.
func TestRedisAckDropsTheEntry(t *testing.T) {
	stream := openRedis(t, Config{Stream: "blurt:test:ack", Group: "agent", Consumer: "worker", ClaimerID: "claimer"})
	ctx := context.Background()
	if err := stream.Publish(ctx, 7, 1); err != nil {
		t.Fatalf("publish: %v", err)
	}
	_, _, msgID, err := stream.Consume(ctx)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := stream.Ack(ctx, msgID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	if messages, err := stream.Claim(ctx, 0); err != nil || len(messages) != 0 {
		t.Fatalf("claim after ack = %+v, %v, want none", messages, err)
	}
	if n, err := stream.client.XLen(ctx, stream.cfg.Stream).Result(); err != nil || n != 0 {
		t.Fatalf("stream length = %d, %v, want 0: ack deletes the entry", n, err)
	}
}

// The sweeper's whole job is republishing rows whose XADD never landed; this
// pins that the publish reaches a real stream.
func TestRedisSweeperRepublishesToTheRealStream(t *testing.T) {
	jobs, handle, userID := newStack(t)
	ctx := context.Background()
	job, err := jobs.Create(ctx, userID, store.Message{Role: "user", Text: "buy glue"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, err := handle.Write.Exec(`UPDATE Job SET queued_at = ? WHERE JobId = ?`, old, job.JobID); err != nil {
		t.Fatalf("age job: %v", err)
	}

	transcription := openRedis(t, Config{Stream: "blurt:test:sweep:tx", Group: "capture", Consumer: "capture", ClaimerID: "claimer"})
	agent := openRedis(t, Config{Stream: "blurt:test:sweep", Group: "agent", Consumer: "worker", ClaimerID: "claimer"})
	sweeper := &Sweeper{Jobs: jobs, TranscriptionQueue: transcription, AgentQueue: agent}
	sweeper.sweep(ctx)

	id, attempt, _, err := agent.Consume(ctx)
	if err != nil || id != job.JobID || attempt != 1 {
		t.Fatalf("swept message = %d/%d, %v, want %d/1", id, attempt, err, job.JobID)
	}
}
