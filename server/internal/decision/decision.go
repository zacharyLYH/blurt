// Package decision is Blurt's typed-decision layer over the self-hosted
// gateway's laya model. It is generic on purpose: the transcript-quality gate
// uses it today, and PostAgentCheck will reuse it later. laya is a classifier,
// not a chat model, so the call is a state plus named questions and the answer
// is one typed value per question.
//
// laya's window is small — observed at 512 tokens, and it truncates silently
// rather than refusing — so the state is capped here. Instructions and criteria
// serialize after the state, so an oversized state would push them out of the
// window instead, which is the one failure that makes every answer meaningless.
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"blurt/internal/config"
)

// Kind is a decision shape the gateway understands. Anything else is refused
// with a 400, so these are the only kinds this layer offers.
type Kind string

const (
	// Bool answers yes or no with a confidence.
	Bool Kind = "bool"
	// Noul is the gateway's accepted alias for Bool; it answers identically and
	// the reply's type is Bool either way.
	Noul Kind = "noul"
	// Choice picks exactly one key from Criteria.
	Choice Kind = "choice"
)

// maxStateChars keeps the whole prompt inside laya's 512-token window with room
// for instructions and criteria. At roughly four characters per English token
// this is ~250 tokens, and a full minute of speech is far under it.
const maxStateChars = 1000

// Question is one typed decision to make about the state.
type Question struct {
	Kind         Kind              `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// Questions is the set to decide, keyed by the caller's own id.
type Questions map[string]Question

// Answer is the classifier's verdict for one Question. Kind says which value is
// set: Choice for a choice, Truth (with Probability) for a bool.
type Answer struct {
	Kind          Kind
	Choice        string
	Truth         bool
	Probability   float64
	Probabilities map[string]float64
}

// Decisions is one Answer per question id.
type Decisions map[string]Answer

// Decider makes typed decisions about a state. An interface so the gate's
// policy is testable without the gateway.
type Decider interface {
	Decide(ctx context.Context, state string, questions Questions) (Decisions, error)
}

// Error is an HTTP-level refusal. Status 0 is never used: transport failures
// stay ordinary errors and are retried.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("decision: %d %s", e.Status, e.Message)
}

// Retryable is false for a 4xx that a retry cannot fix (bad key, bad kind).
func (e *Error) Retryable() bool {
	return e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests ||
		e.Status >= http.StatusInternalServerError
}

// HTTP calls the gateway's /v1/infer with the laya model.
type HTTP struct {
	url    string
	apiKey string
	model  string
	client *http.Client
}

func NewHTTP(cfg config.Decision) *HTTP {
	return &HTTP{
		url:    cfg.URL,
		apiKey: cfg.APIKey,
		model:  cfg.Model,
		// laya answers in well under a second on an idle host; the caller's own
		// timeout sits just above this one.
		client: &http.Client{Timeout: 25 * time.Second},
	}
}

// request is the gateway's body: model plus a state and named questions.
type request struct {
	Model   string `json:"model"`
	Payload struct {
		State     string    `json:"state"`
		Questions Questions `json:"questions"`
	} `json:"payload"`
}

// reply needs only the typed answers.
type reply struct {
	Output struct {
		Answers map[string]wireAnswer `json:"answers"`
	} `json:"output"`
}

// wireAnswer is the union the gateway answers with: a choice carries a key and a
// distribution, a bool a value and one probability.
type wireAnswer struct {
	Type          Kind               `json:"type"`
	Value         json.RawMessage    `json:"value"`
	Probability   float64            `json:"probability"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Decide sends one state and its questions. Every question asked comes back with
// an answer, or the call reports why it could not.
func (h *HTTP) Decide(ctx context.Context, state string, questions Questions) (Decisions, error) {
	if len(questions) == 0 {
		return nil, errors.New("decision: no questions")
	}
	for id, question := range questions {
		if question.Kind == Choice && len(question.Criteria) == 0 {
			return nil, fmt.Errorf("decision: choice %q needs criteria", id)
		}
	}

	body := request{Model: h.model}
	body.Payload.State = truncate(state, maxStateChars)
	body.Payload.Questions = questions
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("decision: build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("decision: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.apiKey)
	}

	res, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("decision: call laya: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, refusal(res)
	}

	var decoded reply
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decision: decode answers: %w", err)
	}
	answers := make(Decisions, len(decoded.Output.Answers))
	for id, wire := range decoded.Output.Answers {
		answer, err := wire.typed()
		if err != nil {
			return nil, fmt.Errorf("decision: answer %q: %w", id, err)
		}
		answers[id] = answer
	}
	return answers, nil
}

// typed resolves the union value into the kind the question asked for.
func (w wireAnswer) typed() (Answer, error) {
	switch w.Type {
	case Choice:
		var key string
		if err := json.Unmarshal(w.Value, &key); err != nil {
			return Answer{}, fmt.Errorf("choice value: %w", err)
		}
		return Answer{Kind: Choice, Choice: key, Probabilities: w.Probabilities}, nil
	case Bool, Noul:
		var truth bool
		if err := json.Unmarshal(w.Value, &truth); err != nil {
			return Answer{}, fmt.Errorf("bool value: %w", err)
		}
		return Answer{Kind: Bool, Truth: truth, Probability: w.Probability}, nil
	default:
		return Answer{}, fmt.Errorf("unexpected type %q", w.Type)
	}
}

// refusal turns the gateway's error body into the refusal the caller reasons
// about: {"error": {"code", "message"}}. laya wraps its own detail inside the
// message, so it is passed through as-is.
func refusal(res *http.Response) *Error {
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
	return &Error{Status: res.StatusCode, Message: message}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
