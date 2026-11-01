package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"blurt/internal/agent"
	"blurt/internal/push"
)

type scripted struct {
	mu    sync.Mutex
	turns []agent.Answer
	// byText answers by the capture's own words, which is what makes two Jobs in
	// flight at once testable: a positional list would race for the turn.
	byText map[string]agent.Answer
	errs   []error
	// blocks hangs the agent turn until the run's deadline, which is how a flow pins
	// the boundary the worker puts on a provider that never answers.
	blocks bool
	inputs []agent.PlannerInput
	// sent is every message the loop added to the conversation, so a flow can read the
	// lint note a retry was handed.
	sent []agent.Message
	// vetVerdict is what the prescreen answers; the zero value is ok, so a flow only
	// sets it to reach the refusal branch.
	vetVerdict agent.Verdict
	// tags is what the tagger answers for every fact, so a flow can assert what the
	// generated features do to retrieval.
	tags []string
	// tagErr makes the tagger fail, so a flow can assert what a capture does when
	// its features cannot be generated at all.
	tagErr error
	// asks counts every stage call, so a flow can assert which AI was asked how
	// often — the plumbing assertion that no stage was skipped or doubled.
	asks asks
}

// asks is one snapshot of the per-stage call counts. plan counts every call that rode the
// planner port: the free-form agent turn and the strict call behind submit_plan are the same
// model, and a run that skipped either is not a run these flows should pass.
type asks struct {
	vet, plan, tags int
}

func (a asks) String() string {
	return fmt.Sprintf("vet=%d plan=%d tags=%d", a.vet, a.plan, a.tags)
}

func (s *scripted) Prescreen(_ context.Context, _ string) (agent.Verdict, error) {
	s.mu.Lock()
	s.asks.vet++
	verdict := s.vetVerdict
	s.mu.Unlock()
	if verdict.Outcome == "" {
		verdict.Outcome = "ok"
	}
	return verdict, nil
}

// Chat is the agent turn. The double never looks anything up, so it submits at once and
// lets the strict call behind it answer; an error or a block here is the transport failure
// every retry flow scripts.
func (s *scripted) Chat(ctx context.Context, in agent.PlannerInput, sent []agent.Message, _ []agent.Tool) (agent.Message, error) {
	s.mu.Lock()
	s.asks.plan++
	s.inputs = append(s.inputs, in)
	s.sent = append(s.sent, sent...)
	block := s.blocks
	var err error
	if len(s.errs) > 0 {
		err, s.errs = s.errs[0], s.errs[1:]
	}
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return agent.Message{}, ctx.Err()
	}
	if err != nil {
		return agent.Message{}, err
	}
	return agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
		ID: "call-1", Type: "function",
		Function: agent.CallFunction{Name: "submit_plan", Arguments: "{}"},
	}}}, nil
}

// Finalize is the strict call: one scripted answer per run, which is what makes a
// clarification flow a list of runs rather than a list of calls.
func (s *scripted) Finalize(_ context.Context, in agent.PlannerInput, _ []agent.Message) (agent.Answer, error) {
	s.mu.Lock()
	s.asks.plan++
	var err error
	if len(s.errs) > 0 {
		err, s.errs = s.errs[0], s.errs[1:]
	}
	var turn agent.Answer
	if err == nil {
		if mapped, ok := s.byText[latestCapture(in)]; ok {
			turn = mapped
		} else if len(s.turns) > 0 {
			turn, s.turns = s.turns[0], s.turns[1:]
		}
	}
	s.mu.Unlock()
	if err != nil {
		return agent.Answer{}, err
	}
	return turn, nil
}

func (s *scripted) planned() []agent.PlannerInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.PlannerInput(nil), s.inputs...)
}

// Tags is the tagger port: one fixed answer for every fact, so a flow can assert what
// the generated features do to retrieval.
func (s *scripted) Tags(_ context.Context, _ string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asks.tags++
	if s.tagErr != nil {
		return nil, s.tagErr
	}
	return s.tags, nil
}

// callCounts is the plumbing assertion: exactly these many asks per stage. A flow
// uses it to pin which AI answered which stage, on which turns.
func (s *scripted) callCounts() asks {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asks
}

// wantAsks is the plumbing assertion at the stage ports: exactly these many asks per
// stage. A run that asked the wrong AI, skipped a stage, or asked twice fails here
// rather than in the rows it wrote.
func (p *pump) wantAsks(t *testing.T, vet, plan, tags int) {
	t.Helper()
	want := asks{vet: vet, plan: plan, tags: tags}
	p.declared, p.expectAt = true, &want
	if got := p.model.callCounts(); got != want {
		t.Fatalf("model asks = %s, want %s", got, want)
	}
}

