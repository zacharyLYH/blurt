package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"blurt/internal/config"
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

// TestGatewayProposeSendsTheStructuredSchema is the transport contract: chat messages
// plus a JSON schema rather than OpenAI tool calling, the two resolved fields pinned by
// a pattern, recurrence an enum, and the capture last in the prompt.
func TestGatewayProposeSendsTheStructuredSchema(t *testing.T) {
	var got inferRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer live-key" {
			t.Errorf("authorization = %q, want the bearer key", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		answer := `{"text":"Call mum","zone":"Asia/Singapore","date":"2026-10-25",` +
			`"time":"19:00","recurrence":"none","evidence":"they said tomorrow at 7pm"}`
		_, _ = w.Write(gatewayAnswer(t, "stop", answer))
	}))
	defer server.Close()

	gateway := NewGateway(config.Agent{URL: server.URL, APIKey: "live-key", Model: "local-model"})
	turn, err := gateway.Propose(context.Background(), Input{
		Conversation: []store.Message{{Role: "user", Text: "call mum tomorrow at 7pm", TZ: "Asia/Singapore"}},
		CurrentTZ:    "Asia/Singapore",
		Now:          pinned,
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
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
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(got.Payload.ResponseFormat.JSONSchema.Schema, &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if len(schema.Properties) != 7 || len(schema.Required) != 7 {
		t.Fatalf("schema has %d fields and %d required, want 7 and 7",
			len(schema.Properties), len(schema.Required))
	}
	// The second record kind rides the same schema, and the server reads which
	// records a turn proposed off which fields came back filled.
	if _, ok := schema.Properties["memoryText"]; !ok {
		t.Fatalf("schema = %s, want the memoryText field", got.Payload.ResponseFormat.JSONSchema.Schema)
	}
	// Every field may be the empty string, which is what makes "I am not sure"
	// expressible, and recurrence is an enum, so a pattern the server cannot read
	// never even arrives.
	if !strings.Contains(string(got.Payload.ResponseFormat.JSONSchema.Schema),
		`"enum":["none","daily","weekly","monthly","yearly"]`) {
		t.Fatalf("schema = %s, want the recurrence enum", got.Payload.ResponseFormat.JSONSchema.Schema)
	}
	if len(got.Payload.Messages) != 2 {
		t.Fatalf("messages = %d, want a system prompt and one user turn", len(got.Payload.Messages))
	}
	if got.Payload.Messages[0].Role != "system" || got.Payload.Messages[0].Content != systemPrompt {
		t.Fatal("the first message is not the system prompt")
	}
	if !strings.HasSuffix(got.Payload.Messages[1].Content, "capture: call mum tomorrow at 7pm") {
		t.Fatalf("user turn = %q, want the capture last", got.Payload.Messages[1].Content)
	}
	// A relative phrase cannot be resolved without knowing what day it is.
	if !strings.Contains(got.Payload.Messages[1].Content, "now_local: 2026-10-24T18:30") {
		t.Fatalf("user turn = %q, want the local clock in it", got.Payload.Messages[1].Content)
	}

	if len(turn.Writes) != 1 || turn.Writes[0].Date != "2026-10-25" ||
		turn.Writes[0].Recurrence != RecurrenceNone || turn.Writes[0].Evidence == "" {
		t.Fatalf("writes = %+v, want one resolved reminder", turn.Writes)
	}
}

// turnOf is the whole shape decision: which fields the model filled is what the server
// commits. An empty event and an empty fact propose nothing at all, and an unanswered
// pattern reads as a one-off, because a model told to fill a declared field sometimes
// fills nothing.
func TestTurnOf(t *testing.T) {
	cases := []struct {
		name   string
		parsed records
		want   []Write
	}{
		{"a resolved reminder", records{Text: " Call mum ", Zone: " Asia/Singapore ", Date: "2026-10-25",
			Time: "19:00", Recurrence: "none", Evidence: " they said 7pm "},
			[]Write{{Kind: KindReminder, Text: "Call mum", Zone: "Asia/Singapore", Date: "2026-10-25",
				Time: "19:00", Recurrence: RecurrenceNone, Evidence: "they said 7pm"}}},
		{"a repeat", records{Text: "Trash", Zone: "Asia/Singapore", Date: "2026-10-27",
			Time: "19:00", Recurrence: "weekly"},
			[]Write{{Kind: KindReminder, Text: "Trash", Zone: "Asia/Singapore", Date: "2026-10-27",
				Time: "19:00", Recurrence: store.FrequencyWeekly}}},
		{"an unanswered pattern", records{Text: "Buy glue"},
			[]Write{{Kind: KindReminder, Text: "Buy glue", Recurrence: RecurrenceNone}}},
		{"a fact alone", records{MemoryText: " Milk is cheaper at store A "},
			[]Write{{Kind: KindMemory, Text: "Milk is cheaper at store A"}}},
		{"an event and a fact", records{Text: "Lunch at Ikea", MemoryText: "I have an Ikea voucher",
			Zone: "Asia/Singapore", Date: "2026-10-30", Time: "13:00", Recurrence: "none"},
			[]Write{
				{Kind: KindReminder, Text: "Lunch at Ikea", Zone: "Asia/Singapore", Date: "2026-10-30",
					Time: "13:00", Recurrence: RecurrenceNone},
				{Kind: KindMemory, Text: "I have an Ikea voucher"},
			}},
		{"no event, no record", records{Text: "", Zone: "Asia/Singapore", Date: "2026-10-25"}, nil},
		{"blanks read as nothing", records{Text: "   ", MemoryText: "  "}, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			turn := turnOf(test.parsed)
			if len(turn.Writes) != len(test.want) || (len(test.want) > 0 && !reflect.DeepEqual(turn.Writes, test.want)) {
				t.Fatalf("writes = %+v, want %+v", turn.Writes, test.want)
			}
		})
	}
}

// The question writer reads the missing fields beside the capture, and neither retrieval
// nor the record's own prose is in front of it: it asks about what is unknown, in the
// user's own words.
func TestContextBlockCarriesTheMissingAndCapture(t *testing.T) {
	block := contextBlock(Input{
		CurrentTZ: "Asia/Singapore",
		Now:       pinned,
		Missing:   []Field{FieldDate, FieldTime},
	}, "buy glue when I'm home")

	for _, want := range []string{
		"current_tz: Asia/Singapore",
		"now_local: 2026-10-24T18:30",
		"missing: date, time",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block = %q, want %q", block, want)
		}
	}
	if !strings.HasSuffix(block, "capture: buy glue when I'm home") {
		t.Fatalf("block = %q, want the capture last", block)
	}
	if strings.Contains(block, "nearby reminders") || strings.Contains(block, "remembered") {
		t.Fatalf("block = %q, want no retrieval in the question prompt", block)
	}
}

