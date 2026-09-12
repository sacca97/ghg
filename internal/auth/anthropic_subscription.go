package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sacca97/ghg/internal/config"
)

// These are the browser OAuth endpoints used by Claude Code for Claude.ai
// subscriptions. They are variables so the token exchange can be tested
// without contacting the real service.
var (
	claudeOAuthAuthURL  = "https://claude.ai/oauth/authorize"
	claudeOAuthTokenURL = "https://console.anthropic.com/v1/oauth/token"
	claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

const (
	claudeOAuthScope = "user:profile user:inference user:sessions:claude_code"
	claudeOAuthBeta  = "oauth-2025-04-20"
)

// ClaudeSubscriptionCredentials stores the managed Claude.ai OAuth tokens for
// ghg. It is deliberately separate from Claude Code's credential store.
type ClaudeSubscriptionCredentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ClaudeCredentialManager provides thread-safe, cross-process safe access to
// Claude subscription credentials and satisfies models.RequestAuthorizer.
type ClaudeCredentialManager interface {
	OAuthCredentialManager
	Credentials(context.Context) (string, error)
}

type claudeCredentialManager struct {
	mu      sync.Mutex
	authDir string
	client  *http.Client
}

// DefaultClaudeCredentialManager creates a manager using ~/.ghg/auth.
func DefaultClaudeCredentialManager() ClaudeCredentialManager {
	return NewClaudeCredentialManager("")
}

// NewClaudeCredentialManager creates a manager rooted in the given directory.
func NewClaudeCredentialManager(dir string) ClaudeCredentialManager {
	return &claudeCredentialManager{
		authDir: dir,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (m *claudeCredentialManager) dir() (string, error) {
	if m.authDir != "" {
		return m.authDir, nil
	}
	cfgDir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "auth"), nil
}

func (m *claudeCredentialManager) credsPath() (string, error) {
	dir, err := m.dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "claude-subscription.json"), nil
}

func (m *claudeCredentialManager) load() (*ClaudeSubscriptionCredentials, error) {
	path, err := m.credsPath()
	if err != nil {
		return nil, err
	}
	var creds ClaudeSubscriptionCredentials
	ok, err := loadJSON(path, &creds)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &creds, nil
}

func (m *claudeCredentialManager) save(creds *ClaudeSubscriptionCredentials) error {
	target, err := m.credsPath()
	if err != nil {
		return err
	}
	return saveJSON(target, creds)
}

func (m *claudeCredentialManager) acquireFileLock() (*os.File, error) {
	dir, err := m.dir()
	if err != nil {
		return nil, err
	}
	return acquireOAuthFileLock(dir, filepath.Join(dir, "claude-subscription.lock"))
}

func (m *claudeCredentialManager) releaseFileLock(f *os.File) {
	if f == nil {
		return
	}
	releaseOAuthFileLock(f)
}

func (m *claudeCredentialManager) Credentials(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	creds, err := m.load()
	if err != nil {
		return "", err
	}
	if creds == nil || creds.AccessToken == "" {
		return "", errors.New("claude-subscription is not configured; run 'ghg auth claude-subscription' to log in")
	}
	if time.Until(creds.ExpiresAt) > 5*time.Minute {
		return creds.AccessToken, nil
	}
	refreshed, err := m.refreshWithLock(ctx, creds)
	if err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

func (m *claudeCredentialManager) refreshWithLock(ctx context.Context, existing *ClaudeSubscriptionCredentials) (*ClaudeSubscriptionCredentials, error) {
	lockF, err := m.acquireFileLock()
	if err != nil {
		return nil, err
	}
	defer m.releaseFileLock(lockF)

	reloaded, err := m.load()
	if err != nil {
		return nil, err
	}
	if reloaded != nil && time.Until(reloaded.ExpiresAt) > 5*time.Minute {
		return reloaded, nil
	}
	refreshToken := existing.RefreshToken
	if reloaded != nil && reloaded.RefreshToken != "" {
		refreshToken = reloaded.RefreshToken
	}
	if refreshToken == "" {
		return nil, errors.New("no refresh token available; run 'ghg auth claude-subscription' to sign in again")
	}
	return m.doRefreshToken(ctx, refreshToken)
}

func (m *claudeCredentialManager) ForceRefresh(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	creds, err := m.load()
	if err != nil {
		return err
	}
	if creds == nil || creds.RefreshToken == "" {
		return errors.New("no credentials available to refresh; run 'ghg auth claude-subscription' to sign in again")
	}
	lockF, err := m.acquireFileLock()
	if err != nil {
		return err
	}
	defer m.releaseFileLock(lockF)
	_, err = m.doRefreshToken(ctx, creds.RefreshToken)
	return err
}

func (m *claudeCredentialManager) doRefreshToken(ctx context.Context, refreshToken string) (*ClaudeSubscriptionCredentials, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     claudeOAuthClientID,
		"scope":         claudeOAuthScope,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeOAuthTokenURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh network error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		_ = m.Logout(ctx)
		return nil, errors.New("claude session expired or revoked; please run 'ghg auth claude-subscription' to sign in again")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("token refresh returned HTTP status %d", resp.StatusCode)
	}
	var tokenResponse struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokenResponse); err != nil {
		return nil, errors.New("malformed token response from server")
	}
	if tokenResponse.AccessToken == "" {
		return nil, errors.New("token response did not contain an access token")
	}
	if tokenResponse.RefreshToken == "" {
		tokenResponse.RefreshToken = refreshToken
	}
	if tokenResponse.ExpiresIn <= 0 {
		tokenResponse.ExpiresIn = 3600
	}
	creds := &ClaudeSubscriptionCredentials{
		AccessToken:  tokenResponse.AccessToken,
		RefreshToken: tokenResponse.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResponse.ExpiresIn) * time.Second),
	}
	if err := m.save(creds); err != nil {
		return nil, err
	}
	return creds, nil
}