// wantWire is wantAsks for a flow on the real transport: exactly these many gateway
// requests per schema. The port counts are unused there, so this satisfies the same
// declare-your-calls rule at the layer the run actually crossed.
func (p *pump) wantWire(t *testing.T, script *gatewayScript, want map[string]int) {
	t.Helper()
	p.declared = true
	script.wantSchemaCalls(t, want)
}

// checkAsks is the cleanup pass behind the declaration rule: a flow that never said
// how many times each stage asked fails even when every row it wrote is right, because
// the rows cannot tell a skipped stage from an unnecessary one.
func (p *pump) checkAsks() {
	t := p.t
	t.Helper()
	if !p.declared {
		t.Errorf("flow never declared its model calls: assert them with wantAsks or wantWire (actual: %s)", p.model.callCounts())
		return
	}
	// A declared port budget is re-read after the loops stop, so a straggling stage
	// cannot slip a call in after the assertion.
	if p.expectAt != nil {
		if got := p.model.callCounts(); got != *p.expectAt {
			t.Errorf("model asks after shutdown = %s, want %s", got, *p.expectAt)
		}
	}
}

// latestCapture is the newest user turn's words, which is what a text-keyed answer
// looks up.
func latestCapture(in agent.PlannerInput) string {
	if n := len(in.Conversation); n > 0 {
		return in.Conversation[n-1].Text
	}
	return ""
}

// recordingPusher stands in for the relay so a flow can assert the user was told.
type recordingPusher struct {
	mu    sync.Mutex
	calls int
	title string
	body  string
}

func (p *recordingPusher) NotifyUser(_ context.Context, _ int64, msg push.Message) (push.Result, error) {
	p.mu.Lock()
	p.calls++
	p.title, p.body = msg.Title, msg.Body
	p.mu.Unlock()
	return push.Result{Sent: 1}, nil
}

func (p *recordingPusher) sent() (int, string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.title, p.body
}

// scriptedTranscriber stands in for phonon: one fixed transcript for every clip.
type scriptedTranscriber struct{ text string }

func (s scriptedTranscriber) Transcribe(_ context.Context, audio io.Reader) (string, error) {
	_, _ = io.Copy(io.Discard, audio)
	return s.text, nil
}

// blockingTranscriber is the slow-provider case: it never answers before the
// worker's own attempt deadline.
type blockingTranscriber struct{}

func (blockingTranscriber) Transcribe(ctx context.Context, audio io.Reader) (string, error) {
	_, _ = io.Copy(io.Discard, audio)
	<-ctx.Done()
	return "", ctx.Err()
}

// wireMessage is the one chat message a prompt assertion reads.
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// gatewayCall is the part of a gateway request a prompt assertion needs: the schema it
// asked for — or "agent" for the turn that offered tools instead of a schema — and the
// messages the model read.
type gatewayCall struct {
	Schema   string
	Messages []wireMessage
}

// gatewayScript stands in for the real gateway: it answers the agent turn by submitting the
// plan, the strict turn with the answers a test scripts, the tags turn with the features a
// test scripts, and the screen and check with ok. Every request is recorded, so a prompt can
// be read back.
type gatewayScript struct {
	mu       sync.Mutex
	records  []string
	tags     string
	requests []gatewayCall
}

