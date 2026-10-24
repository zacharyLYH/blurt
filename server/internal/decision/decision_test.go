package decision

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"blurt/internal/config"
)

// The route is the same /v1/infer as phonon, but laya's body carries a state and
// named questions and the answers come back typed. This pins the contract: the
// bearer key, the model id, the state, and both answer shapes.
func TestHTTPDecide(t *testing.T) {
	var gotModel, gotState, gotAuth, gotType string
	var gotQuestions Questions
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		gotModel, gotState, gotQuestions = body.Model, body.Payload.State, body.Payload.Questions
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "req_1", "model": "laya",
			"output": map[string]any{"answers": map[string]any{
				"transcript": map[string]any{
					"type": "choice", "value": "request",
					"probabilities": map[string]float64{"request": 0.89, "noise": 0.11},
				},
				"churn": map[string]any{"type": "bool", "value": true, "probability": 0.9654},
			}},
		})
	}))
	defer server.Close()

	client := NewHTTP(config.Decision{URL: server.URL, APIKey: "secret", Model: "laya"})
	answers, err := client.Decide(context.Background(), "Billed twice for March.", Questions{
		"transcript": {Kind: Choice, Instructions: "Which is this transcript?",
			Criteria: map[string]string{"request": "a real request", "noise": "unintelligible noise"}},
		"churn": {Kind: Noul, Instructions: "Does the customer threaten to cancel?"},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if gotModel != "laya" || gotAuth != "Bearer secret" || gotType != "application/json" {
		t.Fatalf("model=%q auth=%q type=%q", gotModel, gotAuth, gotType)
	}
	if gotState != "Billed twice for March." || len(gotQuestions) != 2 {
		t.Fatalf("state=%q questions=%v", gotState, gotQuestions)
	}
	label := answers["transcript"]
	if label.Kind != Choice || label.Choice != "request" || label.Probabilities["request"] != 0.89 {
		t.Fatalf("choice answer = %+v", label)
	}
	// noul is an accepted alias for bool, and the reply comes back as bool.
	verdict := answers["churn"]
	if verdict.Kind != Bool || !verdict.Truth || verdict.Probability != 0.9654 {
		t.Fatalf("bool answer = %+v", verdict)
	}
}

// laya's window is 512 tokens and it truncates silently, so an oversized state
// would push the questions out of the window instead. The cap is the only thing
// standing between a long transcript and a question that never gets read.
func TestDecideTruncatesState(t *testing.T) {
	var gotState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body request
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotState = body.Payload.State
		_, _ = w.Write([]byte(`{"output":{"answers":{"q":{"type":"bool","value":true}}}}`))
	}))
	defer server.Close()

	client := NewHTTP(config.Decision{URL: server.URL, Model: "laya"})
	state := strings.Repeat("a", maxStateChars) + "TAIL"
	if _, err := client.Decide(context.Background(), state, Questions{"q": {Kind: Bool, Instructions: "x"}}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(gotState) != maxStateChars || strings.HasSuffix(gotState, "TAIL") {
		t.Fatalf("state length = %d, want the first %d characters only", len(gotState), maxStateChars)
	}
}

// A question the layer cannot build is refused here instead of coming back as an
// opaque gateway error: no questions at all, or a choice with nothing to choose
// from.
func TestDecideRejectsIncompleteQuestions(t *testing.T) {
	client := NewHTTP(config.Decision{URL: "http://unused.invalid", Model: "laya"})
	if _, err := client.Decide(context.Background(), "x", nil); err == nil {
		t.Fatal("Decide with no questions succeeded")
	}
	_, err := client.Decide(context.Background(), "x", Questions{"dept": {Kind: Choice, Instructions: "Which?"}})
	if err == nil || !strings.Contains(err.Error(), "criteria") {
		t.Fatalf("err = %v, want a criteria complaint", err)
	}
}

// An unsupported kind comes back as 400 upstream_error whose message wraps the
// model's own detail. That detail has to reach the caller, and a 4xx stays
// permanent: no retry can fix a bad kind or a bad key.
func TestHTTPRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream_error","message":"{\"detail\":\"This model does not support 'bogus' decisions\"}"}}`))
	}))
	defer server.Close()

	client := NewHTTP(config.Decision{URL: server.URL, Model: "laya"})
	_, err := client.Decide(context.Background(), "x", Questions{"q": {Kind: Bool, Instructions: "y"}})

	var refusal *Error
	if !errors.As(err, &refusal) {
		t.Fatalf("err is not *Error: %v", err)
	}
	if refusal.Status != http.StatusBadRequest || refusal.Retryable() {
		t.Fatalf("status = %d retryable = %v", refusal.Status, refusal.Retryable())
	}
	if !strings.Contains(err.Error(), "does not support 'bogus'") {
		t.Fatalf("err = %v, want the gateway's own detail", err)
	}
}

// A 4xx is a permanent refusal; 429 and 5xx are worth retrying.
func TestErrorRetryability(t *testing.T) {
	for _, test := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
	} {
		if got := (&Error{Status: test.status}).Retryable(); got != test.retryable {
			t.Fatalf("status %d retryable = %v, want %v", test.status, got, test.retryable)
		}
	}
}

// An answer type this layer does not know is a decode error, never a silent zero
// value that reads as "no".
func TestDecideRejectsUnknownAnswerType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"output":{"answers":{"q":{"type":"label","value":"x"}}}}`))
	}))
	defer server.Close()

	client := NewHTTP(config.Decision{URL: server.URL, Model: "laya"})
	_, err := client.Decide(context.Background(), "x", Questions{"q": {Kind: Bool, Instructions: "y"}})
	if err == nil || !strings.Contains(err.Error(), "unexpected type") {
		t.Fatalf("err = %v, want an unexpected type complaint", err)
	}
}
