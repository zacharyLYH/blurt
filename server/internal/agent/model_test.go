package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"blurt/internal/store"
)

// mustJSON encodes one answer envelope, so a transport test never repeats the one kind of
// error no handler can do anything about.
func mustJSON(t *testing.T, body map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode answer: %v", err)
	}
	return encoded
}

// gatewayAnswer builds the home gateway's envelope around one schema-constrained answer.
func gatewayAnswer(t *testing.T, finish, content string) []byte {
	return mustJSON(t, map[string]any{"output": map[string]any{"choices": []any{
		map[string]any{"finish_reason": finish, "message": map[string]any{"content": content}}}}})
}

// openRouterAnswer is the OpenAI-compatible envelope: choices at the top.
func openRouterAnswer(t *testing.T, finish, content string) []byte {
	return mustJSON(t, map[string]any{"choices": []any{
		map[string]any{"finish_reason": finish, "message": map[string]any{"content": content}}}})
}

// toolCallAnswer is the OpenAI envelope for an answer that is a call rather than content.
func toolCallAnswer(t *testing.T, name, arguments string) []byte {
	return mustJSON(t, map[string]any{"choices": []any{map[string]any{
		"finish_reason": "tool_calls",
		"message": map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": name, "arguments": arguments}}},
		},
	}}})
}

// modelServer is the one transport fixture: a stub provider, a client pointed at it, and the
// cleanup. settings carry the provider, key and model a test cares about; the URL is the stub.
func modelServer(t *testing.T, settings Settings, handle func(http.ResponseWriter, *http.Request)) *ModelClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(handle))
	t.Cleanup(server.Close)
	settings.URL = server.URL
	return NewModelClient(settings)
}

