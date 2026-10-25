package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"blurt/internal/config"
)

// The answer is one flat string, so the server normalizes it: trimmed,
// lowercased, deduped, and capped. A model told to always fill a field will
// sometimes fill it with junk, and none of it may reach the index.
func TestParseTagsNormalizesTheAnswer(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"a clean list", "travel, flight, airline", []string{"travel", "flight", "airline"}},
		{"mess and case", `  Travel ,, "FLIGHT"., flight `, []string{"travel", "flight"}},
		{"internal spaces collapse", "air  asia,  trip", []string{"air asia", "trip"}},
		{"nothing", " , ,, ", nil},
		{"capped at ten",
			"one, two, three, four, five, six, seven, eight, nine, ten, eleven, twelve",
			[]string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := parseTags(test.raw)
			if len(got) == 0 && len(test.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseTags(%q) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

// Tags is a second, single-purpose call: the tags prompt and its own schema, the
// fact as the whole input, and the same one-field answer shape the other prompts
// use. It must not touch the records schema, which is measured.
func TestGatewayTagsSendsTheTagsSchema(t *testing.T) {
	var got inferRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write(gatewayAnswer(t, "stop", `{"tags":"travel, flight, airline"}`))
	}))
	defer server.Close()

	const fact = "I have AirAsia points to use by December"
	gateway := NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	tags, err := gateway.Tags(context.Background(), fact)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(got.Payload.Messages) != 2 || got.Payload.Messages[0].Content != tagsPrompt ||
		got.Payload.Messages[1].Content != "fact: "+fact {
		t.Fatalf("messages = %+v, want the tags prompt over the fact", got.Payload.Messages)
	}
	if got.Payload.Messages[0].Content == systemPrompt {
		t.Fatal("the records prompt was sent for tags")
	}
	if got.Payload.ResponseFormat.JSONSchema.Name != "tags" {
		t.Fatalf("schema = %q, want the tags schema", got.Payload.ResponseFormat.JSONSchema.Name)
	}
	if want := []string{"travel", "flight", "airline"}; !reflect.DeepEqual(tags, want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
}
