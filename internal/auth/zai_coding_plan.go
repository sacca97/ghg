package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sacca97/ghg/internal/config"
)

// Z.AI's Coding Plan browser login is the ZCode flow: OAuth produces a
// short-lived token, then Z.AI provisions a durable API key for the account.
// The API key is what ghg uses for model requests.
var (
	zaiOAuthAuthorizeURL = "https://chat.z.ai/api/oauth/authorize"
	zaiOAuthTokenURL     = "https://zcode.z.ai/api/v1/oauth/token"
	zaiBusinessBaseURL   = "https://api.z.ai"
	zaiBusinessLoginURL  = "https://api.z.ai/api/auth/z/login"
	zaiOAuthClientID     = "client_P8X5CMWmlaRO9gyO-KSqtg"
	zaiHTTPClient        = http.DefaultClient
)

const (
	zaiOAuthRedirectURI = "zcode://zai-auth/callback"
	zaiAPIKeyName       = "ghg"
)

// ZaiCodingPlanCredentials stores the durable API key minted by the browser
// login. It is separate from manually configured Z.AI API keys.
type ZaiCodingPlanCredentials struct {
	APIKey    string    `json:"api_key"`
	AccountID string    `json:"account_id,omitempty"`
	Email     string    `json:"email,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ZaiCodingPlanCredentialManager provides the request/status surface for the
// durable key produced by the ZCode login flow.
type ZaiCodingPlanCredentialManager interface {
	OAuthCredentialManager
	Credentials(context.Context) (string, error)
}

type zaiCodingPlanCredentialManager struct {
	mu      sync.Mutex
	authDir string
}

func DefaultZaiCodingPlanCredentialManager() ZaiCodingPlanCredentialManager {
	return NewZaiCodingPlanCredentialManager("")
}

func NewZaiCodingPlanCredentialManager(dir string) ZaiCodingPlanCredentialManager {
	return &zaiCodingPlanCredentialManager{authDir: dir}
}

func (m *zaiCodingPlanCredentialManager) dir() (string, error) {
	if m.authDir != "" {
		return m.authDir, nil
	}
	cfgDir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "auth"), nil
}

func (m *zaiCodingPlanCredentialManager) path() (string, error) {
	dir, err := m.dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "zai-coding-plan.json"), nil
}

func (m *zaiCodingPlanCredentialManager) load() (*ZaiCodingPlanCredentials, error) {
	path, err := m.path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var creds ZaiCodingPlanCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("malformed credentials in %s: %w", path, err)
	}
	return &creds, nil
}

func (m *zaiCodingPlanCredentialManager) save(creds *ZaiCodingPlanCredentials) error {
	dir, err := m.dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create auth dir: %w", err)
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	target, err := m.path()
	if err != nil {
		return err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp creds: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename creds: %w", err)
	}
	return nil
}

func (m *zaiCodingPlanCredentialManager) Credentials(_ context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	creds, err := m.load()
	if err != nil {
		return "", err
	}
	if creds == nil || creds.APIKey == "" {
		return "", errors.New("zai-coding-plan is not configured; run 'ghg auth zai-coding-plan' to log in")
	}
	return creds.APIKey, nil
}

// ForceRefresh cannot refresh a Z.AI Coding Plan key: the browser flow mints
// a durable key. Re-run browser login if Z.AI revokes it.
func (m *zaiCodingPlanCredentialManager) ForceRefresh(context.Context) error {
	return errors.New("Z.AI Coding Plan key cannot be refreshed; run 'ghg auth zai-coding-plan' to sign in again")
}

func (m *zaiCodingPlanCredentialManager) Authorize(req *http.Request) error {
	key, err := m.Credentials(req.Context())
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "ghg/1.0 (terminal)")
	}
	return nil
}

func (m *zaiCodingPlanCredentialManager) Status(_ context.Context) (CredentialStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	creds, err := m.load()
	if err != nil {
		return CredentialStatus{}, err
	}
	return CredentialStatus{Configured: creds != nil && creds.APIKey != ""}, nil
}

func (m *zaiCodingPlanCredentialManager) Logout(_ context.Context) error {
	path, err := m.path()
	if err != nil {
		return err
	}
	_ = os.Remove(path)
	return nil
}

func zaiClient() *http.Client {
	if zaiHTTPClient != nil {
		return zaiHTTPClient
	}
	return http.DefaultClient
}

func zaiRequest(ctx context.Context, method, endpoint, operation string, body any, headers map[string]string) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := zaiClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("Z.AI %s request failed: %w", operation, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("Z.AI %s response read failed: %w", operation, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("Z.AI %s returned HTTP %d", operation, resp.StatusCode)
	}
	return zaiData(data, operation)
}

func zaiData(body []byte, operation string) (json.RawMessage, error) {
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Success *bool           `json:"success"`
		Message string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("Z.AI %s returned malformed JSON", operation)
	}
	if len(envelope.Code) == 0 && envelope.Success == nil {
		return body, nil
	}
	if envelope.Success != nil && !*envelope.Success {
		return nil, fmt.Errorf("Z.AI %s failed: %s", operation, envelope.Message)
	}
	code := strings.Trim(string(envelope.Code), `"`)
	if code != "" && code != "0" && code != "200" {
		return nil, fmt.Errorf("Z.AI %s failed: %s", operation, envelope.Message)
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		return envelope.Data, nil
	}
	return body, nil
}

