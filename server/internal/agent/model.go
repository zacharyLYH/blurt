// The model port: what the AgentLoop asks for, and the two chat envelopes that answer it. A
// user brings one endpoint, one key and one model, so the transport is built per Job from the
// User row and every stage rides it: the screen, the agent loop, the strict call behind it, the
// check and the tagger.
//
// Two wire shapes, differing only in framing. The home gateway takes
// {"model", "payload": {messages, tools, max_tokens, response_format}} and answers under
// output.choices[0].message; an OpenAI-compatible provider takes messages at the top, answers
// under choices[0].message, and needs `strict` on the schema plus require_parameters so the
// router keeps a turn on an endpoint that can honour it. A schema-constrained turn and a tool
// turn are mutually exclusive: a forced schema can never ask to look something up, and a turn
// that offers tools names no schema.
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

	"blurt/internal/failure"
	"blurt/internal/store"
)

// maxOutputTokens caps one answer. A records answer is forty to sixty tokens, but a
// hosted reasoning model spends output tokens thinking before it answers, and the cap
// is what stops a model that has started repeating itself. It is headroom, not a
// target.
const maxOutputTokens = 4096

// callAttempts is one stage's patience with one provider. A free tier is busy often
// and drops packets occasionally; a 429 or an empty answer is worth waiting out, while
// a bad key or an unknown model is an answer about the request itself and returns at
// once. The backoff is capped by the caller's own deadline.
const callAttempts = 4

// Record kinds. A capture can propose a Reminder, a Memory, or both, and each one
// is checked and committed on its own.
const (
	KindReminder = "reminder"
	KindMemory   = "memory"
)

// The two kinds of planner answer. The model names which one it is; the server never
// infers it from an empty field.
const (
	AnswerRecords  = "records"
	AnswerQuestion = "question"
)

// RecurrenceNone is a one-off: the record names one local date and time. Every
// other value is a store.Rule frequency.
const RecurrenceNone = "none"

// Record is one row the model proposed, in the fields it is confident about.
// Everything else is empty, and MissingFields names what that leaves the user to say.
type Record struct {
	Kind       string
	Text       string
	Zone       string // IANA zone the user meant
	Day        string // the day token the model copied, kept for the Trace
	Date       string // ISO date YYYY-MM-DD, empty when unresolved
	Time       string // HH:MM 24-hour, empty when unresolved
	Recurrence string // RecurrenceNone, or a store.Rule frequency
	Evidence   string // the user's own words for the time and the zone
}

// Field names the piece of a record the server could not accept, and so the piece
// the question asks the user about.
type Field string

const (
	FieldEvent      Field = "event"
	FieldZone       Field = "zone"
	FieldDate       Field = "date"
	FieldTime       Field = "time"
	FieldRecurrence Field = "recurrence"
)

// Answer is one planner answer: either the records it proposes to commit, or the one
// question it asks instead. Never both — the loop drops record fields on an ask.
type Answer struct {
	Kind     string
	Records  []Record
	Question string
}

// PlannerInput is everything one planner turn is built from: the conversation the agent
// reads, the anchor it resolves relative times against, and the one thing the server
// prefetches rather than looks up — the memories a capture might lean on. Reminders are
// the model's own lookup, because the server cannot know which one a capture is about.
type PlannerInput struct {
	// Conversation is the Job's turns, oldest first.
	Conversation []store.Message
	// CurrentTZ anchors the zone the model should copy into a record.
	CurrentTZ string
	// Now is the loop's clock, in the zone above: what "tomorrow" and the
	// after-now checks hang off. A relative capture cannot be resolved to a date
	// without it, and a test pins it.
	Now time.Time
	// Remembered is what retrieval found for this capture.
	Remembered []store.Memory
}

// Verdict is what a screening or checking turn answers. Outcome is the enum the
// stage's schema names; Reason is the one-line diagnostic that rides the Trace.
// Ask is the checker's user-facing sentence when it rejects a row, and is empty
// for a screen.
type Verdict struct {
	Outcome string
	Reason  string
	Ask     string
}

