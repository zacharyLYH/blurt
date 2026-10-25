package flow

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"blurt/internal/agent"
	"blurt/internal/decision"
	"blurt/internal/push"
)

type scripted struct {
	mu    sync.Mutex
	turns []agent.Turn
	// byText answers by the capture's own words, which is what makes two Jobs in
	// flight at once testable: a positional list would race for the turn.
	byText map[string]agent.Turn
	errs   []error
	blocks bool
	inputs []agent.Input
}

func (s *scripted) Propose(ctx context.Context, in agent.Input) (agent.Turn, error) {
	s.mu.Lock()
	s.inputs = append(s.inputs, in)
	block := s.blocks
	var err error
	if len(s.errs) > 0 {
		err, s.errs = s.errs[0], s.errs[1:]
	}
	var turn agent.Turn
	// A faulted or hanging attempt consumes no scripted turn: the turn is for the
	// run that actually reaches the model.
	if err == nil && !block {
		if mapped, ok := s.byText[captureOf(in)]; ok {
			turn = mapped
		} else if len(s.turns) > 0 {
			turn, s.turns = s.turns[0], s.turns[1:]
		}
	}
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return agent.Turn{}, ctx.Err()
	}
	if err != nil {
		return agent.Turn{}, err
	}
	return turn, nil
}

func (s *scripted) Ask(ctx context.Context, _ agent.Input) (string, error) {
	s.mu.Lock()
	block := s.blocks
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "When should I remind you?", nil
}

func (s *scripted) planned() []agent.Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Input(nil), s.inputs...)
}

// captureOf is the newest user turn's words, which is what a text-keyed answer
// looks up.
func captureOf(in agent.Input) string {
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

// blockingDecider is the slow-classifier case for the transcript gate.
type blockingDecider struct{}

func (blockingDecider) Decide(ctx context.Context, _ string, _ decision.Questions) (decision.Decisions, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// fixedTagger is the Tagger port with one answer, so a flow can assert what the
// generated features do to retrieval.
type fixedTagger struct{ tags []string }

func (f *fixedTagger) Tags(_ context.Context, _ string) ([]string, error) { return f.tags, nil }

// wireMessage is the one chat message a prompt assertion reads.
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// gatewayCall is the part of a gateway request a prompt assertion needs.
type gatewayCall struct {
	Messages []wireMessage
}

// gatewayScript stands in for the real gateway: it answers the records turn with
// the answers a test scripts, and records every request so a prompt can be read
// back. The question turn gets one fixed sentence, so a clarified flow still runs.
type gatewayScript struct {
	mu       sync.Mutex
	records  []string
	requests []gatewayCall
}

func newGatewayScript(t *testing.T, records ...string) (*httptest.Server, *gatewayScript) {
	t.Helper()
	script := &gatewayScript{records: records}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Payload struct {
				Messages       []wireMessage `json:"messages"`
				ResponseFormat struct {
					JSONSchema struct {
						Name string `json:"name"`
					} `json:"json_schema"`
				} `json:"response_format"`
			} `json:"payload"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)

		script.mu.Lock()
		script.requests = append(script.requests, gatewayCall{Messages: body.Payload.Messages})
		content := `{"question":"When should I remind you?"}`
		if body.Payload.ResponseFormat.JSONSchema.Name != "question" && len(script.records) > 0 {
			content, script.records = script.records[0], script.records[1:]
		}
		script.mu.Unlock()
		_, _ = w.Write(gatewayEnvelope(content))
	}))
	t.Cleanup(server.Close)
	return server, script
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

// call returns the one request whose last message is the captured turn.
func (s *gatewayScript) call(t *testing.T, capture string) gatewayCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, call := range s.requests {
		if len(call.Messages) == 0 {
			continue
		}
		if strings.HasSuffix(call.Messages[len(call.Messages)-1].Content, "capture: "+capture) {
			return call
		}
	}
	t.Fatalf("no prompt carried the capture %q; saw %d requests", capture, len(s.requests))
	return gatewayCall{}
}
