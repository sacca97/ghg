package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/sacca97/ghg/internal/models"
)

// OAuthCredentialManager is the common request/status surface shared by
// subscription providers. Provider-specific token formats stay in their
// respective managers.
type OAuthCredentialManager interface {
	ForceRefresh(context.Context) error
	Authorize(*http.Request) error
	Status(context.Context) (CredentialStatus, error)
	Logout(context.Context) error
}

// CredentialManagerFor returns the manager required by a resolved OAuth
// profile.
func CredentialManagerFor(resolved models.Resolved) (OAuthCredentialManager, error) {
	switch resolved.Auth.Kind {
	case models.AuthCodexSubscription:
		return DefaultCodexCredentialManager(), nil
	case models.AuthClaudeSubscription:
		return DefaultClaudeCredentialManager(), nil
	case models.AuthZaiCodingPlan:
		return DefaultZaiCodingPlanCredentialManager(), nil
	default:
		return nil, fmt.Errorf("provider %q does not use subscription OAuth", resolved.Name)
	}
}

// LoginFor runs the browser login for the selected subscription profile and
// returns an account identifier when that provider exposes one.
func LoginFor(ctx context.Context, resolved models.Resolved, opts LoginOptions) (string, error) {
	switch resolved.Auth.Kind {
	case models.AuthCodexSubscription:
		creds, err := Login(ctx, opts)
		if err != nil {
			return "", err
		}
		return creds.AccountID, nil
	case models.AuthClaudeSubscription:
		_, err := ClaudeLogin(ctx, opts)
		return "", err
	case models.AuthZaiCodingPlan:
		creds, err := ZaiCodingPlanLogin(ctx, opts)
		if err != nil {
			return "", err
		}
		return creds.AccountID, nil
	default:
		return "", fmt.Errorf("provider %q does not support subscription OAuth", resolved.Name)
	}
}

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
		manager, err := CredentialManagerFor(resolved)
		if err != nil {
			return nil, err
		}
		opts.Authorizer = manager
	}
	return models.NewBackend(resolved, opts)
}
