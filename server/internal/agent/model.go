// The model port: what the AgentLoop asks for, and the gateway chat route that
// answers it. The gateway's own /v1/infer is not OpenAI's — chat messages plus
// a JSON schema go in, and a JSON string comes back under
// output.choices[0].message.content — so the schema, not tool calling, is what
// keeps a small model parseable. Which record to commit is the server's call.
package agent

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
	"blurt/internal/store"
)

// maxOutputTokens caps one answer. A measured reply is forty to sixty tokens; the
// headroom covers a long capture, and the cap is what stops a small model that has
// started repeating itself.
const maxOutputTokens = 200

// Write kinds. A capture can propose a Reminder, a Memory, or both, and each one
// is checked and committed on its own.
const (
	KindReminder = "reminder"
	KindMemory   = "memory"
)

// RecurrenceNone is a one-off: the write names one local date and time. Every
// other value is a store.Rule frequency.
const RecurrenceNone = "none"

// Write is one record the model proposed, in the fields it is confident about.
// Everything else is empty, and MissingFields names what that leaves the user to say.
type Write struct {
	Kind       string
	Text       string
	Zone       string // IANA zone the user meant
	Date       string // ISO date YYYY-MM-DD, empty when unresolved
	Time       string // HH:MM 24-hour, empty when unresolved
	Recurrence string // RecurrenceNone, or a store.Rule frequency
	Evidence   string // the user's own words for the time and the zone
}

// Field names the piece of a write the server could not accept, and so the piece
// the question asks the user about.
type Field string

const (
	FieldEvent      Field = "event"
	FieldZone       Field = "zone"
	FieldDate       Field = "date"
	FieldTime       Field = "time"
	FieldRecurrence Field = "recurrence"
)

// Turn is one model reply: what it proposes to commit.
type Turn struct {
	Writes []Write
}

// Input is everything one model turn sees. Retrieval is gathered by the server
// rather than requested through tools — the model's own tool choices were
// measured unreliable, and the results are context either way, so the loop
// prefetches them and the model still decides what they mean.
type Input struct {
	// Conversation is the Job's turns, oldest first.
	Conversation []store.Message
	// CurrentTZ anchors the zone the model should copy into a write.
	CurrentTZ string
	// Now is the loop's clock, in the zone above: what "tomorrow" and the
	// after-now checks hang off. A relative capture cannot be resolved to a date
	// without it, and a test pins it.
	Now time.Time
	// Nearby and Remembered are what recall found for this capture.
	Nearby     []store.Reminder
	Remembered []store.Memory
	// Missing is what the question writer is being asked to ask about.
	Missing []Field
}

// Model is Blurt's one model, asked three ways: for the records a capture becomes, for the one
// question the user answers when MissingFields cannot finish it, and for the search features of a
// committed Memory.
type Model interface {
	Propose(ctx context.Context, in Input) (Turn, error)
	Question(ctx context.Context, in Input, missing []Field) (string, error)
	Tags(ctx context.Context, text string) ([]string, error)
}

// Error is an HTTP-level refusal from the gateway. Status 0 is never used:
// transport failures stay ordinary errors and are retried.
type Error struct {
	Status  int
	Message string
}

// Gateway calls the gateway's chat route. It shares phonon's and laya's endpoint but
// none of their body: chat messages plus a JSON schema in, and the answer as a
// JSON string under output.choices[0].message.content.
type Gateway struct {
	url    string
	apiKey string
	model  string
	client *http.Client
}

// wireMessage is one chat turn as the gateway takes it.
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// inferRequest is the gateway's body: a model plus a payload.
type inferRequest struct {
	Model   string       `json:"model"`
	Payload inferPayload `json:"payload"`
}

