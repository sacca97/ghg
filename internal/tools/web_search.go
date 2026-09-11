package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
)

const (
	braveSearchEndpoint         = "https://api.search.brave.com/res/v1/web/search"
	webSearchDefaultCount       = 5
	webSearchMaxCount           = 10
	webSearchMaxQuery           = 512
	webSearchMaxBody      int64 = 1 << 20
)

type webSearchArgs struct {
	Query string `json:"query"`
	Count int    `json:"count"`
}

type braveSearchResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

type searxngSearchResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

type searchBackend struct {
	Name     string
	Endpoint string
	APIKey   string
}

func webSearchTool() Tool {
	return resultTool(models.NewTool("web_search",
		"Search the public web with the configured SearXNG provider or Brave fallback. Returns up to ten results with source URLs; never fetches result pages automatically.",
		`{"type":"object","properties":{"query":{"type":"string","description":"Web search query"},"count":{"type":"integer","description":"Number of results, default 5, maximum 10"}},"required":["query"],"additionalProperties":false}`), runWebSearch)
}

func searchProviderStatus() (searchBackend, error) {
	cfg, err := config.Load()
	if err != nil {
		return searchBackend{}, err
	}
	if name := strings.TrimSpace(cfg.SearchProvider); name != "" {
		provider, ok := cfg.SearchProviders[name]
		if !ok {
			return searchBackend{}, fmt.Errorf("selected search provider %q is not configured", name)
		}
		endpoint, err := searxngSearchEndpoint(provider.BaseURL)
		if err != nil {
			return searchBackend{}, err
		}
		return searchBackend{Name: name, Endpoint: endpoint, APIKey: provider.APIKey}, nil
	}
	key := strings.TrimSpace(os.Getenv("BRAVE_SEARCH_API_KEY"))
	if key == "" {
		return searchBackend{}, errors.New("BRAVE_SEARCH_API_KEY is not set and no custom search provider is selected")
	}
	return searchBackend{Name: "brave", Endpoint: braveSearchEndpoint, APIKey: key}, nil
}

func runWebSearch(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var in webSearchArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ToolResult{}, fmt.Errorf("invalid web_search request: %w", err)
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		return ToolResult{}, errors.New("web_search query is required")
	}
	if len(in.Query) > webSearchMaxQuery {
		return ToolResult{}, fmt.Errorf("web_search query exceeds %d characters", webSearchMaxQuery)
	}
	if in.Count == 0 {
		in.Count = webSearchDefaultCount
	}
	if in.Count < 1 || in.Count > webSearchMaxCount {
		return ToolResult{}, fmt.Errorf("web_search count must be between 1 and %d", webSearchMaxCount)
	}
	runtime := RuntimeFromContext(ctx)
	if runtime == nil || runtime.Policy == nil {
		return ToolResult{}, errors.New("web access is unavailable without an execution policy")
	}
	backend, err := searchProviderStatus()
	if err != nil {
		return ToolResult{}, fmt.Errorf("web_search is unavailable: %w", err)
	}
	policy, err := runtime.authorizeNetwork(ctx, "web_search", backend.Endpoint)
	if err != nil {
		return ToolResult{}, err
	}
	ctx = WithRuntime(ctx, runtime.WithPolicy(policy))
	if backend.Name != "brave" {
		return searchSearxng(ctx, in, backend)
	}
	return searchBrave(ctx, in, backend.APIKey)
}

func searchBrave(ctx context.Context, in webSearchArgs, key string) (ToolResult, error) {
	client := &http.Client{Timeout: webFetchTimeout, Transport: &http.Transport{Proxy: nil, MaxResponseHeaderBytes: webFetchMaxHeaders}}
	return searchBraveWithClient(ctx, in, key, braveSearchEndpoint, client)
}

func searchBraveWithClient(ctx context.Context, in webSearchArgs, key, endpoint string, client *http.Client) (ToolResult, error) {
	query := url.Values{}
	query.Set("q", in.Query)
	query.Set("count", strconv.Itoa(in.Count))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return ToolResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ToolResult{}, ctx.Err()
		}
		return ToolResult{}, fmt.Errorf("web_search request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, webSearchMaxBody+1))
	if err != nil {
		return ToolResult{}, fmt.Errorf("read web_search response: %w", err)
	}
	if int64(len(body)) > webSearchMaxBody {
		return ToolResult{}, errors.New("web_search response exceeds 1 MiB")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return ToolResult{}, fmt.Errorf("brave search returned %s", resp.Status)
	}
	var decoded braveSearchResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ToolResult{}, fmt.Errorf("decode Brave Search response: %w", err)
	}
	var out strings.Builder
	shown := 0
	for _, result := range decoded.Web.Results {
		title := strings.TrimSpace(result.Title)
		link := strings.TrimSpace(result.URL)
		if title == "" || link == "" {
			continue
		}
		shown++
		fmt.Fprintf(&out, "%d. %s\n   %s\n   %s\n", shown, title, link, strings.TrimSpace(result.Description))
	}
	if out.Len() == 0 {
		out.WriteString("(no results)")
	}
	result := MarkUntrusted(TextResult(out.String(), ""), "web_search:brave")
	result.Metadata["provider"] = "brave"
	return result, nil
}

func searxngSearchEndpoint(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("SearXNG base URL must be an http(s) URL without credentials, query, or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/search") {
		u.Path += "/search"
	}
	return u.String(), nil
}

func searchSearxng(ctx context.Context, in webSearchArgs, backend searchBackend) (ToolResult, error) {
	client := &http.Client{Timeout: webFetchTimeout, Transport: &http.Transport{Proxy: nil, MaxResponseHeaderBytes: webFetchMaxHeaders}}
	return searchSearxngWithClient(ctx, in, backend, client)
}

func searchSearxngWithClient(ctx context.Context, in webSearchArgs, backend searchBackend, client *http.Client) (ToolResult, error) {
	query := url.Values{}
	query.Set("q", in.Query)
	query.Set("format", "json")
	query.Set("pageno", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, backend.Endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return ToolResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	if backend.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+backend.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ToolResult{}, ctx.Err()
		}
		return ToolResult{}, fmt.Errorf("SearXNG request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, webSearchMaxBody+1))
	if err != nil {
		return ToolResult{}, fmt.Errorf("read SearXNG response: %w", err)
	}
	if int64(len(body)) > webSearchMaxBody {
		return ToolResult{}, errors.New("SearXNG response exceeds 1 MiB")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return ToolResult{}, fmt.Errorf("SearXNG returned %s", resp.Status)
	}
	var decoded searxngSearchResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ToolResult{}, fmt.Errorf("decode SearXNG response: %w", err)
	}
	var out strings.Builder
	shown := 0
	for _, item := range decoded.Results {
		title, link := strings.TrimSpace(item.Title), strings.TrimSpace(item.URL)
		if title == "" || link == "" {
			continue
		}
		shown++
		fmt.Fprintf(&out, "%d. %s\n   %s\n   %s\n", shown, title, link, strings.TrimSpace(item.Content))
		if shown >= webSearchMaxCount {
			break
		}
	}
	if out.Len() == 0 {
		out.WriteString("(no results)")
	}
	result := MarkUntrusted(TextResult(out.String(), ""), "web_search:searxng")
	result.Metadata["provider"] = "searxng"
	result.Metadata["provider_name"] = backend.Name
	return result, nil
}