// TestModelClientFinalizeSendsTheStructuredSchema is the transport contract of the strict
// call behind submit_plan: chat messages plus a JSON schema and no tools, the clock carried
// as a plain field the server parses, recurrence an enum, the kind an enum, and the capture
// last in the prompt. The day rides in as a token the server resolves, so the answer below
// is a phrase and the record is a date.
func TestModelClientFinalizeSendsTheStructuredSchema(t *testing.T) {
	var got gatewayRequest
	gateway := modelServer(t, Settings{APIKey: "live-key", Model: "local-model"}, func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer live-key" {
			t.Errorf("authorization = %q, want the bearer key", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		answer := `{"kind":"records","text":"Call mum","memories":[],"zone":"Asia/Singapore",` +
			`"day":"tomorrow","time":"19:00","recurrence":"none",` +
			`"evidence":"they said tomorrow at 7pm","question":""}`
		_, _ = w.Write(gatewayAnswer(t, "stop", answer))
	})

	turn, err := gateway.Finalize(context.Background(), PlannerInput{
		Conversation: []store.Message{{Role: "user", Text: "call mum tomorrow at 7pm", TZ: "Asia/Singapore"}},
		CurrentTZ:    "Asia/Singapore",
		Now:          pinned,
	}, nil)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got.Payload.Tools != nil {
		t.Fatalf("tools = %+v, want none on the strict call", got.Payload.Tools)
	}

	if got.Model != "local-model" {
		t.Fatalf("model = %q, want the configured one", got.Model)
	}
	if got.Payload.MaxTokens != maxOutputTokens {
		t.Fatalf("max_tokens = %d, want %d", got.Payload.MaxTokens, maxOutputTokens)
	}
	if got.Payload.ResponseFormat == nil || got.Payload.ResponseFormat.Type != "json_schema" ||
		got.Payload.ResponseFormat.JSONSchema.Name != "planner" {
		t.Fatalf("response format = %+v, want the record schema", got.Payload.ResponseFormat)
	}
	var schema struct {
		Properties           map[string]any `json:"properties"`
		Required             []string       `json:"required"`
		AdditionalProperties bool           `json:"additionalProperties"`
	}
	if err := json.Unmarshal(got.Payload.ResponseFormat.JSONSchema.Schema, &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	// Nine fields, all required, and the object closed: that is the shape a strict
	// structured output accepts, and the kind is one of them so the server never
	// infers an answer from an empty string.
	if len(schema.Properties) != 9 || len(schema.Required) != 9 || schema.AdditionalProperties {
		t.Fatalf("schema has %d fields, %d required, additionalProperties %v; want 9, 9, false",
			len(schema.Properties), len(schema.Required), schema.AdditionalProperties)
	}
	for _, field := range []string{"kind", "text", "memories", "zone", "day", "time", "recurrence", "evidence", "question"} {
		if _, ok := schema.Properties[field]; !ok {
			t.Fatalf("schema = %s, want the %s field", got.Payload.ResponseFormat.JSONSchema.Schema, field)
		}
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
	if got.Payload.Messages[0].Role != "system" || got.Payload.Messages[0].Content != AgentPrompt {
		t.Fatal("the first message is not the agent prompt")
	}
	if !strings.HasSuffix(got.Payload.Messages[1].Content, "capture: call mum tomorrow at 7pm") {
		t.Fatalf("user turn = %q, want the capture last", got.Payload.Messages[1].Content)
	}
	// A relative phrase cannot be resolved without knowing what day it is.
	if !strings.Contains(got.Payload.Messages[1].Content, "now_local: 2026-10-24T18:30") {
		t.Fatalf("user turn = %q, want the local clock in it", got.Payload.Messages[1].Content)
	}

	if turn.Kind != AnswerRecords || len(turn.Records) != 1 || turn.Records[0].Date != "2026-10-25" ||
		turn.Records[0].Day != "tomorrow" || turn.Records[0].Recurrence != RecurrenceNone ||
		turn.Records[0].Evidence == "" {
		t.Fatalf("turn = %+v, want one resolved reminder", turn)
	}
}

// The agent turn is the free-form half of the loop: no schema, the tools on offer, and an
// answer that may be a call rather than content. A tool call is read as one, arguments and
// all, because that is what the loop dispatches; the loop's own turns ride after the
// conversation the server rendered.
func TestModelClientChatOffersToolsAndReadsACall(t *testing.T) {
	var got openRouterRequest
	gateway := modelServer(t, Settings{Provider: ProviderOpenRouter, Model: "hosted/model"}, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write(toolCallAnswer(t, toolFindReminders, `{"query":"dentist"}`))
	})

	reply, err := gateway.Chat(context.Background(), PlannerInput{
		Conversation: []store.Message{{Role: "user", Text: "push the dentist", TZ: zone}},
		CurrentTZ:    zone,
		Now:          pinned,
	}, []Message{{Role: "tool", ToolCallID: "call_1", Content: "id=7 Dentist | 2026-10-27T10:00"}}, agentTools)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if got.ResponseFormat != nil {
		t.Fatalf("response format = %+v, want none on a tool turn", got.ResponseFormat)
	}
	if len(got.Tools) != len(agentTools) || got.Tools[0].Function.Name != toolFindReminders ||
		got.Tools[2].Function.Name != toolSubmitPlan {
		t.Fatalf("tools = %+v, want the agent's own list", got.Tools)
	}
	if len(got.Messages) != 3 || got.Messages[2].Role != "tool" || got.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("messages = %+v, want the loop's own turn last", got.Messages)
	}
	if reply.Role != "assistant" || len(reply.ToolCalls) != 1 ||
		reply.ToolCalls[0].Function.Name != toolFindReminders ||
		reply.ToolCalls[0].Function.Arguments != `{"query":"dentist"}` {
		t.Fatalf("reply = %+v, want the call the model asked for", reply)
	}
}

// The hosted provider is OpenAI's shape, not the home gateway's: messages at the
// top, `strict` so the schema is enforced rather than treated as a hint,
// require_parameters so the router stays on an endpoint that can honour it, and
// reasoning left on because the model is a reasoning one.
func TestGatewayOpenRouterEnvelope(t *testing.T) {
	var got openRouterRequest
	gateway := modelServer(t, Settings{Provider: ProviderOpenRouter, Model: "hosted/model"}, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write(openRouterAnswer(t, "stop",
			`{"kind":"records","text":"Buy glue","memories":[],"zone":"Asia/Singapore",`+
				`"day":"tomorrow","time":"18:00","recurrence":"none","evidence":"tomorrow at 6pm","question":""}`))
	})

	turn, err := gateway.Finalize(context.Background(), PlannerInput{
		Conversation: []store.Message{{Role: "user", Text: "buy glue tomorrow at 6pm", TZ: "Asia/Singapore"}},
		CurrentTZ:    "Asia/Singapore",
		Now:          pinned,
	}, nil)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got.Model != "hosted/model" || len(got.Messages) != 2 {
		t.Fatalf("request = %+v, want the model and the two messages", got)
	}
	if !got.ResponseFormat.JSONSchema.Strict {
		t.Fatal("strict = false, want the schema enforced")
	}
	if got.Provider == nil || !got.Provider.RequireParameters {
		t.Fatalf("provider = %+v, want require_parameters", got.Provider)
	}
	if got.Reasoning == nil || !got.Reasoning.Enabled {
		t.Fatalf("reasoning = %+v, want it enabled", got.Reasoning)
	}
	if got.MaxTokens < 2000 {
		t.Fatalf("max_tokens = %d, want headroom for reasoning", got.MaxTokens)
	}
	if turn.Kind != AnswerRecords || len(turn.Records) != 1 || turn.Records[0].Time != "18:00" {
		t.Fatalf("turn = %+v, want the records answer off the OpenAI envelope", turn)
	}
}