type inferPayload struct {
	Messages       []wireMessage  `json:"messages"`
	MaxTokens      int            `json:"max_tokens"`
	ResponseFormat responseFormat `json:"response_format"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
}

// inferReply mirrors the envelope: a chat completion nested under output, with
// the schema-constrained answer as a JSON string.
type inferReply struct {
	Output struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	} `json:"output"`
}

var _ Model = (*Gateway)(nil)

func (e *Error) Error() string {
	return fmt.Sprintf("agent: %d %s", e.Status, e.Message)
}

// Retryable is false for a 4xx a retry cannot fix (bad key, unknown model).
func (e *Error) Retryable() bool {
	return e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests ||
		e.Status >= http.StatusInternalServerError
}

func NewGateway(cfg config.Agent) *Gateway {
	return &Gateway{
		url:    cfg.URL,
		apiKey: cfg.APIKey,
		model:  cfg.Model,
		// A home-hosted model answers in about a second, but it is a real
		// machine: the worker's attempt timeout sits just above this one, so a
		// slow host reports as a model failure rather than a cancelled turn.
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

// call sends one schema-constrained prompt and answers with the content of the first choice and its
// finish reason. Both prompts Blurt asks for travel this one route, and each reads the content its
// own way.
func (h *Gateway) call(ctx context.Context, messages []wireMessage, name, schema string) (string, string, error) {
	body := inferRequest{Model: h.model}
	body.Payload.Messages = messages
	body.Payload.MaxTokens = maxOutputTokens
	body.Payload.ResponseFormat = responseFormat{
		Type:       "json_schema",
		JSONSchema: jsonSchema{Name: name, Schema: json.RawMessage(schema)},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", "", fmt.Errorf("agent: build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(encoded))
	if err != nil {
		return "", "", fmt.Errorf("agent: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.apiKey)
	}

	res, err := h.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("agent: call model: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", "", refusal(res)
	}

	var decoded inferReply
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&decoded); err != nil {
		return "", "", fmt.Errorf("agent: decode model reply: %w", err)
	}
	if len(decoded.Output.Choices) == 0 {
		return "", "", errors.New("agent: model returned no answer")
	}
	choice := decoded.Output.Choices[0]
	return choice.Message.Content, choice.FinishReason, nil
}

func (h *Gateway) Propose(ctx context.Context, in Input) (Turn, error) {
	content, finish, err := h.call(ctx, messages(systemPrompt, in), "records", recordsSchema)
	if err != nil {
		return Turn{}, err
	}
	var parsed records
	// An answer that is not the schema is refused rather than half-read. It is the very failure the
	// schema exists to prevent, and a retry beats a wrong record, so the finish reason rides along
	// in the error for the Trace.
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return Turn{}, fmt.Errorf("agent: model answer is not the schema (finish %s): %w", finish, err)
	}
	return turnOf(parsed), nil
}

// Question writes the question the user has to answer: the same route and the
// same model as the records turn, with one string to fill instead of six, and the
// fields MissingFields could not accept named in the prompt.
func (h *Gateway) Question(ctx context.Context, in Input, missing []Field) (string, error) {
	in.Missing = missing
	content, finish, err := h.call(ctx, messages(questionPrompt, in), "question", questionSchema)
	if err != nil {
		return "", err
	}
	var parsed question
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return "", fmt.Errorf("agent: question answer is not the schema (finish %s): %w", finish, err)
	}
	return strings.TrimSpace(parsed.Question), nil
}

// Tags names the search features of one fact, so a later capture worded nothing like it can still
// find it. The call carries no conversation and no retrieval — the fact is the whole input — and the
// answer is one string the server splits and normalizes. A Memory is the only record that carries
// them, and they are generated on creation, which is what makes them features rather than text.
func (h *Gateway) Tags(ctx context.Context, text string) ([]string, error) {
	content, finish, err := h.call(ctx, []wireMessage{
		{Role: "system", Content: tagsPrompt},
		{Role: "user", Content: "fact: " + text},
	}, "tags", tagsSchema)
	if err != nil {
		return nil, err
	}
	var parsed tags
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, fmt.Errorf("agent: tags answer is not the schema (finish %s): %w", finish, err)
	}
	return parseTags(parsed.Tags), nil
}

// refusal turns the gateway's error body into the refusal the caller reasons
// about: {"error": {"code", "message"}}.
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