// Proposal is one postagentcheck: the user's own words beside the rows the commit
// would record. Reminder is nil for a fact-only turn, and Fact is empty for a
// turn that wrote no Memory — the checker sees exactly what would land.
type Proposal struct {
	Evidence string
	Reminder *store.Reminder
	Fact     string
}

// Model is Blurt's model, asked five ways: to screen a capture, to work one free-form
// agent turn with the tools on offer, to answer the strict schema that ends the turn, to
// check a proposed record against the user's words, and to name the search features of a
// committed Memory.
type Model interface {
	Prescreen(ctx context.Context, capture string) (Verdict, error)
	Chat(ctx context.Context, in PlannerInput, sent []Message, tools []Tool) (Message, error)
	Finalize(ctx context.Context, in PlannerInput, sent []Message) (Answer, error)
	Verify(ctx context.Context, c Proposal) (Verdict, error)
	Tags(ctx context.Context, text string) ([]string, error)
}

// Provider selects the chat envelope, not the vendor: every supported provider is
// an OpenAI-shaped chat route except the home gateway, which has its own body.
type Provider string

const (
	ProviderGateway    Provider = "gateway"
	ProviderOpenRouter Provider = "openrouter"
)

// Settings is one user's model: what the setup page stores on the User row, with
// the key already decrypted.
type Settings struct {
	Provider Provider
	URL      string
	APIKey   string
	Model    string
}

// ModelClient calls one user's chat route. Every stage travels this one method; the
// stage differs in the prompt and schema it names.
type ModelClient struct {
	settings Settings
	client   *http.Client
	// Prompt overrides the agent's system prompt when it is set. It exists so the eval can
	// A/B a prompt against the same model and nothing in the app sets it.
	Prompt string
}

// prompt is the system prompt the agent loop sends.
func (h *ModelClient) prompt() string {
	if h.Prompt != "" {
		return h.Prompt
	}
	return AgentPrompt
}

// Message is one turn of a conversation as the provider takes it: the role, the words,
// the tool calls the model asked for, and — on a tool result — the call it answers.
// Every stage sends this one shape; only the agent loop fills the tool fields.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool is one callable the agent may use, in the OpenAI tool shape: a type and a
// function. The function's parameters are a JSON schema, which is the shape the server
// parses the model's arguments as — never a shape it trusts them to be.
type Tool struct {
	Type     string    `json:"type"`
	Function ToolShape `json:"function"`
}

// ToolShape is one tool's declaration: what it is called, what it answers, and the
// arguments it takes.
type ToolShape struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is one call the model asked for: the id its result is paired with, and the
// function it named with the arguments it wrote as a JSON string.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function CallFunction `json:"function"`
}

// CallFunction is the function inside one call.
type CallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// functionType is the one tool type the OpenAI shape defines, so every declaration names
// it and nothing else.
const functionType = "function"

// openRouterRequest is the OpenAI-compatible body: messages, the tools on offer and the
// cap at the top, a strict schema when the turn names one, and the two knobs that keep a
// hosted router honest.
type openRouterRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Tools          []Tool          `json:"tools,omitempty"`
	MaxTokens      int             `json:"max_tokens"`
	Reasoning      *reasoning      `json:"reasoning,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Provider       *providerPrefs  `json:"provider,omitempty"`
}

// reasoning is the hosted provider's own switch. A model that does not reason
// ignores it; a free reasoning model needs the headroom in maxOutputTokens.
type reasoning struct {
	Enabled bool `json:"enabled"`
}

// providerPrefs keeps the router on endpoints that honour structured outputs.
// Without it an answer can come back from an endpoint that dropped the schema.
type providerPrefs struct {
	RequireParameters bool `json:"require_parameters"`
}

// gatewayRequest is the home gateway's body: a model plus a payload.
type gatewayRequest struct {
	Model   string       `json:"model"`
	Payload gatewayInput `json:"payload"`
}

type gatewayInput struct {
	Messages       []Message       `json:"messages"`
	Tools          []Tool          `json:"tools,omitempty"`
	MaxTokens      int             `json:"max_tokens"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema"`
}

