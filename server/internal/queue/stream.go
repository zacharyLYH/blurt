// Package queue routes Jobs between workers with Redis Streams. Redis owns
// routing and delivery; SQL remains the durable Job record.
package queue

import (
	"context"
	"errors"
	"time"
)

// ErrEmpty means a consume found no message.
var ErrEmpty = errors.New("queue empty")

// Config names one stream, its consumer group, and this process's consumer.
type Config struct {
	Stream    string
	Group     string
	Consumer  string
	ClaimerID string
}

// Message is one queued job attempt.
type Message struct {
	JobID   int64
	Attempt int
	ID      string
}

// Stream is the queue one processing stage consumes.
type Stream interface {
	Publish(ctx context.Context, jobID int64, attempt int) error
	Consume(ctx context.Context) (jobID int64, attempt int, msgID string, err error)
	Ack(ctx context.Context, msgID string) error
	Close() error
}

// Claiming is a Stream that can reclaim messages from dead consumers.
type Claiming interface {
	Stream
	Claim(ctx context.Context, minIdle time.Duration) ([]Message, error)
}

// Queues are the two stage streams producers publish to.
type Queues struct {
	TranscriptionQueue Stream
	AgentQueue         Stream
}

// Every calls step immediately and then once per interval until ctx is done.
func Every(ctx context.Context, interval time.Duration, step func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
