package agent

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"blurt/internal/secret"
	"blurt/internal/store"
)

// UserModels resolves the model a Job's owner configured on the setup page. The
// PRD keeps model settings out of `.env`: one provider, endpoint, key and model per
// user, stored on the User row with the key encrypted at rest. This model plans one
// Job; the screen and the tagger are the home gateway's own two models,
// so a missing setting here never silently falls back to the cheap stages.
type UserModels struct {
	Users *store.UserStore
	// Key opens the stored API key. It is config's encryption key.
	Key []byte
}

// SetupError is a user whose model is not usable: no endpoint, no model, an
// unknown provider, or a key that will not open. A retry cannot fix a setup, so the
// Job fails rather than spending its attempt budget on it.
type SetupError struct {
	Message string
}

func (e *SetupError) Error() string { return "agent: " + e.Message }

// Retryable is false: the same answer would come back until the user finishes the
// setup page.
func (e *SetupError) Retryable() bool { return false }

// ModelFor builds the transport for one user. The model id it returns is what the
// Trace names, not a second transport.
func (u UserModels) ModelFor(ctx context.Context, userID int64) (Model, string, error) {
	settings, err := u.Users.ModelSettings(ctx, userID)
	if err != nil {
		return nil, "", &SetupError{Message: fmt.Sprintf("read model settings for user %d: %v", userID, err)}
	}
	endpoint := strings.TrimSpace(settings.Endpoint)
	model := strings.TrimSpace(settings.Model)
	if endpoint == "" || model == "" {
		return nil, "", &SetupError{Message: fmt.Sprintf("user %d has no model configured", userID)}
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, "", &SetupError{Message: fmt.Sprintf("user %d has an invalid model endpoint", userID)}
	}
	provider := Provider(strings.ToLower(strings.TrimSpace(settings.Provider)))
	switch provider {
	case "", ProviderGateway:
		provider = ProviderGateway
	case ProviderOpenRouter:
	default:
		return nil, "", &SetupError{Message: fmt.Sprintf("user %d has an unknown model provider %q", userID, settings.Provider)}
	}
	apiKey, err := secret.Decrypt(u.Key, settings.APIKey)
	if err != nil {
		return nil, "", &SetupError{Message: fmt.Sprintf("open model key for user %d: %v", userID, err)}
	}
	return NewModelClient(Settings{
		Provider: provider,
		URL:      endpoint,
		APIKey:   apiKey,
		Model:    model,
	}), model, nil
}