// The screen is the same transport with its own prompt and schema, and its answer is read
// only after the enum is validated.
func TestGatewayScreen(t *testing.T) {
	var names []string
	gateway := modelServer(t, Settings{Model: "local-model"}, func(w http.ResponseWriter, r *http.Request) {
		var body gatewayRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		name := body.Payload.ResponseFormat.JSONSchema.Name
		names = append(names, name)
		switch name {
		case "prescreen":
			if !strings.HasSuffix(body.Payload.Messages[1].Content, "capture: buy glue at 6pm") {
				t.Errorf("screen turn = %q", body.Payload.Messages[1].Content)
			}
			_, _ = w.Write(gatewayAnswer(t, "stop", `{"verdict":"ok","reason":"a thing to do"}`))
		default:
			t.Errorf("unexpected schema %q", name)
		}
	})

	screen, err := gateway.Prescreen(context.Background(), "buy glue at 6pm")
	if err != nil {
		t.Fatalf("Prescreen: %v", err)
	}
	if screen.Outcome != "ok" || screen.Reason != "a thing to do" {
		t.Fatalf("screen = %+v", screen)
	}
	if !reflect.DeepEqual(names, []string{"prescreen"}) {
		t.Fatalf("schemas = %v", names)
	}
}

// A malformed answer is refused rather than half-read, and the finish reason rides
// along for the Trace.
func TestGatewayRefusesAnUnreadableAnswer(t *testing.T) {
	gateway := modelServer(t, Settings{Model: "local-model"}, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(gatewayAnswer(t, "length", `{"kind":"records"`))
	})

	_, err := gateway.Finalize(context.Background(), PlannerInput{
		Conversation: []store.Message{{Role: "user", Text: "buy glue"}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "finish length") {
		t.Fatalf("Finalize error = %v, want the schema refusal and the finish reason", err)
	}
}

// answerFrom is the whole shape decision: which kind it is, what that kind proposed, and the
// strictness the decoder cannot give us — a question filled no record and a record did not also
// ask. The day is the one field the server reads rather than copies.
func TestTurnOf(t *testing.T) {
	cases := []struct {
		name   string
		parsed plannerAnswer
		kind   string
		want   []Record
	}{
		{"a resolved reminder", plannerAnswer{Kind: AnswerRecords, Text: " Call mum ", Zone: " Asia/Singapore ",
			Day: "tomorrow", Time: "19:00", Recurrence: "none", Evidence: " they said 7pm "},
			AnswerRecords, []Record{{Kind: KindReminder, Text: "Call mum", Zone: "Asia/Singapore", Day: "tomorrow",
				Date: "2026-10-25", Time: "19:00", Recurrence: RecurrenceNone, Evidence: "they said 7pm"}}},
		{"a repeat", plannerAnswer{Kind: AnswerRecords, Text: "Trash", Zone: "Asia/Singapore", Day: "this:tuesday",
			Time: "19:00", Recurrence: "weekly"},
			AnswerRecords, []Record{{Kind: KindReminder, Text: "Trash", Zone: "Asia/Singapore", Day: "this:tuesday",
				Date: "2026-10-27", Time: "19:00", Recurrence: store.FrequencyWeekly}}},
		{"a day count", plannerAnswer{Kind: AnswerRecords, Text: "Renew the pass", Zone: "Asia/Singapore", Day: "in:13d",
			Time: "09:00", Recurrence: "none"},
			AnswerRecords, []Record{{Kind: KindReminder, Text: "Renew the pass", Zone: "Asia/Singapore", Day: "in:13d",
				Date: "2026-11-06", Time: "09:00", Recurrence: RecurrenceNone}}},
		{"an unanswered pattern", plannerAnswer{Kind: AnswerRecords, Text: "Buy glue"},
			AnswerRecords, []Record{{Kind: KindReminder, Text: "Buy glue", Recurrence: RecurrenceNone}}},
		{"a token the server cannot read", plannerAnswer{Kind: AnswerRecords, Text: "Buy glue", Zone: "Asia/Singapore",
			Day: "when I'm home", Time: "18:00", Recurrence: "none"},
			AnswerRecords, []Record{{Kind: KindReminder, Text: "Buy glue", Zone: "Asia/Singapore",
				Day: "when I'm home", Time: "18:00", Recurrence: RecurrenceNone}}},
		{"a fact alone", plannerAnswer{Kind: AnswerRecords, Memories: []string{" Milk is cheaper at store A "}},
			AnswerRecords, []Record{{Kind: KindMemory, Text: "Milk is cheaper at store A"}}},
		{"an event and a fact", plannerAnswer{Kind: AnswerRecords, Text: "Lunch at Ikea", Memories: []string{"I have an Ikea voucher"},
			Zone: "Asia/Singapore", Day: "this:friday", Time: "13:00", Recurrence: "none"},
			AnswerRecords, []Record{
				{Kind: KindReminder, Text: "Lunch at Ikea", Zone: "Asia/Singapore", Day: "this:friday",
					Date: "2026-10-30", Time: "13:00", Recurrence: RecurrenceNone},
				{Kind: KindMemory, Text: "I have an Ikea voucher"},
			}},
		{"no event, no record", plannerAnswer{Kind: AnswerRecords, Zone: "Asia/Singapore", Day: "tomorrow"}, AnswerRecords, nil},
		{"blanks read as nothing", plannerAnswer{Kind: AnswerRecords, Text: "   ", Memories: []string{"  "}}, AnswerRecords, nil},
		// A question asks and nothing else: recurrence carries the schema's own default here,
		// which is not something the model proposed.
		{"a question", plannerAnswer{Kind: AnswerQuestion, Recurrence: RecurrenceNone, Question: " When? "},
			AnswerQuestion, nil},
		// A kind the server does not know proposes nothing, and the linter is what names it.
		{"a kind that is neither", plannerAnswer{Kind: "cancel", Text: "Buy glue"}, "cancel", nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			turn, err := answerFrom(test.parsed, pinned)
			if err != nil {
				t.Fatalf("answerFrom: %v", err)
			}
			if turn.Kind != test.kind {
				t.Fatalf("kind = %q, want %q", turn.Kind, test.kind)
			}
			if len(turn.Records) != len(test.want) || (len(test.want) > 0 && !reflect.DeepEqual(turn.Records, test.want)) {
				t.Fatalf("records = %+v, want %+v", turn.Records, test.want)
			}
			if test.kind == AnswerQuestion && turn.Question != "When?" {
				t.Fatalf("question = %q, want it kept", turn.Question)
			}
		})
	}
}

