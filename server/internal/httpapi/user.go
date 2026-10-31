package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"blurt/internal/agent"
	"blurt/internal/secret"
	"blurt/internal/store"
)

// openRouterChatURL is the endpoint a hosted provider gets when the setup post names
// none: the OpenAI-compatible chat route Blurt's transport already speaks.
const openRouterChatURL = "https://openrouter.ai/api/v1/chat/completions"

// getUser returns the cookie identity plus questionnaire progress, which is what the
// app needs on boot, and whether a model is configured — the setup page's cue.
func (s *Server) getUser(w http.ResponseWriter, r *http.Request, user store.User) {
	configured, err := s.modelStatus(r.Context(), user.UserID)
	if err != nil {
		s.fail(w, "read model settings", err)
		return
	}
	writeJSON(w, http.StatusOK, userEnvelope{User: user, ModelConfigured: configured})
}

// modelRequest is one setup post. The key is write-only: it goes in and no endpoint
// ever returns it.
type modelRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"apiKey"`
}

// saveModelSettings is the write half of the setup page. Without it the model columns
// on the User row could only ever be read: UserModels refuses the Job before any stage
// runs, so every capture ends on the fallback sentence with no model traffic at all.
func (s *Server) saveModelSettings(w http.ResponseWriter, r *http.Request, user store.User) {
	if s.Users == nil || len(s.EncryptionKey) != secret.KeyLen {
		writeError(w, http.StatusNotImplemented, "not_configured", "Model setup is not configured.")
		return
	}
	var body modelRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	// The stored key is read before validation because an empty key in the post means
	// "keep the one you have", not "clear it".
	stored, err := s.Users.ModelSettings(r.Context(), user.UserID)
	if err != nil {
		s.fail(w, "read model settings", err)
		return
	}
	settings, err := s.validatedSettings(stored, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	if err := s.Users.SaveModelSettings(r.Context(), user.UserID, settings); err != nil {
		s.fail(w, "save model settings", err)
		return
	}
	writeJSON(w, http.StatusOK, userEnvelope{User: user, ModelConfigured: true})
}

// validatedSettings turns one setup post into the row it would store, sealing the key.
// It is the validation UserModels assumes: a known provider, a model name, an http(s)
// endpoint, and a key that opens at run time.
func (s *Server) validatedSettings(stored store.ModelSettings, body modelRequest) (store.ModelSettings, error) {
	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	endpoint := strings.TrimSpace(body.Endpoint)
	switch provider {
	case "":
		provider = string(agent.ProviderGateway)
	case string(agent.ProviderOpenRouter):
		if endpoint == "" {
			endpoint = openRouterChatURL
		}
	case string(agent.ProviderGateway):
	default:
		return store.ModelSettings{}, fmt.Errorf("provider must be %q or %q", agent.ProviderGateway, agent.ProviderOpenRouter)
	}

	model := strings.TrimSpace(body.Model)
	if model == "" {
		return store.ModelSettings{}, errors.New("a model name is required")
	}
	parsed, err := url.Parse(endpoint)
	if endpoint == "" || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return store.ModelSettings{}, errors.New("an http(s) endpoint is required")
	}

	sealed := stored.APIKey
	if key := strings.TrimSpace(body.APIKey); key != "" {
		if sealed, err = secret.Encrypt(s.EncryptionKey, key); err != nil {
			return store.ModelSettings{}, err
		}
	}
	if sealed == "" {
		return store.ModelSettings{}, errors.New("an API key is required")
	}
	return store.ModelSettings{Provider: provider, Model: model, Endpoint: endpoint, APIKey: sealed}, nil
}

// modelStatus reports whether this user can plan at all, so the app can route to setup
// instead of letting a capture fail on the fallback sentence.
func (s *Server) modelStatus(ctx context.Context, userID int64) (bool, error) {
	if s.Users == nil {
		return false, nil
	}
	settings, err := s.Users.ModelSettings(ctx, userID)
	if err != nil {
		return false, err
	}
	return settings.Model != "" && settings.Endpoint != "" && settings.APIKey != "", nil
}
