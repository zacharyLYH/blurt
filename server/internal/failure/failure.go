// Package failure classifies provider errors and carries what both processing stages
// do when a capture or plan cannot be processed at all: the sentence the user reads,
// and the one give-up path that records it.
package failure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"blurt/internal/push"
	"blurt/internal/store"
)

// FallbackMessage is what the user reads when the work could not be done.
const FallbackMessage = "I couldn't process that. Please say it again or type it."

// retryable is the shape the provider errors share without sharing a type.
type retryable interface {
	Retryable() bool
}

// Permanent reports whether a retry cannot fix the error: a bad key, an unknown
// model, or another refusal the next attempt would repeat.
func Permanent(err error) bool {
	var refusal retryable
	return errors.As(err, &refusal) && !refusal.Retryable()
}

// Refusal is an HTTP-level refusal from a provider. Status 0 is never used:
// transport failures stay ordinary errors and are retried. Service names the
// caller in the error text, e.g. "stt".
type Refusal struct {
	Service string
	Status  int
	Message string
}

func (e *Refusal) Error() string {
	return fmt.Sprintf("%s: %d %s", e.Service, e.Status, e.Message)
}

// Retryable is false for a 4xx a retry cannot fix (bad key, unknown model, no
// credits).
func (e *Refusal) Retryable() bool {
	return e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests ||
		e.Status >= http.StatusInternalServerError
}

// ReadRefusal turns a provider's non-2xx body into the refusal the caller reasons
// about: {"error": {"code", "message"}}. A provider that answers with something
// else still yields the status, which is what Retryable actually needs.
func ReadRefusal(service string, res *http.Response) *Refusal {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body)
	message := strings.TrimSpace(body.Error.Message)
	if message == "" {
		message = strings.TrimSpace(body.Error.Code)
	}
	return &Refusal{Service: service, Status: res.StatusCode, Message: message}
}

// Outcome names a failure in the Trace.
func Outcome(err error) string {
	if Permanent(err) {
		return "rejected"
	}
	return "error"
}

// Jobs is the durable half of a give-up.
type Jobs interface {
	Fail(ctx context.Context, stage string, jobID int64, attempt int, fallback string, detail map[string]any) error
}

// GiveUp is one abandoned attempt: where it happened, what it cost, what the Trace
// should say, and the headline of the push the user gets.
type GiveUp struct {
	Stage   string
	Attempt int
	Outcome string
	Title   string
	Detail  map[string]any
}

// Park ends one Job's attempt for good, the way both stages do it: the attempt is
// capped at the poison-pill ceiling, the outcome lands in the Trace, and the user is
// told FallbackMessage. It reports whether the durable write landed — a failed write
// leaves the message pending for the claimer.
func Park(ctx context.Context, jobs Jobs, pusher push.Notifier, job store.Job, g GiveUp) bool {
	if g.Detail == nil {
		g.Detail = map[string]any{}
	}
	if g.Attempt > store.MaxAttempts {
		g.Attempt = store.MaxAttempts
	}
	g.Detail["outcome"], g.Detail["attempt"] = g.Outcome, g.Attempt
	if err := jobs.Fail(ctx, g.Stage, job.JobID, g.Attempt, FallbackMessage, g.Detail); err != nil {
		slog.Error("fail job", "stage", g.Stage, "job_id", job.JobID, "err", err)
		return false
	}
	Notify(ctx, pusher, job, g.Title, FallbackMessage)
	return true
}

// Notify sends one push about a Job, tagged and routed the one way every message
// about that Job is. Best effort, like every push Blurt sends: the Job already says
// the same thing in its Conversation.
func Notify(ctx context.Context, pusher push.Notifier, job store.Job, title, body string) {
	if pusher == nil {
		return
	}
	_, err := pusher.NotifyUser(ctx, job.UserID, push.Message{
		Title:    title,
		Body:     body,
		Tag:      fmt.Sprintf("job-%d", job.JobID),
		Navigate: fmt.Sprintf("/jobs/%d", job.JobID),
	})
	if err != nil {
		slog.Warn("send job push", "job_id", job.JobID, "err", err)
	}
}
