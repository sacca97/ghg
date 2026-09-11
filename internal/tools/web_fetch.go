package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/sacca97/ghg/internal/models"
)

const (
	webFetchMaxBody      int64 = 2 << 20
	webFetchMaxHeaders   int64 = 64 << 10
	webFetchMaxRedirects       = 5
	webFetchTimeout            = 20 * time.Second
	webFetchDialTimeout        = 10 * time.Second
)

type webFetchArgs struct {
	URL string `json:"url"`
}

type webLookup func(context.Context, string) ([]net.IP, error)

func webFetchTool() Tool {
	return resultTool(models.NewTool("web_fetch",
		"Fetch a public HTTP or HTTPS URL and return bounded readable text. No cookies, credentials, JavaScript, forms, or private-network addresses are allowed.",
		`{"type":"object","properties":{"url":{"type":"string","description":"Public http:// or https:// URL"}},"required":["url"],"additionalProperties":false}`), runWebFetch)
}

func runWebFetch(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var in webFetchArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ToolResult{}, fmt.Errorf("invalid web_fetch request: %w", err)
	}
	runtime := RuntimeFromContext(ctx)
	if runtime == nil || runtime.Policy == nil {
		return ToolResult{}, errors.New("web access is unavailable without an execution policy")
	}
	policy, err := runtime.authorizeNetwork(ctx, "web_fetch", in.URL)
	if err != nil {
		return ToolResult{}, err
	}
	ctx = WithRuntime(ctx, runtime.WithPolicy(policy))
	return fetchWeb(ctx, in.URL, newPublicWebClient(defaultWebLookup), defaultWebLookup)
}

func defaultWebLookup(ctx context.Context, host string) ([]net.IP, error) {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		ips = append(ips, address.IP)
	}
	return ips, nil
}

func newPublicWebClient(lookup webLookup) *http.Client {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            newPublicWebDialer(lookup).DialContext,
		MaxResponseHeaderBytes: webFetchMaxHeaders,
		TLSHandshakeTimeout:    webFetchDialTimeout,
		ResponseHeaderTimeout:  webFetchTimeout,
		ExpectContinueTimeout:  time.Second,
		IdleConnTimeout:        30 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   webFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= webFetchMaxRedirects {
				return errors.New("web_fetch redirect limit exceeded")
			}
			if err := validatePublicWebURL(req.Context(), req.URL, lookup); err != nil {
				return fmt.Errorf("web_fetch redirect rejected: %w", err)
			}
			return nil
		},
	}
}

type webDialer struct {
	lookup webLookup
	dialer net.Dialer
}

func newPublicWebDialer(lookup webLookup) *webDialer {
	return &webDialer{lookup: lookup, dialer: net.Dialer{Timeout: webFetchDialTimeout}}
}

func (d *webDialer) DialContext(ctx context.Context, _, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("split web address: %w", err)
	}
	ips, err := resolvePublicWebIPs(ctx, host, d.lookup)
	if err != nil {
		return nil, err
	}
	var failures []string
	for _, ip := range ips {
		protocol := "tcp6"
		if ip.To4() != nil {
			protocol = "tcp4"
		}
		conn, dialErr := d.dialer.DialContext(ctx, protocol, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		failures = append(failures, dialErr.Error())
	}
	return nil, fmt.Errorf("connect to public web address: %s", strings.Join(failures, "; "))
}

func fetchWeb(ctx context.Context, rawURL string, client *http.Client, lookup webLookup) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	u, err := parsePublicWebURL(rawURL)
	if err != nil {
		return ToolResult{}, err
	}
	if err := validatePublicWebURL(ctx, u, lookup); err != nil {
		return ToolResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ToolResult{}, fmt.Errorf("create web_fetch request: %w", err)
	}
	req.Header.Set("Accept", "text/plain, text/html, application/json, application/xml, text/xml, */*;q=0.1")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ToolResult{}, ctx.Err()
		}
		return ToolResult{}, fmt.Errorf("web_fetch request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength > webFetchMaxBody {
		return ToolResult{}, fmt.Errorf("web_fetch response exceeds %d-byte limit", webFetchMaxBody)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBody+1))
	if err != nil {
		return ToolResult{}, fmt.Errorf("read web_fetch response: %w", err)
	}
	if int64(len(body)) > webFetchMaxBody {
		return ToolResult{}, fmt.Errorf("web_fetch response exceeds %d-byte limit", webFetchMaxBody)
	}
	mediaType := responseMediaType(resp, body)
	if !supportedWebMediaType(mediaType) {
		return ToolResult{}, fmt.Errorf("web_fetch does not support content type %q", mediaType)
	}
	finalRequestURL := u
	if resp.Request != nil && resp.Request.URL != nil {
		finalRequestURL = resp.Request.URL
	}
	title, content := "", string(body)
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		title, content, err = extractWebHTML(body, finalRequestURL)
		if err != nil {
			return ToolResult{}, fmt.Errorf("parse web_fetch HTML: %w", err)
		}
	}
	finalURL := finalRequestURL.String()
	if strings.TrimSpace(content) == "" {
		content = "(no readable content)"
	}
	raw := fmt.Sprintf("Requested URL: %s\nFinal URL: %s\nHTTP status: %s\nContent-Type: %s\nPage title: %s\n\n%s",
		u.String(), finalURL, resp.Status, mediaType, title, content)
	result := MarkUntrusted(TextResult(raw, ""), "web_fetch")
	result.Metadata["requested_url"] = u.String()
	result.Metadata["final_url"] = finalURL
	result.Metadata["content_type"] = mediaType
	return result, nil
}

func parsePublicWebURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("web_fetch url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid web_fetch url: %w", err)
	}
	if u.User != nil {
		return nil, errors.New("web_fetch URLs cannot contain username or password")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("web_fetch supports only http and https URLs")
	}
	if u.Host == "" || u.Hostname() == "" || strings.Contains(u.Hostname(), "%") {
		return nil, errors.New("web_fetch URL must contain a valid public host")
	}
	return u, nil
}

func validatePublicWebURL(ctx context.Context, u *url.URL, lookup webLookup) error {
	if _, err := parsePublicWebURL(u.String()); err != nil {
		return err
	}
	if lookup == nil {
		lookup = defaultWebLookup
	}
	if _, err := resolvePublicWebIPs(ctx, u.Hostname(), lookup); err != nil {
		return fmt.Errorf("web_fetch host rejected: %w", err)
	}
	return nil
}

func resolvePublicWebIPs(ctx context.Context, host string, lookup webLookup) ([]net.IP, error) {
	if lookup == nil {
		lookup = defaultWebLookup
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve web host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve web host %q returned no addresses", host)
	}
	for _, ip := range ips {
		if !publicWebIP(ip) {
			return nil, fmt.Errorf("host %q resolves to a non-public address", host)
		}
	}
	return ips, nil
}

func publicWebIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	if !address.Is4() {
		return true
	}
	b := address.As4()
	return !(b[0] == 0 || (b[0] == 100 && b[1] >= 64 && b[1] <= 127) ||
		(b[0] == 192 && b[1] == 0 && b[2] == 0) ||
		(b[0] == 192 && b[1] == 0 && b[2] == 2) ||
		(b[0] == 198 && b[1] >= 18 && b[1] <= 19) ||
		(b[0] == 198 && b[1] == 51 && b[2] == 100) ||
		(b[0] == 203 && b[1] == 0 && b[2] == 113) || b[0] >= 224)
}

func responseMediaType(resp *http.Response, body []byte) string {
	value := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if value != "" {
		if mediaType, _, err := mime.ParseMediaType(value); err == nil {
			return strings.ToLower(mediaType)
		}
	}
	path := ""
	if resp.Request != nil && resp.Request.URL != nil {
		path = resp.Request.URL.Path
	}
	if mediaType := mime.TypeByExtension(path); mediaType != "" {
		return strings.ToLower(mediaType)
	}
	if len(body) > 0 {
		return strings.ToLower(strings.SplitN(http.DetectContentType(body), ";", 2)[0])
	}
	return "text/plain"
}

func supportedWebMediaType(mediaType string) bool {
	return mediaType == "text/plain" || mediaType == "text/html" || mediaType == "application/xhtml+xml" ||
		mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") ||
		mediaType == "application/xml" || mediaType == "text/xml" || strings.HasSuffix(mediaType, "+xml")
}

func extractWebHTML(body []byte, base *url.URL) (string, string, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	title := htmlTitle(doc)
	var lines []string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			if text := strings.Join(strings.Fields(node.Data), " "); text != "" {
				lines = append(lines, text)
			}
			return
		}
		if node.Type == html.ElementNode {
			tag := strings.ToLower(node.Data)
			if webHTMLSkipTag(tag) {
				return
			}
			if tag == "a" {
				if text := htmlInlineText(node, base); text != "" {
					lines = append(lines, text)
				}
				return
			}
			if level := htmlHeadingLevel(tag); level > 0 {
				if text := htmlInlineText(node, base); text != "" {
					lines = append(lines, strings.Repeat("#", level)+" "+text)
				}
				return
			}
			if webHTMLBlockTag(tag) {
				if text := htmlInlineText(node, base); text != "" {
					lines = append(lines, text)
				}
				return
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	if len(lines) == 0 {
		if text := htmlInlineText(doc, base); text != "" {
			lines = append(lines, text)
		}
	}
	return title, strings.Join(lines, "\n"), nil
}

func htmlTitle(node *html.Node) string {
	var title string
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if title != "" || current == nil {
			return
		}
		if current.Type == html.ElementNode && strings.EqualFold(current.Data, "title") {
			title = htmlInlineText(current, nil)
			return
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return title
}

func htmlInlineText(node *html.Node, base *url.URL) string {
	if node == nil {
		return ""
	}
	if node.Type == html.TextNode {
		return strings.Join(strings.Fields(node.Data), " ")
	}
	if node.Type == html.ElementNode {
		tag := strings.ToLower(node.Data)
		if webHTMLSkipTag(tag) {
			return ""
		}
		if tag == "br" {
			return " "
		}
		if tag == "a" {
			text := htmlChildrenText(node, base)
			for _, attribute := range node.Attr {
				if attribute.Key != "href" || base == nil {
					continue
				}
				link, err := base.Parse(strings.TrimSpace(attribute.Val))
				if err == nil && (link.Scheme == "http" || link.Scheme == "https") && link.User == nil {
					if text == "" {
						return link.String()
					}
					return text + " (" + link.String() + ")"
				}
			}
			return text
		}
	}
	return htmlChildrenText(node, base)
}

func htmlChildrenText(node *html.Node, base *url.URL) string {
	var parts []string
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if text := htmlInlineText(child, base); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

func webHTMLSkipTag(tag string) bool {
	switch tag {
	case "head", "script", "style", "svg", "noscript", "template", "iframe", "canvas", "form":
		return true
	default:
		return false
	}
}

func webHTMLBlockTag(tag string) bool {
	switch tag {
	case "p", "li", "pre", "blockquote", "dt", "dd", "td", "th":
		return true
	default:
		return false
	}
}

func htmlHeadingLevel(tag string) int {
	if len(tag) != 2 || tag[0] != 'h' || tag[1] < '1' || tag[1] > '6' {
		return 0
	}
	return int(tag[1] - '0')
}
