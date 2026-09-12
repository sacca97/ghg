package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

var (
	codexOAuthAuthURL  = "https://auth.openai.com/oauth/authorize"
	codexOAuthTokenURL = "https://auth.openai.com/oauth/token"
	codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthPort     = 1455
)

// CodexSubscriptionCredentials stores the managed ChatGPT OAuth tokens for ghg.
type CodexSubscriptionCredentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// CodexCredentialManager provides thread-safe, cross-process safe access to
// codex-subscription credentials and satisfies models.RequestAuthorizer.
type CodexCredentialManager interface {
	Credentials(ctx context.Context) (accessToken, accountID string, err error)
	ForceRefresh(ctx context.Context) error
	Authorize(req *http.Request) error
	Status(ctx context.Context) (CredentialStatus, error)
	Logout(ctx context.Context) error
}

type codexCredentialManager struct {
	mu      sync.Mutex
	authDir string
	client  *http.Client
}

// DefaultCodexCredentialManager creates a manager using ~/.ghg/auth.
func DefaultCodexCredentialManager() CodexCredentialManager {
	return NewCodexCredentialManager("")
}

// NewCodexCredentialManager creates a manager rooted in the given directory.
func NewCodexCredentialManager(dir string) CodexCredentialManager {
	return &codexCredentialManager{
		authDir: dir,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (m *codexCredentialManager) dir() (string, error) {
	if m.authDir != "" {
		return m.authDir, nil
	}
	cfgDir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "auth"), nil
}

func (m *codexCredentialManager) credsPath() (string, error) {
	d, err := m.dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "codex-subscription.json"), nil
}

func (m *codexCredentialManager) load() (*CodexSubscriptionCredentials, error) {
	path, err := m.credsPath()
	if err != nil {
		return nil, err
	}
	var creds CodexSubscriptionCredentials
	ok, err := loadJSON(path, &creds)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &creds, nil
}

func (m *codexCredentialManager) save(creds *CodexSubscriptionCredentials) error {
	target, err := m.credsPath()
	if err != nil {
		return err
	}
	return saveJSON(target, creds)
}

func (m *codexCredentialManager) acquireFileLock() (*os.File, error) {
	dir, err := m.dir()
	if err != nil {
		return nil, err
	}
	return acquireOAuthFileLock(dir, filepath.Join(dir, "codex-subscription.lock"))
}

func (m *codexCredentialManager) releaseFileLock(f *os.File) {
	if f == nil {
		return
	}
	releaseOAuthFileLock(f)
}

func (m *codexCredentialManager) Credentials(ctx context.Context) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	creds, err := m.load()
	if err != nil {
		return "", "", err
	}
	if creds == nil || creds.AccessToken == "" {
		return "", "", errors.New("codex-subscription is not configured; run 'ghg auth codex-subscription' to log in")
	}

	if time.Until(creds.ExpiresAt) > 5*time.Minute {
		return creds.AccessToken, creds.AccountID, nil
	}

	refreshed, err := m.refreshWithLock(ctx, creds)
	if err != nil {
		return "", "", err
	}
	return refreshed.AccessToken, refreshed.AccountID, nil
}

func (m *codexCredentialManager) refreshWithLock(ctx context.Context, existing *CodexSubscriptionCredentials) (*CodexSubscriptionCredentials, error) {
	lockF, err := m.acquireFileLock()
	if err != nil {
		return nil, err
	}
	defer m.releaseFileLock(lockF)

	reloaded, err := m.load()
	if err == nil && reloaded != nil && time.Until(reloaded.ExpiresAt) > 5*time.Minute {
		return reloaded, nil
	}

	refreshTok := existing.RefreshToken
	accountID := existing.AccountID
	if reloaded != nil && reloaded.RefreshToken != "" {
		refreshTok = reloaded.RefreshToken
		accountID = reloaded.AccountID
	}
	if refreshTok == "" {
		return nil, errors.New("no refresh token available; run 'ghg auth codex-subscription' to sign in again")
	}

	return m.doRefreshToken(ctx, refreshTok, accountID)
}

func (m *codexCredentialManager) ForceRefresh(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	creds, err := m.load()
	if err != nil {
		return err
	}
	if creds == nil || creds.RefreshToken == "" {
		return errors.New("no credentials available to refresh; run 'ghg auth codex-subscription' to sign in again")
	}

	lockF, err := m.acquireFileLock()
	if err != nil {
		return err
	}
	defer m.releaseFileLock(lockF)

	_, err = m.doRefreshToken(ctx, creds.RefreshToken, creds.AccountID)
	return err
}

