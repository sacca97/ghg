package auth

import (
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/models"
)

// NewBackend builds the adapter selected by a resolved provider profile.
func NewBackend(resolved models.Resolved, key, modelAPI string, maxRetries int) (models.Backend, error) {
	if resolved.RequiresAPIKey() && strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("no API key for provider %q (set apiKey/apiKeyEnv in ~/.ghg/config.json)", resolved.Name)
	}
	opts := models.BackendOptions{APIKey: key, MaxRetries: maxRetries}
	if modelAPI = strings.TrimSpace(modelAPI); modelAPI != "" {
		opts.ProtocolOverride = models.Protocol(modelAPI)
	}
	if resolved.RequiresOAuth() {
		opts.Authorizer = DefaultCodexCredentialManager()
	}
	return models.NewBackend(resolved, opts)
}