// The second prompt is the same transport with one string to fill, and the fields
// MissingFields refused named in it: that is the only thing the question is about.
func TestGatewayQuestionSendsTheQuestionPrompt(t *testing.T) {
	var got inferRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write(gatewayAnswer(t, "stop", `{"question":"Which day should I set?"}`))
	}))
	defer server.Close()

	gateway := NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	question, err := gateway.Question(context.Background(), Input{
		Conversation: []store.Message{{Role: "user", Text: "buy glue", TZ: "Asia/Singapore"}},
		CurrentTZ:    "Asia/Singapore",
		Now:          pinned,
	}, []Field{FieldDate})
	if err != nil {
		t.Fatalf("Question: %v", err)
	}
	if question != "Which day should I set?" {
		t.Fatalf("question = %q", question)
	}
	if got.Payload.ResponseFormat.JSONSchema.Name != "question" {
		t.Fatalf("schema = %q, want the question schema", got.Payload.ResponseFormat.JSONSchema.Name)
	}
	if got.Payload.Messages[0].Content != questionPrompt {
		t.Fatal("the question prompt was not the system message")
	}
	if got.Payload.Messages[0].Content == systemPrompt {
		t.Fatal("the records prompt was sent for the question")
	}
	if !strings.Contains(got.Payload.Messages[1].Content, "missing: date") {
		t.Fatalf("user turn = %q, want the missing fields", got.Payload.Messages[1].Content)
	}
}