// A day token is the user's calendar, not the server's: at 00:30 in Singapore the
// server's UTC clock still reads the previous day, and "today" must not slip back one.
func TestAnswerFromResolvesTheDayInTheRecordZone(t *testing.T) {
	turn, err := answerFrom(plannerAnswer{Kind: AnswerRecords, Text: "Buy glue",
		Zone: zone, Day: "today", Time: "09:00", Recurrence: RecurrenceNone},
		time.Date(2026, 10, 24, 16, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("answerFrom: %v", err)
	}
	if got := turn.Records[0].Date; got != "2026-10-25" {
		t.Fatalf("date = %q, want the user's own day", got)
	}
}

// The core prompt's context block is the clock, recall and the capture last. It no
// longer names missing fields: the model names its own gap in the answer.
func TestContextBlockCarriesTheClockAndCapture(t *testing.T) {
	block := ContextBlock(PlannerInput{
		CurrentTZ: "Asia/Singapore",
		Now:       pinned,
	}, "buy glue when I'm home")

	for _, want := range []string{
		"current_tz: Asia/Singapore",
		"now_local: 2026-10-24T18:30",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block = %q, want %q", block, want)
		}
	}
	if !strings.HasSuffix(block, "capture: buy glue when I'm home") {
		t.Fatalf("block = %q, want the capture last", block)
	}
	if strings.Contains(block, "missing:") {
		t.Fatalf("block = %q, want no missing-fields line in the planner turn", block)
	}
	if strings.Contains(block, "memories:") {
		t.Fatalf("block = %q, want no section when recall found nothing", block)
	}
}