// replyChoice is one answer, whichever envelope carried it: its finish reason and the
// message itself, tool calls included.
type replyChoice struct {
	FinishReason string  `json:"finish_reason"`
	Message      Message `json:"message"`
}

// inferReply mirrors both envelopes: an OpenAI-compatible reply puts choices at
// the top, and the home gateway nests them under output.
type inferReply struct {
	Choices []replyChoice `json:"choices"`
	Output  struct {
		Choices []replyChoice `json:"choices"`
	} `json:"output"`
}

func (r inferReply) first() (replyChoice, bool) {
	if len(r.Choices) > 0 {
		return r.Choices[0], true
	}
	if len(r.Output.Choices) > 0 {
		return r.Output.Choices[0], true
	}
	return replyChoice{}, false
}

var _ Model = (*ModelClient)(nil)

// NewModelClient builds the transport for one user's settings. Only ProviderOpenRouter
// is not the gateway envelope, so an unset provider takes the gateway path anyway.
func NewModelClient(settings Settings) *ModelClient {
	return &ModelClient{
		settings: settings,
		// The model is hosted now: a free tier answers in tens of seconds, and a
		// reasoning model can spend a minute before its first token. The loop's
		// own attempt deadline sits just above this, so a slow provider reports as
		// a model failure rather than a cancelled turn.
		client: &http.Client{Timeout: 300 * time.Second},
	}
}

// turn is one request's shape: what the answer is asked as (the schema name and its
// JSON) and the tools on offer. A tool turn names no schema, exactly as a schema turn
// offers no tools: a forced schema can never ask to look something up.
type turn struct {
	name   string
	schema string
	tools  []Tool
}

// call sends one prompt and answers with the first choice's message and its finish
// reason. Every stage travels this one route and reads the message its own way. A busy
// endpoint and an answer with nothing in it are retried inside the attempt; anything a
// retry cannot fix is returned at once.
func (h *ModelClient) call(ctx context.Context, messages []Message, t turn) (Message, string, error) {
	var last error
	for attempt := 0; attempt < callAttempts; attempt++ {
		message, finish, err := h.attempt(ctx, messages, t)
		if err == nil {
			return message, finish, nil
		}
		last = err
		if failure.Permanent(err) {
			return Message{}, "", err
		}
		wait := time.Duration(attempt+1) * 3 * time.Second
		select {
		case <-ctx.Done():
			return Message{}, "", ctx.Err()
		case <-time.After(wait):
		}
	}
	return Message{}, "", last
}

