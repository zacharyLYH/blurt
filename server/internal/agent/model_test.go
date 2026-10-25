package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"blurt/internal/config"
	"blurt/internal/failure"
	"blurt/internal/store"
)

// gatewayAnswer builds the gateway's envelope around one schema-constrained answer,
// so a test only has to write the part it cares about.
func gatewayAnswer(t *testing.T, finish, content string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"output": map[string]any{"choices": []any{
		map[string]any{"finish_reason": finish, "message": map[string]any{"content": content}}}}})
	if err != nil {
		t.Fatalf("encode answer: %v", err)
	}
	return body
}

// TestGatewayProposeSendsTheSmallSchema is the transport contract: chat messages plus a
// JSON schema rather than OpenAI tool calling, and the capture last in the prompt.
func TestGatewayProposeSendsTheSmallSchema(t *testing.T) {
	var got inferRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer live-key" {
			t.Errorf("authorization = %q, want the bearer key", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		answer := `{"say":"Reminder set for tomorrow at 6pm.","text":"Buy glue",` +
			`"timezone":"Asia/Singapore","timePhrase":"tomorrow at 6pm","memoryText":""}`
		_, _ = w.Write(gatewayAnswer(t, "stop", answer))
	}))
	defer server.Close()

	gateway := NewGateway(config.Agent{URL: server.URL, APIKey: "live-key", Model: "local-model"})
	turn, err := gateway.Propose(context.Background(), Input{
		Conversation: []store.Message{{Role: "user", Text: "buy glue at 6pm tomorrow", TZ: "Asia/Singapore"}},
		CurrentTZ:    "Asia/Singapore",
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if got.Model != "local-model" {
		t.Fatalf("model = %q, want the configured one", got.Model)
	}
	if got.Payload.MaxTokens != maxOutputTokens {
		t.Fatalf("max_tokens = %d, want %d", got.Payload.MaxTokens, maxOutputTokens)
	}
	if got.Payload.ResponseFormat.Type != "json_schema" ||
		got.Payload.ResponseFormat.JSONSchema.Name != "records" {
		t.Fatalf("response format = %+v, want records as a json_schema", got.Payload.ResponseFormat)
	}
	// The schema stays small on purpose: a ten-field one made the model repeat
	// itself until the answer was unparseable.
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(got.Payload.ResponseFormat.JSONSchema.Schema, &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if len(schema.Properties) != 5 {
		t.Fatalf("schema has %d fields, want the measured 5", len(schema.Properties))
	}
	if len(got.Payload.Messages) != 2 {
		t.Fatalf("messages = %d, want a system prompt and one user turn", len(got.Payload.Messages))
	}
	if got.Payload.Messages[0].Role != "system" || got.Payload.Messages[0].Content != systemPrompt {
		t.Fatal("the first message is not the system prompt")
	}
	if !strings.HasSuffix(got.Payload.Messages[1].Content, "capture: buy glue at 6pm tomorrow") {
		t.Fatalf("user turn = %q, want the capture last", got.Payload.Messages[1].Content)
	}
	if !strings.Contains(got.Payload.Messages[1].Content, "current_tz: Asia/Singapore") {
		t.Fatalf("user turn = %q, want the current zone in it", got.Payload.Messages[1].Content)
	}

	if len(turn.Writes) != 1 {
		t.Fatalf("writes = %+v, want one reminder", turn.Writes)
	}
	write := turn.Writes[0]
	if write.Kind != KindReminder || write.Text != "Buy glue" || write.TimePhrase != "tomorrow at 6pm" ||
		write.Timezone != "Asia/Singapore" {
		t.Fatalf("write = %+v", write)
	}
	if turn.Say != "Reminder set for tomorrow at 6pm." {
		t.Fatalf("say = %q", turn.Say)
	}
}

// A history turn rides in front of the capture, so a clarification reply is planned
// in the conversation it belongs to.
func TestGatewayProposeCarriesTheConversation(t *testing.T) {
	var got inferRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write(gatewayAnswer(t, "stop", `{"say":"Reminder set.","text":"Buy glue",`+
			`"timezone":"Asia/Singapore","timePhrase":"tomorrow at 6pm","memoryText":""}`))
	}))
	defer server.Close()

	gateway := NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	if _, err := gateway.Propose(context.Background(), Input{Conversation: []store.Message{
		{Role: "user", Text: "buy glue", TZ: "Asia/Singapore"},
		{Role: "assistant", Text: "When should I remind you?"},
		{Role: "user", Text: "at 6pm tomorrow", TZ: "Asia/Singapore"},
	}}); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(got.Payload.Messages) != 4 {
		t.Fatalf("messages = %d, want the prompt, two history turns and the capture",
			len(got.Payload.Messages))
	}
	if got.Payload.Messages[2].Content != "When should I remind you?" {
		t.Fatalf("history turn = %+v", got.Payload.Messages[2])
	}
}