func newGatewayScript(t *testing.T, records ...string) (*httptest.Server, *gatewayScript) {
	t.Helper()
	script := &gatewayScript{records: records}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both envelopes are read in one shape: the gateway nests messages, the tools
		// and the schema under payload, an OpenAI-compatible route puts them at the top.
		var body struct {
			Messages       []wireMessage `json:"messages"`
			Tools          []toolShape   `json:"tools"`
			ResponseFormat struct {
				JSONSchema struct {
					Name string `json:"name"`
				} `json:"json_schema"`
			} `json:"response_format"`
			Payload struct {
				Messages       []wireMessage `json:"messages"`
				Tools          []toolShape   `json:"tools"`
				ResponseFormat struct {
					JSONSchema struct {
						Name string `json:"name"`
					} `json:"json_schema"`
				} `json:"response_format"`
			} `json:"payload"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)

		name, messages := body.Payload.ResponseFormat.JSONSchema.Name, body.Payload.Messages
		tools := body.Payload.Tools
		if name == "" && messages == nil {
			name, messages, tools = body.ResponseFormat.JSONSchema.Name, body.Messages, body.Tools
		}
		// The turn that offers tools names no schema, and its answer is a call rather
		// than content: submitting is how a captured turn ends.
		if len(tools) > 0 {
			name = "agent"
		}

		script.mu.Lock()
		script.requests = append(script.requests, gatewayCall{Schema: name, Messages: messages})
		// The screen passes by default; only the tests that script a refusal answer
		// otherwise.
		content := `{"verdict":"ok","reason":"","question":""}`
		switch name {
		case "agent":
			content = `{"tool":"submit_plan"}`
		case "planner":
			// The answer a flow that scripts too few turns falls back on: the capture
			// could not be placed, so the model asks rather than proposing nothing.
			content = `{"kind":"question","question":"When should I remind you?"}`
			if len(script.records) > 0 {
				content, script.records = script.records[0], script.records[1:]
			}
		case "tags":
			raw, _ := json.Marshal(map[string]string{"tags": script.tags})
			content = string(raw)
		}
		script.mu.Unlock()
		if name == "agent" {
			_, _ = w.Write(gatewayToolCall())
			return
		}
		_, _ = w.Write(gatewayEnvelope(content))
	}))
	t.Cleanup(server.Close)
	return server, script
}

// toolShape is one declared tool as a request carries it, which is all a script needs to
// tell the agent turn from a schema turn.
type toolShape struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// gatewayToolCall is the gateway's envelope for an answer that is a call: the plan is
// submitted, and the strict call behind it is what the test scripts.
func gatewayToolCall() []byte {
	body, err := json.Marshal(map[string]any{"output": map[string]any{"choices": []any{
		map[string]any{"finish_reason": "tool_calls", "message": map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id": "call-1", "type": "function",
				"function": map[string]any{"name": "submit_plan", "arguments": "{}"},
			}},
		}}}}})
	if err != nil {
		panic(err)
	}
	return body
}

// gatewayEnvelope wraps one schema answer the way the gateway does.
func gatewayEnvelope(content string) []byte {
	body, err := json.Marshal(map[string]any{"output": map[string]any{"choices": []any{
		map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}}}})
	if err != nil {
		panic(err)
	}
	return body
}

// block is the message a call rendered the block into. It is not always the last message:
// the strict call appends the loop's own turns after it, so a prompt assertion reads the
// message the capture is in rather than the newest one.
func (c gatewayCall) block(t *testing.T, capture string) string {
	t.Helper()
	for _, message := range c.Messages {
		if strings.HasSuffix(message.Content, "capture: "+capture) {
			return message.Content
		}
	}
	t.Fatalf("no message in the %s call carried the capture %q", c.Schema, capture)
	return ""
}

// call returns the core request whose conversation carried the captured turn. The screen
// carries the same words in a shorter message, so the schema is what tells them apart.
func (s *gatewayScript) call(t *testing.T, capture string) gatewayCall {
	t.Helper()
	return s.callFor(t, capture, "planner")
}

// wantSchemaCalls is the plumbing assertion at the wire: exactly these many requests
// per schema, so a real-transport flow proves which stage reached the gateway and how
// often. A schema not named is one the flow must not have paid for.
func (s *gatewayScript) wantSchemaCalls(t *testing.T, want map[string]int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	got := map[string]int{}
	for _, call := range s.requests {
		got[call.Schema]++
	}
	for schema, n := range want {
		if got[schema] != n {
			t.Fatalf("%s calls = %d, want %d (all: %v)", schema, got[schema], n, got)
		}
		delete(got, schema)
	}
	for schema, n := range got {
		if n > 0 {
			t.Fatalf("%s was called %d times, want none (all: %v)", schema, n, got)
		}
	}
}

// callFor returns the request of one schema whose conversation carried the capture. The
// block is not always last: the strict call appends the loop's own turns after it, so the
// search is over every message rather than the last one.
func (s *gatewayScript) callFor(t *testing.T, capture, schema string) gatewayCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, call := range s.requests {
		if len(call.Messages) == 0 || (schema != "" && call.Schema != schema) {
			continue
		}
		for _, message := range call.Messages {
			if strings.HasSuffix(message.Content, "capture: "+capture) {
				return call
			}
		}
	}
	t.Fatalf("no prompt (schema %q) carried the capture %q; saw %d requests",
		schema, capture, len(s.requests))
	return gatewayCall{}
}
