package auth

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

type zaiTestRoundTripper func(*http.Request) (*http.Response, error)

func (f zaiTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func zaiTestResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestZaiCodingPlanLoginMintsAndStoresDurableKey(t *testing.T) {
	oldAuthorize, oldToken := zaiOAuthAuthorizeURL, zaiOAuthTokenURL
	oldBusiness, oldBusinessLogin := zaiBusinessBaseURL, zaiBusinessLoginURL
	oldClient := zaiHTTPClient
	defer func() {
		zaiOAuthAuthorizeURL, zaiOAuthTokenURL = oldAuthorize, oldToken
		zaiBusinessBaseURL, zaiBusinessLoginURL = oldBusiness, oldBusinessLogin
		zaiHTTPClient = oldClient
	}()
	zaiOAuthAuthorizeURL = "https://chat.test/authorize"
	zaiOAuthTokenURL = "https://zcode.test/token"
	zaiBusinessBaseURL = "https://api.test"
	zaiBusinessLoginURL = "https://api.test/login"
	var calls []string
	zaiHTTPClient = &http.Client{Transport: zaiTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.Method+" "+req.URL.String())
		switch {
		case req.URL.String() == zaiOAuthTokenURL:
			return zaiTestResponse(`{"code":0,"data":{"zai":{"access_token":"oauth-token"},"user":{"id":42,"email":"user@example.test"}}}`), nil
		case req.URL.String() == zaiBusinessLoginURL:
			return zaiTestResponse(`{"code":200,"success":true,"data":{"access_token":"business-token"}}`), nil
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/customer/getCustomerInfo"):
			return zaiTestResponse(`{"code":200,"data":{"organizations":[{"organizationId":"org","isDefault":true,"projects":[{"projectId":"project","isDefault":true}]}]}}`), nil
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/api_keys"):
			return zaiTestResponse(`{"code":200,"data":[]}`), nil
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/api_keys"):
			return zaiTestResponse(`{"code":200,"data":{"apiKey":"id"}}`), nil
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/copy/id"):
			return zaiTestResponse(`{"code":200,"data":{"secretKey":"secret"}}`), nil
		default:
			return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: os.ErrNotExist}
		}
	})}

	dir := t.TempDir()
	var printedURL string
	creds, err := ZaiCodingPlanLogin(context.Background(), LoginOptions{
		AuthDir: dir,
		Printer: func(value string) { printedURL = value },
		Prompt: func(string) (string, error) {
			parsed, err := url.Parse(printedURL)
			if err != nil {
				return "", err
			}
			return "zcode://zai-auth/callback?code=code&state=" + parsed.Query().Get("state"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if creds.APIKey != "id.secret" || creds.AccountID != "42" || creds.Email != "user@example.test" {
		t.Fatalf("credentials = %+v", creds)
	}
	manager := NewZaiCodingPlanCredentialManager(dir)
	key, err := manager.Credentials(context.Background())
	if err != nil || key != "id.secret" {
		t.Fatalf("stored key = %q, err=%v", key, err)
	}
	if len(calls) != 6 {
		t.Fatalf("request count = %d, calls=%v", len(calls), calls)
	}
}