func zaiBusinessLogin(ctx context.Context, oauthToken string) (string, error) {
	data, err := zaiRequest(ctx, http.MethodPost, zaiBusinessLoginURL, "business login", map[string]string{"token": oauthToken}, nil)
	if err != nil {
		return "", err
	}
	var result struct {
		AccessToken string `json:"access_token"`
		Access      string `json:"accessToken"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("Z.AI business login returned malformed data")
	}
	if result.AccessToken == "" {
		result.AccessToken = result.Access
	}
	if result.AccessToken == "" {
		return "", errors.New("Z.AI business login returned no access token")
	}
	return result.AccessToken, nil
}

type zaiOrganization struct {
	OrganizationID string       `json:"organizationId"`
	IsDefault      bool         `json:"isDefault"`
	Projects       []zaiProject `json:"projects"`
}

type zaiProject struct {
	ProjectID string `json:"projectId"`
	IsDefault bool   `json:"isDefault"`
}

type zaiAPIKey struct {
	Name   string `json:"name"`
	APIKey string `json:"apiKey"`
}

func zaiKeyList(data json.RawMessage) []zaiAPIKey {
	var list []zaiAPIKey
	if json.Unmarshal(data, &list) == nil {
		return list
	}
	var wrapped struct {
		List    []zaiAPIKey `json:"list"`
		Keys    []zaiAPIKey `json:"keys"`
		APIKeys []zaiAPIKey `json:"apiKeys"`
		Records []zaiAPIKey `json:"records"`
	}
	if json.Unmarshal(data, &wrapped) != nil {
		return nil
	}
	for _, candidate := range [][]zaiAPIKey{wrapped.List, wrapped.Keys, wrapped.APIKeys, wrapped.Records} {
		if candidate != nil {
			return candidate
		}
	}
	return nil
}

func zaiMintAPIKey(ctx context.Context, oauthToken string) (string, error) {
	bizToken, err := zaiBusinessLogin(ctx, oauthToken)
	if err != nil {
		return "", err
	}
	authHeaders := map[string]string{"Authorization": "Bearer " + bizToken}
	customerData, err := zaiRequest(ctx, http.MethodGet, zaiBusinessBaseURL+"/api/biz/customer/getCustomerInfo", "customer lookup", nil, authHeaders)
	if err != nil {
		return "", err
	}
	var customer struct {
		Organizations []zaiOrganization `json:"organizations"`
	}
	if err := json.Unmarshal(customerData, &customer); err != nil {
		return "", errors.New("Z.AI customer lookup returned malformed data")
	}
	if len(customer.Organizations) == 0 {
		return "", errors.New("Z.AI key provisioning found no organization")
	}
	org := customer.Organizations[0]
	for _, candidate := range customer.Organizations {
		if candidate.IsDefault {
			org = candidate
			break
		}
	}
	if len(org.Projects) == 0 {
		return "", errors.New("Z.AI key provisioning found no project")
	}
	project := org.Projects[0]
	for _, candidate := range org.Projects {
		if candidate.IsDefault {
			project = candidate
			break
		}
	}
	if org.OrganizationID == "" || project.ProjectID == "" {
		return "", errors.New("Z.AI key provisioning found an incomplete organization/project")
	}
	keysURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys", zaiBusinessBaseURL, url.PathEscape(org.OrganizationID), url.PathEscape(project.ProjectID))
	keysData, err := zaiRequest(ctx, http.MethodGet, keysURL, "API key lookup", nil, authHeaders)
	if err != nil {
		return "", err
	}
	var key *zaiAPIKey
	for _, candidate := range zaiKeyList(keysData) {
		if candidate.Name == zaiAPIKeyName && candidate.APIKey != "" {
			copy := candidate
			key = &copy
			break
		}
	}
	if key == nil {
		created, err := zaiRequest(ctx, http.MethodPost, keysURL, "API key creation", map[string]string{"name": zaiAPIKeyName}, authHeaders)
		if err != nil {
			return "", err
		}
		var candidate zaiAPIKey
		if err := json.Unmarshal(created, &candidate); err != nil || candidate.APIKey == "" {
			return "", errors.New("Z.AI API key creation returned no key")
		}
		key = &candidate
	}
	copied, err := zaiRequest(ctx, http.MethodGet, keysURL+"/copy/"+url.PathEscape(key.APIKey), "API key copy", nil, authHeaders)
	if err != nil {
		return "", err
	}
	var secret struct {
		SecretKey string `json:"secretKey"`
	}
	if err := json.Unmarshal(copied, &secret); err != nil || secret.SecretKey == "" {
		return "", errors.New("Z.AI API key copy returned no secret")
	}
	return key.APIKey + "." + secret.SecretKey, nil
}

// ZaiCodingPlanLogin opens the ZCode browser login and provisions the durable
// API key used by the Coding Plan endpoint. Z.AI's client is registered for a
// custom URI, so the user pastes the resulting redirect URL or code back.
func ZaiCodingPlanLogin(ctx context.Context, opts LoginOptions) (*ZaiCodingPlanCredentials, error) {
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, err
	}
	state := hex.EncodeToString(stateBytes)
	authURL := zaiOAuthAuthorizeURL + "?" + url.Values{
		"redirect_uri":  {zaiOAuthRedirectURI},
		"response_type": {"code"},
		"client_id":     {zaiOAuthClientID},
		"state":         {state},
	}.Encode()
	if opts.Printer != nil {
		opts.Printer(authURL)
	}
	if opts.OpenBrowser {
		_ = openBrowser(authURL)
	}
	if opts.Prompt == nil {
		return nil, errors.New("Z.AI browser login requires a redirect URL or authorization code")
	}
	value, err := opts.Prompt("After browser login, paste the final zcode:// redirect URL or authorization code:")
	if err != nil {
		return nil, err
	}
	code, callbackState, err := zaiCallbackValue(value)
	if err != nil {
		return nil, err
	}
	if callbackState != "" && callbackState != state {
		return nil, errors.New("Z.AI OAuth state mismatch")
	}

	tokenData, err := zaiRequest(ctx, http.MethodPost, zaiOAuthTokenURL, "OAuth token exchange", map[string]string{
		"provider":     "zai",
		"code":         code,
		"redirect_uri": zaiOAuthRedirectURI,
		"state":        state,
	}, nil)
	if err != nil {
		return nil, err
	}
	var token struct {
		ZAI struct {
			AccessToken string `json:"access_token"`
		} `json:"zai"`
		User struct {
			ID    any    `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(tokenData, &token); err != nil || token.ZAI.AccessToken == "" {
		return nil, errors.New("Z.AI OAuth response contained no access token")
	}
	mintedKey, err := zaiMintAPIKey(ctx, token.ZAI.AccessToken)
	if err != nil {
		return nil, err
	}
	accountID := ""
	if token.User.ID != nil {
		accountID = fmt.Sprint(token.User.ID)
		if number, ok := token.User.ID.(float64); ok {
			accountID = strconv.FormatInt(int64(number), 10)
		}
	}
	creds := &ZaiCodingPlanCredentials{APIKey: mintedKey, AccountID: accountID, Email: token.User.Email, CreatedAt: time.Now()}
	manager := NewZaiCodingPlanCredentialManager(opts.AuthDir).(*zaiCodingPlanCredentialManager)
	if err := manager.save(creds); err != nil {
		return nil, err
	}
	return creds, nil
}

func zaiCallbackValue(value string) (code, state string, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", errors.New("Z.AI OAuth code is empty")
	}
	parsed, parseErr := url.Parse(value)
	if parseErr == nil && parsed.Scheme != "" {
		if parsed.Scheme != "zcode" || parsed.Host != "zai-auth" {
			return "", "", errors.New("Z.AI redirect URL must use zcode://zai-auth/callback")
		}
		query := parsed.Query()
		if oauthErr := query.Get("error"); oauthErr != "" {
			return "", "", fmt.Errorf("Z.AI OAuth error: %s", oauthErr)
		}
		if query.Get("code") == "" {
			return "", "", errors.New("Z.AI redirect URL contains no authorization code")
		}
		return query.Get("code"), query.Get("state"), nil
	}
	return value, "", nil
}