func (m *claudeCredentialManager) httpClient() *http.Client {
	if m.client != nil {
		return m.client
	}
	return http.DefaultClient
}

func (m *claudeCredentialManager) Authorize(req *http.Request) error {
	token, err := m.Credentials(req.Context())
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "ghg/1.0 (terminal)")
	}
	return nil
}

func (m *claudeCredentialManager) Status(_ context.Context) (CredentialStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	creds, err := m.load()
	if err != nil {
		return CredentialStatus{}, err
	}
	if creds == nil || creds.AccessToken == "" {
		return CredentialStatus{Configured: false}, nil
	}
	return CredentialStatus{
		Configured: true,
		ExpiresAt:  creds.ExpiresAt,
		Expired:    time.Now().After(creds.ExpiresAt),
	}, nil
}

func (m *claudeCredentialManager) Logout(_ context.Context) error {
	path, err := m.credsPath()
	if err != nil {
		return err
	}
	_ = os.Remove(path)
	return nil
}

// ClaudeLogin runs Claude.ai's browser PKCE authorization code flow.
func ClaudeLogin(ctx context.Context, opts LoginOptions) (*ClaudeSubscriptionCredentials, error) {
	pkce, err := generatePKCE()
	if err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", opts.Port))
	if err != nil {
		return nil, fmt.Errorf("listen for Claude OAuth callback: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", port)
	params := url.Values{
		"code":                  {"true"},
		"client_id":             {claudeOAuthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {claudeOAuthScope},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {"S256"},
		"state":                 {pkce.State},
	}
	authURL := claudeOAuthAuthURL + "?" + params.Encode()
	if opts.Printer != nil {
		opts.Printer(authURL)
	}
	if opts.OpenBrowser {
		_ = openBrowser(authURL)
	}

	server, codeChan, errChan := startOAuthCallback(listener, "/callback", pkce.State)
	defer stopOAuthCallback(server, listener)

	var code string
	select {
	case code = <-codeChan:
	case err := <-errChan:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tokenBody, err := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
		"client_id":     claudeOAuthClientID,
		"code_verifier": pkce.Verifier,
		"state":         pkce.State,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeOAuthTokenURL, strings.NewReader(string(tokenBody)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("code exchange request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("authorization code exchange failed (HTTP %d)", resp.StatusCode)
	}
	var tokenResponse struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokenResponse); err != nil {
		return nil, errors.New("malformed token exchange response")
	}
	if tokenResponse.AccessToken == "" {
		return nil, errors.New("token exchange response did not contain an access token")
	}
	if tokenResponse.ExpiresIn <= 0 {
		tokenResponse.ExpiresIn = 3600
	}
	creds := &ClaudeSubscriptionCredentials{
		AccessToken:  tokenResponse.AccessToken,
		RefreshToken: tokenResponse.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResponse.ExpiresIn) * time.Second),
	}
	manager := NewClaudeCredentialManager(opts.AuthDir).(*claudeCredentialManager)
	if err := manager.save(creds); err != nil {
		return nil, err
	}
	return creds, nil
}
