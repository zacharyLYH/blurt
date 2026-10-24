package stt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"blurt/internal/config"
)

// The gateway route is not OpenAI's: one JSON body carrying a base64 WAV data
// URI, and the transcript under output.text. This pins that contract, including
// the bearer key and the model id the gateway checks.
func TestHTTPTranscribe(t *testing.T) {
	var gotModel, gotAudio, gotAuth, gotType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		var body inferRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		gotModel, gotAudio = body.Model, body.Payload.Audio
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "req_1", "model": "phonon",
			"output": map[string]string{"text": "  buy glue  "},
		})
	}))
	defer server.Close()

	client := NewHTTP(config.STT{URL: server.URL, APIKey: "secret", Model: "phonon"})
	text, err := client.Transcribe(context.Background(), strings.NewReader("audio bytes"))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "buy glue" {
		t.Fatalf("text = %q", text)
	}
	if gotModel != "phonon" || gotAuth != "Bearer secret" || gotType != "application/json" {
		t.Fatalf("model=%q auth=%q type=%q", gotModel, gotAuth, gotType)
	}
	if want := audioPrefix + base64.StdEncoding.EncodeToString([]byte("audio bytes")); gotAudio != want {
		t.Fatalf("audio = %q, want %q", gotAudio, want)
	}
}

// A refusal arrives as {"error": {"code", "message"}}. The message has to reach
// the worker's trace, and a 401 must stay permanent: no retry can fix a bad key.
func TestHTTPRefusalMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"code": "unauthorized", "message": "missing or invalid api key"}}`))
	}))
	defer server.Close()

	client := NewHTTP(config.STT{URL: server.URL, Model: "phonon"})
	_, err := client.Transcribe(context.Background(), strings.NewReader("audio"))

	var refusal *Error
	if !errors.As(err, &refusal) {
		t.Fatalf("err is not *Error: %v", err)
	}
	if refusal.Status != http.StatusUnauthorized || refusal.Retryable() {
		t.Fatalf("status = %d retryable = %v", refusal.Status, refusal.Retryable())
	}
	if !strings.Contains(err.Error(), "missing or invalid api key") {
		t.Fatalf("err = %v, want the gateway's own message", err)
	}
}

// A 4xx is a permanent refusal (bad key or bad clip); 5xx is worth retrying.
func TestHTTPErrorRetryability(t *testing.T) {
	for _, test := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(test.status)
			_, _ = w.Write([]byte("no"))
		}))
		client := NewHTTP(config.STT{URL: server.URL, Model: "phonon"})
		_, err := client.Transcribe(context.Background(), strings.NewReader("audio"))
		server.Close()

		var refusal *Error
		if !strings.Contains(err.Error(), "stt:") {
			t.Fatalf("status %d err = %v", test.status, err)
		}
		if ok := errors.As(err, &refusal); !ok {
			t.Fatalf("status %d err is not *Error: %v", test.status, err)
		}
		if refusal.Retryable() != test.retryable {
			t.Fatalf("status %d retryable = %v, want %v", test.status, refusal.Retryable(), test.retryable)
		}
	}
}

func TestFakeModes(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		mode string
		text string
		err  bool
	}{
		{"ok", FakeTranscript, false},
		{"flaky", FakeTranscript, false},
		{"error", "", true},
		{"illegible", "", false},
	} {
		fake := NewFake(test.mode)
		if test.mode == "flaky" {
			if _, err := fake.Transcribe(ctx, strings.NewReader("")); err == nil {
				t.Fatal("flaky first call should fail")
			}
		}
		text, err := fake.Transcribe(ctx, strings.NewReader(""))
		if (err != nil) != test.err || text != test.text {
			t.Fatalf("%s: text=%q err=%v", test.mode, text, err)
		}
	}
}
