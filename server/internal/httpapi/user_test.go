package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"blurt/internal/agent"
	"blurt/internal/auth"
	"blurt/internal/store"
)

// modelSetupUser signs one user in and keeps the handle, because these tests read the
// row the endpoint wrote.
func modelSetupUser(t *testing.T) (*http.Client, string, *store.UserStore, store.User) {
	t.Helper()
	mailer := &fakeMailer{}
	pins := auth.NewPinStore(5*time.Minute, "")
	client, base, handle, _ := newTestStack(t, pins, mailer, nil)
	user := login(t, client, base, "lam@example.com", mailer)
	return client, base, store.NewUserStore(handle.Write, handle.Read), user
}

// TestModelSetupStoresTheSealedKey is the setup page's write half. Until this endpoint
// existed the model columns could only be read, so UserModels refused every Job before
// any stage ran — no screen, no plan, no check, no tags, and no model traffic at all.
func TestModelSetupStoresTheSealedKey(t *testing.T) {
	client, base, users, user := modelSetupUser(t)

	status, body := call(t, client, http.MethodPost, base+"/api/user/model", map[string]any{
		"provider": "OpenRouter",
		"model":    "liquid/lfm-2.5-24b-a2b",
		"apiKey":   "sk-or-test",
	})
	if status != http.StatusOK {
		t.Fatalf("save status = %d: %s", status, body)
	}

	stored, err := users.ModelSettings(context.Background(), user.UserID)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if stored.Provider != "openrouter" || stored.Model != "liquid/lfm-2.5-24b-a2b" {
		t.Fatalf("settings = %+v, want the posted provider and model", stored)
	}
	// A hosted provider with no endpoint named gets the chat route the transport speaks.
	if stored.Endpoint != openRouterChatURL {
		t.Fatalf("endpoint = %q, want %q", stored.Endpoint, openRouterChatURL)
	}
	if stored.APIKey == "" || stored.APIKey == "sk-or-test" {
		t.Fatalf("key = %q, want it sealed at rest", stored.APIKey)
	}

	// The loop's own resolver builds the transport from the same row, which is the
	// proof the seal opens: a bad key fails here exactly as it would in a Job.
	model, name, err := agent.UserModels{Users: users, Key: testModelKey()}.
		ModelFor(context.Background(), user.UserID)
	if err != nil {
		t.Fatalf("resolve model: %v", err)
	}
	if model == nil || name != "liquid/lfm-2.5-24b-a2b" {
		t.Fatalf("resolved %v named %q", model, name)
	}

	// The app reads back "setup is done" rather than sending a capture into the fallback.
	status, body = call(t, client, http.MethodGet, base+"/api/user", nil)
	if status != http.StatusOK {
		t.Fatalf("get user status = %d: %s", status, body)
	}
	var envelope struct {
		ModelConfigured bool `json:"modelConfigured"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode user: %v", err)
	}
	if !envelope.ModelConfigured {
		t.Fatal("modelConfigured = false, want true after setup")
	}

	// Changing the model without re-entering the key keeps the sealed one.
	status, body = call(t, client, http.MethodPost, base+"/api/user/model", map[string]any{
		"provider": "openrouter", "model": "other/model",
	})
	if status != http.StatusOK {
		t.Fatalf("model-only save status = %d: %s", status, body)
	}
	kept, err := users.ModelSettings(context.Background(), user.UserID)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if kept.APIKey != stored.APIKey || kept.Model != "other/model" {
		t.Fatalf("settings = %+v, want the new model and the stored key", kept)
	}
}

// TestModelSetupRejectsIncompleteSettings pins the four things UserModels needs, so a
// half-filled setup page fails at the form rather than on a user's capture.
func TestModelSetupRejectsIncompleteSettings(t *testing.T) {
	client, base, _, _ := modelSetupUser(t)

	for name, body := range map[string]map[string]any{
		"no key":            {"provider": "openrouter", "model": "m"},
		"no model":          {"provider": "openrouter", "apiKey": "k"},
		"unknown provider":  {"provider": "anthropic", "model": "m", "apiKey": "k"},
		"non-http endpoint": {"model": "m", "endpoint": "ftp://host/x", "apiKey": "k"},
		"gateway no route":  {"provider": "gateway", "model": "m", "apiKey": "k"},
	} {
		t.Run(name, func(t *testing.T) {
			status, reply := call(t, client, http.MethodPost, base+"/api/user/model", body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", status, reply)
			}
		})
	}

	// Nothing was stored, so the app still routes to setup.
	status, reply := call(t, client, http.MethodGet, base+"/api/user", nil)
	if status != http.StatusOK {
		t.Fatalf("get user status = %d: %s", status, reply)
	}
	var envelope struct {
		ModelConfigured bool `json:"modelConfigured"`
	}
	if err := json.Unmarshal(reply, &envelope); err != nil {
		t.Fatalf("decode user: %v", err)
	}
	if envelope.ModelConfigured {
		t.Fatal("modelConfigured = true, want false when every post was rejected")
	}
}

// openRouterChatURL mirrors the server's default; a wrong value here is a wrong default
// at runtime, so the test names it rather than trusting the same constant.
const openRouterChatURL = "https://openrouter.ai/api/v1/chat/completions"
