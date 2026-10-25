package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is a Redis Streams Stream.
type Redis struct {
	client *redis.Client
	cfg    Config
}

// NewRedis connects and creates the consumer group before any message is added.
func NewRedis(ctx context.Context, url string, cfg Config) (*Redis, error) {
	options, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	if err := client.XGroupCreateMkStream(ctx, cfg.Stream, cfg.Group, "$").Err(); err != nil &&
		!strings.Contains(err.Error(), "BUSYGROUP") {
		client.Close()
		return nil, fmt.Errorf("create group %s: %w", cfg.Group, err)
	}
	return &Redis{client: client, cfg: cfg}, nil
}

func (r *Redis) Publish(ctx context.Context, jobID int64, attempt int) error {
	return r.client.XAdd(ctx, &redis.XAddArgs{
		Stream: r.cfg.Stream,
		Values: map[string]any{"jobId": jobID, "attempt": attempt},
	}).Err()
}

func (r *Redis) Consume(ctx context.Context) (int64, int, string, error) {
	read, err := r.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: r.cfg.Group, Consumer: r.cfg.Consumer,
		Streams: []string{r.cfg.Stream, ">"}, Count: 1, Block: time.Second,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return 0, 0, "", ErrEmpty
	}
	if err != nil {
		return 0, 0, "", err
	}
	if len(read) == 0 || len(read[0].Messages) == 0 {
		return 0, 0, "", ErrEmpty
	}
	message := read[0].Messages[0]
	jobID, attempt, err := fields(message)
	return jobID, attempt, message.ID, err
}

func (r *Redis) Claim(ctx context.Context, minIdle time.Duration) ([]Message, error) {
	read, _, err := r.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: r.cfg.Stream, Group: r.cfg.Group, Consumer: r.cfg.ClaimerID,
		MinIdle: minIdle, Start: "0-0", Count: 10,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	messages := make([]Message, 0, len(read))
	for _, raw := range read {
		jobID, attempt, err := fields(raw)
		if err != nil {
			return nil, err
		}
		messages = append(messages, Message{JobID: jobID, Attempt: attempt, ID: raw.ID})
	}
	return messages, nil
}

func (r *Redis) Ack(ctx context.Context, msgID string) error {
	return errors.Join(
		r.client.XAck(ctx, r.cfg.Stream, r.cfg.Group, msgID).Err(),
		r.client.XDel(ctx, r.cfg.Stream, msgID).Err(),
	)
}

func (r *Redis) Close() error {
	return r.client.Close()
}

func fields(message redis.XMessage) (int64, int, error) {
	jobID, err := strconv.ParseInt(fmt.Sprint(message.Values["jobId"]), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse job id: %w", err)
	}
	attempt, err := strconv.Atoi(fmt.Sprint(message.Values["attempt"]))
	if err != nil {
		return 0, 0, fmt.Errorf("parse attempt: %w", err)
	}
	return jobID, attempt, nil
}