// attempt is one request, with the body, the headers and the envelope the provider
// takes.
func (h *ModelClient) attempt(ctx context.Context, messages []Message, t turn) (Message, string, error) {
	encoded, err := json.Marshal(h.body(messages, t))
	if err != nil {
		return Message{}, "", fmt.Errorf("agent: build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.settings.URL, bytes.NewReader(encoded))
	if err != nil {
		return Message{}, "", fmt.Errorf("agent: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.settings.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.settings.APIKey)
	}

	res, err := h.client.Do(req)
	if err != nil {
		return Message{}, "", fmt.Errorf("agent: call model: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return Message{}, "", failure.ReadRefusal("agent", res)
	}

	var decoded inferReply
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&decoded); err != nil {
		return Message{}, "", fmt.Errorf("agent: decode model reply: %w", err)
	}
	choice, ok := decoded.first()
	if !ok {
		return Message{}, "", errors.New("agent: model returned no answer")
	}
	if strings.TrimSpace(choice.Message.Content) == "" && len(choice.Message.ToolCalls) == 0 {
		// A 200 with nothing in it is an upstream failure or a model that spent the
		// whole cap thinking; either is worth the retry a 502 gets.
		return Message{}, "", fmt.Errorf("agent: model returned no content (finish %s)", choice.FinishReason)
	}
	return choice.Message, choice.FinishReason, nil
}

// body is the one request shape, per provider. The messages and the tools are identical
// either way; only the framing and the two routing knobs differ.
func (h *ModelClient) body(messages []Message, t turn) any {
	var format *responseFormat
	if t.schema != "" {
		format = &responseFormat{
			Type:       "json_schema",
			JSONSchema: jsonSchema{Name: t.name, Schema: json.RawMessage(t.schema)},
		}
	}
	if h.settings.Provider == ProviderOpenRouter {
		if format != nil {
			format.JSONSchema.Strict = true
		}
		return openRouterRequest{
			Model:          h.settings.Model,
			Messages:       messages,
			Tools:          t.tools,
			MaxTokens:      maxOutputTokens,
			Reasoning:      &reasoning{Enabled: true},
			ResponseFormat: format,
			Provider:       &providerPrefs{RequireParameters: true},
		}
	}
	return gatewayRequest{
		Model: h.settings.Model,
		Payload: gatewayInput{
			Messages:       messages,
			Tools:          t.tools,
			MaxTokens:      maxOutputTokens,
			ResponseFormat: format,
		},
	}
}

// schemaError is an answer that did not fit what the schema means: the model's own mistake, and
// the one model error a retry of the loop can fix. Everything else out of a model call — an
// endpoint that never answered, an empty completion, a 401 — is a run error, because replaying
// the loop does not fix it. The loop tells the two apart to decide fault from failure.
type schemaError struct{ err error }

func (e schemaError) Error() string { return e.err.Error() }
func (e schemaError) Unwrap() error { return e.err }

// ask sends one stage's messages and decodes its answer. Every stage fails the same
// way when the model writes something the schema did not: the stage, finish reason
// and head of the answer ride in the error for the Trace.
func ask[T any](ctx context.Context, h *ModelClient, messages []Message, t turn) (T, error) {
	var parsed T
	message, finish, err := h.call(ctx, messages, t)
	if err != nil {
		return parsed, err
	}
	if err := json.Unmarshal([]byte(message.Content), &parsed); err != nil {
		return parsed, schemaError{fmt.Errorf("agent: %s answer is not the schema (finish %s): %w (answer %s)",
			t.name, finish, err, snippet(message.Content))}
	}
	return parsed, nil
}

// Prescreen is the first thing a Job's turn asks: are these words permissible? It
// reads the words alone — no retrieval, no clock — so it stays cheap, and a false "ok"
// costs one wasted planner call rather than a wrong row.
func (h *ModelClient) Prescreen(ctx context.Context, capture string) (Verdict, error) {
	parsed, err := ask[prescreenAnswer](ctx, h, []Message{
		{Role: "system", Content: prescreenPrompt},
		{Role: "user", Content: "capture: " + capture},
	}, turn{name: "prescreen", schema: prescreenSchema})
	if err != nil {
		return Verdict{}, err
	}
	switch parsed.Verdict {
	case "ok", "abuse", "incoherent":
		return Verdict{Outcome: parsed.Verdict, Reason: strings.TrimSpace(parsed.Reason)}, nil
	default:
		return Verdict{}, fmt.Errorf("agent: prescreen verdict %q is not one of ok, abuse, incoherent", parsed.Verdict)
	}
}

// Chat is one free-form agent turn: the capture's context, every message the loop has
// added since (its tool calls, their results, and any lint fault), and the tools on
// offer. No schema is forced, because a schema-constrained turn could never ask to look
// something up; the model ends the loop by calling submit_plan.
func (h *ModelClient) Chat(ctx context.Context, in PlannerInput, sent []Message, tools []Tool) (Message, error) {
	message, _, err := h.call(ctx, append(AgentMessages(h.prompt(), in), sent...), turn{tools: tools})
	if err != nil {
		return Message{}, err
	}
	message.Role = "assistant"
	return message, nil
}

// Finalize is the one strict-schema call behind submit_plan: the same conversation, read
// through the schema the answer is then checked against. What the strict shape refuses — a
// question carrying a record, a record that also asks — is a schemaError too, because it is the
// model's answer that is wrong, not the provider.
func (h *ModelClient) Finalize(ctx context.Context, in PlannerInput, sent []Message) (Answer, error) {
	parsed, err := ask[plannerAnswer](ctx, h, append(AgentMessages(h.prompt(), in), sent...),
		turn{name: "planner", schema: plannerSchema})
	if err != nil {
		return Answer{}, err
	}
	answer, err := answerFrom(parsed, in.Now)
	if err != nil {
		return Answer{}, schemaError{err}
	}
	return answer, nil
}

// Verify is the postagentcheck: the user's own words beside the row the commit
// would record, read by a model that did not propose it.
func (h *ModelClient) Verify(ctx context.Context, c Proposal) (Verdict, error) {
	parsed, err := ask[verifyAnswer](ctx, h, []Message{
		{Role: "system", Content: verifyPrompt},
		{Role: "user", Content: verifyBlock(c)},
	}, turn{name: "verify", schema: verifySchema})
	if err != nil {
		return Verdict{}, err
	}
	switch parsed.Verdict {
	case "ok", "mismatch":
		return Verdict{
			Outcome: parsed.Verdict,
			Reason:  strings.TrimSpace(parsed.Reason),
			Ask:     strings.TrimSpace(parsed.Question),
		}, nil
	default:
		return Verdict{}, fmt.Errorf("agent: verify verdict %q is neither ok nor mismatch", parsed.Verdict)
	}
}

// Tags names the search features of one fact, so a later capture worded nothing like it
// can still find it. The call carries no conversation and no retrieval — the fact is the
// whole input — and the answer is one string the server splits and normalizes. A Memory
// is the only record that carries them, and they are generated on creation.
func (h *ModelClient) Tags(ctx context.Context, text string) ([]string, error) {
	parsed, err := ask[tags](ctx, h, []Message{
		{Role: "system", Content: tagsPrompt},
		{Role: "user", Content: "fact: " + text},
	}, turn{name: "tags", schema: tagsSchema})
	if err != nil {
		return nil, err
	}
	return parseTags(parsed.Tags), nil
}

// tool builds one declaration, so the agent's tool table reads as a list rather than as
// nesting.
func tool(name, description, parameters string) Tool {
	return Tool{Type: functionType, Function: ToolShape{
		Name: name, Description: description, Parameters: json.RawMessage(parameters),
	}}
}

// snippet is the head of an unreadable answer, so a Trace names what the model
// actually wrote instead of only that the decoder refused it.
func snippet(content string) string {
	const limit = 300
	content = strings.Join(strings.Fields(content), " ")
	if len(content) <= limit {
		return content
	}
	return content[:limit] + "…"
}

// verifyBlock is what the checker reads: the user's words first, then the row as it
// would be stored. It is the whole input on purpose — no conversation, no
// retrieval, nothing else to blame.
func verifyBlock(c Proposal) string {
	var block strings.Builder
	block.WriteString("evidence: " + c.Evidence + "\n")
	if c.Reminder == nil {
		block.WriteString("reminder: none\n")
	} else {
		block.WriteString("reminder: " + reminderLine(*c.Reminder) + "\n")
	}
	if c.Fact == "" {
		block.WriteString("fact: none")
	} else {
		block.WriteString("fact: " + c.Fact)
	}
	return block.String()
}

// reminderLine renders one proposed row the way it would be stored: the fact of
// the row, when it rings, in which zone, and how it repeats.
func reminderLine(reminder store.Reminder) string {
	when := reminder.LocalDateTime
	if when == "" {
		when = wallClock(reminder.NextEmitAtUtc, reminder.Timezone)
	}
	frequency := "none"
	if reminder.Rule != nil {
		frequency = reminder.Rule.Frequency
	}
	return fmt.Sprintf("%s | %s %s | recurrence %s", reminder.Text, when, reminder.Timezone, frequency)
}
