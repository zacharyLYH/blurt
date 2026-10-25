package queue

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// Mock is an in-memory Stream for unit tests.
type Mock struct {
	mu        sync.Mutex
	inbox     []Message
	delivered []Message
	acked     map[string]bool
	next      int
}

func NewMock() *Mock {
	return &Mock{acked: map[string]bool{}}
}

func (m *Mock) Publish(_ context.Context, jobID int64, attempt int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	m.inbox = append(m.inbox, Message{JobID: jobID, Attempt: attempt, ID: strconv.Itoa(m.next)})
	return nil
}

func (m *Mock) Consume(ctx context.Context) (int64, int, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.inbox) == 0 {
		if err := ctx.Err(); err != nil {
			return 0, 0, "", err
		}
		return 0, 0, "", ErrEmpty
	}
	message := m.inbox[0]
	m.inbox = m.inbox[1:]
	m.delivered = append(m.delivered, message)
	return message.JobID, message.Attempt, message.ID, nil
}

func (m *Mock) Claim(_ context.Context, _ time.Duration) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var messages []Message
	for _, message := range m.delivered {
		if !m.acked[message.ID] {
			messages = append(messages, message)
		}
	}
	return messages, nil
}

func (m *Mock) Ack(_ context.Context, msgID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acked[msgID] = true
	return nil
}

func (m *Mock) Close() error { return nil }