// The second prompt is the same transport with one string to fill: the question prompt goes as the
// system message, and the schema is named for what it asks for.
func TestGatewayAskSendsTheQuestionPrompt(t *testing.T) {
	var got inferRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write(gatewayAnswer(t, "stop", `{"question":"When should I remind you about “buy glue”?"}`))
	}))
	defer server.Close()

	gateway := NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	question, err := gateway.Ask(context.Background(), Input{
		Conversation: []store.Message{{Role: "user", Text: "buy glue when I'm home", TZ: "Asia/Singapore"}},
		CurrentTZ:    "Asia/Singapore",
		Checks:       []Check{{Code: CheckTimeUnresolved, Field: "timePhrase"}},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if question != "When should I remind you about “buy glue”?" {
		t.Fatalf("question = %q", question)
	}
	if got.Payload.ResponseFormat.JSONSchema.Name != "question" {
		t.Fatalf("schema name = %q, want question", got.Payload.ResponseFormat.JSONSchema.Name)
	}
	if got.Payload.Messages[0].Content != questionPrompt {
		t.Fatal("the system message is not the question prompt")
	}
	// The refusal the question is about reaches the writer, so it asks for what is missing.
	if !strings.Contains(got.Payload.Messages[1].Content, "these were refused, fix them or ask:") {
		t.Fatalf("user turn = %q, want the refusals in it", got.Payload.Messages[1].Content)
	}
}

// An answer the schema did not produce is refused rather than half-read, so the
// worker retries instead of committing a record built out of a truncated turn.
func TestGatewayProposeRefusesAnAnswerOutsideTheSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(gatewayAnswer(t, "stop", `{"say":"Reminder set for tomorrow at 6pm`))
	}))
	defer server.Close()

	gateway := NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	_, err := gateway.Propose(context.Background(), Input{
		Conversation: []store.Message{{Role: "user", Text: "buy glue"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not the schema") {
		t.Fatalf("Plan with a truncated answer: %v", err)
	}
}

// A refusal is permanent when a retry cannot fix it, so a bad key fails the Job at
// once instead of burning every attempt.
func TestGatewayProposeClassifiesRefusals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"missing or invalid api key"}}`))
	}))
	defer server.Close()

	gateway := NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	_, err := gateway.Propose(context.Background(), Input{
		Conversation: []store.Message{{Role: "user", Text: "buy glue"}},
	})
	if err == nil {
		t.Fatal("Plan with a 401: want an error")
	}
	if !failure.Permanent(err) {
		t.Fatalf("permanent(%v) = false, want a bad key to be permanent", err)
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("err = %v, want the gateway's own message", err)
	}
}

// A refusal reaches the next prompt with its own words, and a sibling that passed
// check is named beside it, so the model can attribute the refusal and knows what
// survived.
func TestContextBlockCarriesTheWrites(t *testing.T) {
	block := contextBlock(Input{
		CurrentTZ: "Asia/Singapore",
		Checks: []Check{{
			Write: reminderWrite("Buy glue", "when I am home"),
			Code:  CheckTimeUnresolved,
			Field: "timePhrase",
		}},
		Accepted: []Write{reminderWrite("Buy glue", "tomorrow at 6pm")},
	}, "buy glue at 6pm tomorrow")

	for _, want := range []string{
		"- time_unresolved (timePhrase): Buy glue / when I am home",
		"these passed check:",
		"- Buy glue / tomorrow at 6pm",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block = %q, want %q", block, want)
		}
	}
}

// turnOf is the whole shape decision: which fields the model filled is what the
// server commits, so nothing asks a small model to pick a tool name.
func TestTurnOf(t *testing.T) {
	cases := []struct {
		name    string
		parsed  records
		want    []string
		wantSay string
	}{
		{"a reminder", records{Say: "Reminder set.", Text: "Buy glue", Timezone: "Asia/Singapore",
			TimePhrase: "tomorrow at 6pm"}, []string{KindReminder}, "Reminder set."},
		{"a fact", records{Say: "Noted.", MemoryText: "Milk is cheaper at store A"},
			[]string{KindMemory}, "Noted."},
		{"both", records{Say: "Both.", Text: "Lunch at Ikea", Timezone: "Asia/Singapore",
			TimePhrase: "friday at 1pm", MemoryText: "I have an Ikea voucher"},
			[]string{KindReminder, KindMemory}, "Both."},
		{"neither", records{Say: "Hmm."}, nil, "Hmm."},
		{"blanks read as nothing", records{Text: "   "}, nil, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			turn := turnOf(test.parsed)
			if turn.Say != test.wantSay {
				t.Fatalf("say = %q, want %q", turn.Say, test.wantSay)
			}
			if len(turn.Writes) != len(test.want) {
				t.Fatalf("writes = %+v, want %v", turn.Writes, test.want)
			}
			for i, kind := range test.want {
				if turn.Writes[i].Kind != kind {
					t.Fatalf("write %d = %q, want %q", i, turn.Writes[i].Kind, kind)
				}
			}
		})
	}
}