func (m *codexCredentialManager) doRefreshToken(ctx context.Context, currentRefreshToken, accountID string) (*CodexSubscriptionCredentials, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {codexOAuthClientID},
		"refresh_token": {currentRefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := m.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh network error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		_ = m.Logout(ctx)
		return nil, errors.New("ChatGPT session expired or revoked; please run 'ghg auth codex-subscription' to sign in again")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token refresh returned HTTP status %d", resp.StatusCode)
	}

	var tokResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokResp); err != nil {
		return nil, errors.New("malformed token response from server")
	}
	newRefresh := tokResp.RefreshToken
	if newRefresh == "" {
		newRefresh = currentRefreshToken
	}
	newAccountID := extractAccountID(tokResp.IDToken, tokResp.AccessToken)
	if newAccountID == "" {
		newAccountID = accountID
	}
	expiresIn := tokResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	creds := &CodexSubscriptionCredentials{
		AccessToken:  tokResp.AccessToken,
		RefreshToken: newRefresh,
		AccountID:    newAccountID,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	if err := m.save(creds); err != nil {
		return nil, err
	}
	return creds, nil
}

func (m *codexCredentialManager) Authorize(req *http.Request) error {
	token, accountID, err := m.Credentials(req.Context())
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "ghg/1.0 (terminal)")
	}
	if req.Header.Get("originator") == "" {
		req.Header.Set("originator", "ghg")
	}
	return nil
}

func (m *codexCredentialManager) Status(_ context.Context) (CredentialStatus, error) {
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
		AccountID:  creds.AccountID,
		ExpiresAt:  creds.ExpiresAt,
		Expired:    time.Now().After(creds.ExpiresAt),
	}, nil
}

func (m *codexCredentialManager) Logout(_ context.Context) error {
	path, err := m.credsPath()
	if err != nil {
		return err
	}
	_ = os.Remove(path)
	return nil
}

// Login runs the PKCE OAuth authorization code flow.
func Login(ctx context.Context, opts LoginOptions) (*CodexSubscriptionCredentials, error) {
	pkce, err := generatePKCE()
	if err != nil {
		return nil, err
	}

	port := codexOAuthPort
	if opts.Port > 0 {
		port = opts.Port
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen on 127.0.0.1:%d for OAuth callback: %w", port, err)
	}

	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {codexOAuthClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid profile email offline_access"},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {"S256"},
		"state":                 {pkce.State},
	}
	authURL := codexOAuthAuthURL + "?" + params.Encode()

	if opts.Printer != nil {
		opts.Printer(authURL)
	}
	if opts.OpenBrowser {
		_ = openBrowser(authURL)
	}

	server, codeChan, errChan := startOAuthCallback(listener, "/auth/callback", pkce.State)
	defer stopOAuthCallback(server, listener)

	var code string
	select {
	case code = <-codeChan:
	case err := <-errChan:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tokenData := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexOAuthClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {pkce.Verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(tokenData.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("code exchange request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("authorization code exchange failed (HTTP %d)", resp.StatusCode)
	}

	var tokResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokResp); err != nil {
		return nil, errors.New("malformed token exchange response")
	}

	expiresIn := tokResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	accountID := extractAccountID(tokResp.IDToken, tokResp.AccessToken)
	creds := &CodexSubscriptionCredentials{
		AccessToken:  tokResp.AccessToken,
		RefreshToken: tokResp.RefreshToken,
		AccountID:    accountID,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
	}

	mgr := NewCodexCredentialManager(opts.AuthDir)
	if cm, ok := mgr.(*codexCredentialManager); ok {
		if err := cm.save(creds); err != nil {
			return nil, err
		}
	}

	return creds, nil
}

func extractAccountID(idToken, accessToken string) string {
	for _, tok := range []string{idToken, accessToken} {
		if tok == "" {
			continue
		}
		parts := strings.Split(tok, ".")
		if len(parts) < 2 {
			continue
		}
		payloadBytes, err := decodeJWTPayload(parts[1])
		if err != nil {
			continue
		}
		var claims struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			AccountID        string `json:"account_id"`
			Auth             struct {
				ChatGPTAccountID string `json:"chatgpt_account_id"`
			} `json:"https://api.openai.com/auth"`
			Organizations []struct {
				ID string `json:"id"`
			} `json:"organizations"`
		}
		if err := json.Unmarshal(payloadBytes, &claims); err != nil {
			continue
		}
		if claims.ChatGPTAccountID != "" {
			return claims.ChatGPTAccountID
		}
		if claims.Auth.ChatGPTAccountID != "" {
			return claims.Auth.ChatGPTAccountID
		}
		if claims.AccountID != "" {
			return claims.AccountID
		}
		if len(claims.Organizations) > 0 && claims.Organizations[0].ID != "" {
			return claims.Organizations[0].ID
		}
	}
	return ""
}

func decodeJWTPayload(segment string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return b, nil
	}
	pad := len(segment) % 4
	if pad > 0 {
		segment += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(segment)
}
