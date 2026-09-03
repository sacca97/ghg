// Package models loads reusable, non-secret provider profiles and resolves
// them with the credential-bearing instances in config.json.
package models

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

const (
	// SchemaVersion is the current provider-profile schema understood by the
	// ghg. A profile must opt into this version explicitly.
	SchemaVersion = 1

	AuthBearer            = "bearer"
	AuthHeader            = "header"
	AuthNone              = "none"
	AuthCodexSubscription = "codex-subscription"

	CatalogOpenAIModels    = "openai-models"
	CatalogAnthropicModels = "anthropic-models"
	CatalogNone            = "none"
)

const maxProfileBytes = 256 << 10

const legacyOpenCodeAnthropicProfileID = "opencode-anthropic"

// Profile is the non-secret description of a provider endpoint. It is loaded
// from YAML; credentials never belong here and are supplied by an Instance.
type Profile struct {
	Schema         int               `yaml:"schema"`
	ID             string            `yaml:"id"`
	DisplayName    string            `yaml:"display_name"`
	Protocol       Protocol          `yaml:"protocol"`
	BaseURL        string            `yaml:"base_url"`
	Auth           Auth              `yaml:"auth"`
	Docs           Docs              `yaml:"docs"`
	DefaultHeaders map[string]string `yaml:"default_headers"`
	Catalog        Catalog           `yaml:"catalog"`
	Capabilities   Capabilities      `yaml:"capabilities"`
	Routes         []Route           `yaml:"routes"`

	source string
}

// Route overrides the protocol-level request details for models matching one
// of Models. Routes are evaluated in declaration order and the first match
// wins. Credentials remain profile-level: Auth.EnvVar is deliberately not a
// route field and is inherited from the profile when Auth overrides its mode.
type Route struct {
	Models         []string          `yaml:"models"`
	Protocol       Protocol          `yaml:"protocol"`
	Auth           Auth              `yaml:"auth"`
	DefaultHeaders map[string]string `yaml:"default_headers"`
}

// Auth describes how an instance's resolved API key is placed on requests.
// bearer prefixes it with "Bearer "; header sends it as-is; none sends no
// credential even when the JSONC instance has no apiKey.
type Auth struct {
	Kind   string `yaml:"kind"`
	Header string `yaml:"header"`
	EnvVar string `yaml:"env_var"`
}

// Docs contains safe, user-facing links for provider setup. It never carries
// a credential or a secret reference.
type Docs struct {
	KeysURL string `yaml:"keys_url"`
}

// Catalog describes the optional model-discovery behavior of a profile.
type Catalog struct {
	Kind   string `yaml:"kind"`
	Public bool   `yaml:"public"`
	// ModelsDev is the matching provider ID in models.dev when it differs from
	// the profile ID.
	ModelsDev string `yaml:"models_dev"`
}

// Capabilities are descriptive profile metadata; model catalogs may refine it.
type Capabilities struct {
	Tools       bool   `yaml:"tools"`
	Vision      bool   `yaml:"vision"`
	Thinking    bool   `yaml:"thinking"`
	PromptCache string `yaml:"prompt_cache"`
}

// Instance is the credential-free part of a config.Provider used to resolve a
// profile. The API key is deliberately resolved separately by config.Provider.
type Instance struct {
	Name     string
	Profile  string
	BaseURL  string
	Protocol Protocol
}

// Resolved is a validated profile after an instance's optional overrides have
// been applied. It still contains no credential material.
type Resolved struct {
	Name           string
	Profile        Profile
	BaseURL        string
	Protocol       Protocol
	Auth           Auth
	Docs           Docs
	DefaultHeaders map[string]string
	Catalog        Catalog
	Capabilities   Capabilities
}

// RequiresAPIKey reports whether the selected auth mode needs a JSONC
// apiKey/apiKeyEnv or another credential source.
func (r Resolved) RequiresAPIKey() bool {
	return r.Auth.Kind != AuthNone && r.Auth.Kind != AuthCodexSubscription
}

// RequiresOAuth reports whether the selected auth mode requires an OAuth flow.
func (r Resolved) RequiresOAuth() bool {
	return r.Auth.Kind == AuthCodexSubscription
}

// Profiles is the loaded profile set. Its zero value is useful: resolving an
// old JSONC provider without a profile produces an anonymous in-memory
// profile, so existing configs do not require migration.
type Profiles struct {
	profiles map[string]Profile
}

// LoadOptions controls filesystem profile loading. Empty UserDir and
// ProjectDir select the normal ghg locations. Project profiles are read
// only when ProjectTrusted is true.
type LoadOptions struct {
	UserDir        string
	ProjectDir     string
	ProjectTrusted bool
}

//go:embed profiles/*.yaml
var embeddedProfiles embed.FS

// Load reads embedded profiles, then user profiles, then (only when trusted)
// project profiles. Later levels replace an ID from an earlier level; a
// duplicate within one level is an error.
func Load(opts LoadOptions) (Profiles, error) {
	loaded := Profiles{profiles: make(map[string]Profile)}
	if err := loadEmbedded(loaded.profiles); err != nil {
		return Profiles{}, err
	}

	userDir := opts.UserDir
	if userDir == "" {
		dir, err := defaultUserProfileDir()
		if err != nil {
			return Profiles{}, fmt.Errorf("provider profiles: resolve user directory: %w", err)
		}
		userDir = filepath.Join(dir, "providers")
	}
	if err := loadDirectory(loaded.profiles, userDir, "user"); err != nil {
		return Profiles{}, err
	}

	if opts.ProjectTrusted {
		projectDir := opts.ProjectDir
		if projectDir == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return Profiles{}, fmt.Errorf("provider profiles: resolve project directory: %w", err)
			}
			projectDir = filepath.Join(cwd, ".ghg", "providers")
		}
		if err := loadDirectory(loaded.profiles, projectDir, "trusted project"); err != nil {
			return Profiles{}, err
		}
	}
	return loaded, nil
}

// Lookup returns a defensive copy of a profile by ID.
func (p Profiles) Lookup(id string) (Profile, bool) {
	profile, ok := p.profiles[id]
	if !ok {
		return Profile{}, false
	}
	return cloneProfile(profile), true
}

// IDs returns loaded profile IDs in deterministic order.
func (p Profiles) IDs() []string {
	ids := make([]string, 0, len(p.profiles))
	for id := range p.profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Resolve applies a JSONC provider instance to a named profile. A provider
// without a profile is normalized to an anonymous OpenAI-compatible profile
// using its legacy API/baseUrl fields. It intentionally does not apply a
// model route; callers with a wire model ID should use ResolveModel.
func (p Profiles) Resolve(in Instance) (Resolved, error) {
	return p.ResolveModel(in, "")
}

// ResolveModel applies a JSONC provider instance and the first matching
// model route. An empty modelID preserves profile-level defaults, which is the
// behavior needed by catalog fetches and authentication probes.
func (p Profiles) ResolveModel(in Instance, modelID string) (Resolved, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "provider"
	}

	var profile Profile
	if id := strings.TrimSpace(in.Profile); id != "" {
		var ok bool
		profile, ok = p.Lookup(id)
		if !ok {
			if id != legacyOpenCodeAnthropicProfileID {
				return Resolved{}, fmt.Errorf("provider %q references unknown profile %q (available: %s)", name, id, strings.Join(p.IDs(), ", "))
			}
			profile = legacyOpenCodeAnthropicProfile(name, in)
		}
	} else {
		profile = Profile{
			Schema:      SchemaVersion,
			ID:          "anonymous",
			DisplayName: name,
			Protocol:    normalizeProtocol(in.Protocol),
			BaseURL:     in.BaseURL,
			Auth:        Auth{Kind: AuthBearer, Header: "Authorization"},
			Catalog:     Catalog{Kind: CatalogOpenAIModels},
		}
		if profile.Protocol == "" {
			profile.Protocol = ProtocolOpenAIChatCompletions
		}
	}

	if strings.TrimSpace(string(in.Protocol)) != "" {
		profile.Protocol = normalizeProtocol(in.Protocol)
	}
	if strings.TrimSpace(in.BaseURL) != "" {
		profile.BaseURL = in.BaseURL
	}
	if profile.DisplayName == "" {
		profile.DisplayName = name
	}
	if err := validateProfile(&profile); err != nil {
		return Resolved{}, located(profile.source, fmt.Errorf("provider %q: %w", name, err))
	}

	resolved := Resolved{
		Name:           name,
		Profile:        cloneProfile(profile),
		BaseURL:        profile.BaseURL,
		Protocol:       profile.Protocol,
		Auth:           profile.Auth,
		Docs:           profile.Docs,
		DefaultHeaders: cloneHeaders(profile.DefaultHeaders),
		Catalog:        profile.Catalog,
		Capabilities:   profile.Capabilities,
	}
	if strings.TrimSpace(modelID) != "" {
		if err := applyRoute(&resolved, modelID); err != nil {
			return Resolved{}, err
		}
	}
	return resolved, nil
}

func loadEmbedded(dst map[string]Profile) error {
	files, err := fs.Glob(embeddedProfiles, "profiles/*.yaml")
	if err != nil {
		return fmt.Errorf("provider profiles: list embedded profiles: %w", err)
	}
	seen := make(map[string]string, len(files))
	for _, name := range files {
		data, err := fs.ReadFile(embeddedProfiles, name)
		if err != nil {
			return fmt.Errorf("provider profiles: read %s: %w", name, err)
		}
		profile, err := parseProfile(data, "embedded:"+name)
		if err != nil {
			return err
		}
		if err := addProfile(dst, seen, profile); err != nil {
			return fmt.Errorf("provider profiles: embedded: %w", err)
		}
	}
	return nil
}

func loadDirectory(dst map[string]Profile, dir, level string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provider profiles: read %s directory %q: %w", level, dir, err)
	}
	seen := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || !isProfileFilename(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readProfileFile(path)
		if err != nil {
			return err
		}
		profile, err := parseProfile(data, path)
		if err != nil {
			return err
		}
		if err := addProfile(dst, seen, profile); err != nil {
			return fmt.Errorf("provider profiles: %s: %w", level, err)
		}
	}
	return nil
}

func isProfileFilename(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

func readProfileFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("provider profile %s: open: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxProfileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("provider profile %s: read: %w", path, err)
	}
	if len(data) > maxProfileBytes {
		return nil, fmt.Errorf("provider profile %s: file exceeds %d-byte limit", path, maxProfileBytes)
	}
	return data, nil
}

func addProfile(dst map[string]Profile, seen map[string]string, profile Profile) error {
	if previous, ok := seen[profile.ID]; ok {
		return fmt.Errorf("duplicate profile ID %q in %s and %s", profile.ID, previous, profile.source)
	}
	seen[profile.ID] = profile.source
	dst[profile.ID] = profile
	return nil
}

func parseProfile(data []byte, source string) (Profile, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var profile Profile
	if err := decoder.Decode(&profile); err != nil {
		if errors.Is(err, io.EOF) {
			return Profile{}, located(source, errors.New("empty YAML document"))
		}
		return Profile{}, located(source, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Profile{}, located(source, errors.New("multiple YAML documents are not allowed; use one profile per file"))
		}
		return Profile{}, located(source, err)
	}
	profile.source = source
	if err := validateProfile(&profile); err != nil {
		return Profile{}, located(source, err)
	}
	return profile, nil
}

func validateProfile(profile *Profile) error {
	if profile.Schema != SchemaVersion {
		return fmt.Errorf("profile %q has schema %d; supported schema is %d", profile.ID, profile.Schema, SchemaVersion)
	}
	profile.ID = strings.TrimSpace(profile.ID)
	if !validID(profile.ID) {
		return fmt.Errorf("profile ID %q is invalid; use 1-%d lowercase letters, digits, '.', '_' or '-'", profile.ID, 64)
	}
	profile.DisplayName = strings.TrimSpace(profile.DisplayName)
	if profile.DisplayName == "" {
		return fmt.Errorf("profile %q display_name is required", profile.ID)
	}
	if len(profile.DisplayName) > 200 || hasControl(profile.DisplayName) {
		return fmt.Errorf("profile %q display_name is invalid", profile.ID)
	}

	// YAML profiles use canonical protocol names. The legacy
	// openai-completions spelling is accepted only while normalizing the old
	// JSONC provider instance in Resolve.
	profile.Protocol = normalizeProtocol(profile.Protocol)
	switch profile.Protocol {
	case ProtocolOpenAIChatCompletions, ProtocolAnthropicMessages, ProtocolOpenAIResponses:
	default:
		return fmt.Errorf("profile %q has unknown protocol %q (want %s, %s, or %s)", profile.ID, profile.Protocol, ProtocolOpenAIChatCompletions, ProtocolAnthropicMessages, ProtocolOpenAIResponses)
	}
	baseURL, err := normalizeBaseURL(profile.BaseURL)
	if err != nil {
		return fmt.Errorf("profile %q base_url: %w", profile.ID, err)
	}
	profile.BaseURL = baseURL

	profile.Auth.Kind = strings.ToLower(strings.TrimSpace(profile.Auth.Kind))
	profile.Auth.Header = strings.TrimSpace(profile.Auth.Header)
	profile.Auth.EnvVar = strings.TrimSpace(profile.Auth.EnvVar)
	switch profile.Auth.Kind {
	case AuthNone:
		if profile.Auth.Header != "" || profile.Auth.EnvVar != "" {
			return fmt.Errorf("profile %q auth.header and auth.env_var must be empty when auth.kind is none", profile.ID)
		}
	case AuthCodexSubscription:
		if profile.Auth.Header != "" || profile.Auth.EnvVar != "" {
			return fmt.Errorf("profile %q auth.header and auth.env_var must be empty when auth.kind is codex-subscription", profile.ID)
		}
	case AuthBearer, AuthHeader:
		if !validHeaderName(profile.Auth.Header) {
			return fmt.Errorf("profile %q auth.header must be a valid HTTP header name", profile.ID)
		}
		if profile.Auth.EnvVar != "" && !validEnvVar(profile.Auth.EnvVar) {
			return fmt.Errorf("profile %q auth.env_var must be a valid environment variable name", profile.ID)
		}
	default:
		return fmt.Errorf("profile %q has unknown auth.kind %q (want bearer, header, none, or codex-subscription)", profile.ID, profile.Auth.Kind)
	}

	profile.Docs.KeysURL = strings.TrimSpace(profile.Docs.KeysURL)
	if profile.Docs.KeysURL != "" {
		if _, err := normalizeDocsURL(profile.Docs.KeysURL); err != nil {
			return fmt.Errorf("profile %q docs.keys_url: %w", profile.ID, err)
		}
	}

	if profile.DefaultHeaders == nil {
		profile.DefaultHeaders = map[string]string{}
	}
	for name, value := range profile.DefaultHeaders {
		if !validHeaderName(name) {
			return fmt.Errorf("profile %q default_headers contains invalid header name %q", profile.ID, name)
		}
		if !validHeaderValue(value) {
			return fmt.Errorf("profile %q default_headers[%q] contains a forbidden control character", profile.ID, name)
		}
		if isCredentialHeader(name) {
			return fmt.Errorf("profile %q default_headers must not contain credential header %q; configure auth instead", profile.ID, name)
		}
		if profile.Auth.Header != "" && strings.EqualFold(name, profile.Auth.Header) {
			return fmt.Errorf("profile %q default_headers must not override auth.header %q; configure auth instead", profile.ID, profile.Auth.Header)
		}
	}

	profile.Catalog.Kind = strings.ToLower(strings.TrimSpace(profile.Catalog.Kind))
	profile.Catalog.ModelsDev = strings.TrimSpace(profile.Catalog.ModelsDev)
	if hasControl(profile.Catalog.ModelsDev) {
		return fmt.Errorf("profile %q catalog.models_dev contains a control character", profile.ID)
	}
	switch profile.Catalog.Kind {
	case CatalogOpenAIModels, CatalogAnthropicModels, CatalogNone:
	default:
		return fmt.Errorf("profile %q has unknown catalog.kind %q", profile.ID, profile.Catalog.Kind)
	}
	profile.Capabilities.PromptCache = strings.ToLower(strings.TrimSpace(profile.Capabilities.PromptCache))
	if profile.Capabilities.PromptCache != "" && profile.Capabilities.PromptCache != "provider" && profile.Capabilities.PromptCache != "none" {
		return fmt.Errorf("profile %q has unknown capabilities.prompt_cache %q (want provider or none)", profile.ID, profile.Capabilities.PromptCache)
	}
	if err := validateRoutes(profile); err != nil {
		return err
	}
	return nil
}

func validateRoutes(profile *Profile) error {
	for i := range profile.Routes {
		route := &profile.Routes[i]
		if len(route.Models) == 0 {
			return fmt.Errorf("profile %q route %d must name at least one model pattern", profile.ID, i+1)
		}
		for j, pattern := range route.Models {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				return fmt.Errorf("profile %q route %d model pattern %d is empty", profile.ID, i+1, j+1)
			}
			if hasControl(pattern) {
				return fmt.Errorf("profile %q route %d model pattern %d contains a control character", profile.ID, i+1, j+1)
			}
			if _, err := path.Match(pattern, ""); err != nil {
				return fmt.Errorf("profile %q route %d model pattern %q: %w", profile.ID, i+1, pattern, err)
			}
			route.Models[j] = pattern
		}

		route.Protocol = normalizeProtocol(route.Protocol)
		switch route.Protocol {
		case ProtocolOpenAIChatCompletions, ProtocolAnthropicMessages, ProtocolOpenAIResponses:
		default:
			return fmt.Errorf("profile %q route %d has unknown protocol %q", profile.ID, i+1, route.Protocol)
		}

		if route.Auth.EnvVar != "" {
			return fmt.Errorf("profile %q route %d auth.env_var is provider-level and must be omitted", profile.ID, i+1)
		}
		if route.Auth.Kind == "" {
			if route.Auth.Header != "" {
				return fmt.Errorf("profile %q route %d auth.kind is required when auth.header is set", profile.ID, i+1)
			}
		} else {
			route.Auth.Kind = strings.ToLower(strings.TrimSpace(route.Auth.Kind))
			route.Auth.Header = strings.TrimSpace(route.Auth.Header)
			switch route.Auth.Kind {
			case AuthNone:
				if route.Auth.Header != "" {
					return fmt.Errorf("profile %q route %d auth.header must be empty when auth.kind is none", profile.ID, i+1)
				}
			case AuthBearer, AuthHeader:
				if !validHeaderName(route.Auth.Header) {
					return fmt.Errorf("profile %q route %d auth.header must be a valid HTTP header name", profile.ID, i+1)
				}
			default:
				return fmt.Errorf("profile %q route %d has unknown auth.kind %q", profile.ID, i+1, route.Auth.Kind)
			}
		}

		if route.DefaultHeaders == nil {
			route.DefaultHeaders = map[string]string{}
		}
		authHeader := profile.Auth.Header
		if route.Auth.Kind != "" {
			authHeader = route.Auth.Header
		}
		for name, value := range route.DefaultHeaders {
			if !validHeaderName(name) {
				return fmt.Errorf("profile %q route %d default_headers contains invalid header name %q", profile.ID, i+1, name)
			}
			if !validHeaderValue(value) {
				return fmt.Errorf("profile %q route %d default_headers[%q] contains a forbidden control character", profile.ID, i+1, name)
			}
			if isCredentialHeader(name) {
				return fmt.Errorf("profile %q route %d default_headers must not contain credential header %q; configure auth instead", profile.ID, i+1, name)
			}
			if authHeader != "" && strings.EqualFold(name, authHeader) {
				return fmt.Errorf("profile %q route %d default_headers must not override auth.header %q; configure auth instead", profile.ID, i+1, authHeader)
			}
		}
	}
	return nil
}

func normalizeProtocol(protocol Protocol) Protocol {
	protocol = Protocol(strings.ToLower(strings.TrimSpace(string(protocol))))
	if protocol == ProtocolOpenAICompletions {
		return ProtocolOpenAIChatCompletions
	}
	return protocol
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("is invalid: %w", err)
	}
	if u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return "", errors.New("must be an absolute HTTP(S) URL")
	}
	if u.User != nil {
		return "", errors.New("must not contain userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("must not contain a query or fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := u.Hostname()
	if host == "" {
		return "", errors.New("must contain a host")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(host) {
			return "", errors.New("HTTP is allowed only for loopback endpoints (use HTTPS for remote hosts)")
		}
	default:
		return "", fmt.Errorf("unsupported URL scheme %q; use https or loopback http", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopback(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// defaultUserProfileDir mirrors the ghg home selection without importing
// config. Keeping profile loading independent lets config persist a resolved
// provider without creating an import cycle.
func defaultUserProfileDir() (string, error) {
	root := strings.TrimSpace(os.Getenv("GHG_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".ghg")
	}
	return root, nil
}

func normalizeDocsURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("is invalid: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.Opaque != "" || u.User != nil {
		return "", errors.New("must be an absolute HTTPS URL without userinfo")
	}
	if hasControl(raw) {
		return "", errors.New("contains a control character")
	}
	return raw, nil
}

func validEnvVar(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && r != '_' {
				return false
			}
			continue
		}
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func validID(id string) bool {
	if id == "" || len(id) > 64 || id[0] == '-' || id[0] == '.' {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func validHeaderName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r >= 127 || r == ':' {
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	return !strings.ContainsAny(value, "\r\n\x00")
}

func isCredentialHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "api-key", "x-api-key":
		return true
	default:
		return false
	}
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func cloneProfile(profile Profile) Profile {
	profile.DefaultHeaders = cloneHeaders(profile.DefaultHeaders)
	if profile.Routes != nil {
		routes := profile.Routes
		profile.Routes = make([]Route, len(profile.Routes))
		for i, route := range routes {
			profile.Routes[i] = route
			profile.Routes[i].Models = slices.Clone(route.Models)
			profile.Routes[i].DefaultHeaders = cloneHeaders(route.DefaultHeaders)
		}
	}
	return profile
}

func applyRoute(resolved *Resolved, modelID string) error {
	for i, route := range resolved.Profile.Routes {
		matched := false
		for _, pattern := range route.Models {
			ok, err := path.Match(pattern, modelID)
			if err != nil {
				return fmt.Errorf("provider %q route %d model pattern %q: %w", resolved.Name, i+1, pattern, err)
			}
			if ok {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}

		resolved.Protocol = route.Protocol
		if route.Auth.Kind != "" {
			resolved.Auth = route.Auth
			if route.Auth.Kind != AuthNone {
				resolved.Auth.EnvVar = resolved.Profile.Auth.EnvVar
			}
		}
		for name, value := range route.DefaultHeaders {
			if resolved.DefaultHeaders == nil {
				resolved.DefaultHeaders = make(map[string]string)
			}
			resolved.DefaultHeaders[name] = value
		}
		return nil
	}
	return nil
}

func legacyOpenCodeAnthropicProfile(name string, in Instance) Profile {
	protocol := normalizeProtocol(in.Protocol)
	if protocol == "" {
		protocol = ProtocolAnthropicMessages
	}
	return Profile{
		Schema:      SchemaVersion,
		ID:          "anonymous",
		DisplayName: name,
		Protocol:    protocol,
		BaseURL:     in.BaseURL,
		Auth:        Auth{Kind: AuthHeader, Header: "x-api-key"},
		DefaultHeaders: map[string]string{
			"anthropic-version": "2023-06-01",
		},
		Catalog: Catalog{Kind: CatalogOpenAIModels, Public: true},
	}
}

func cloneHeaders(headers map[string]string) map[string]string {
	return maps.Clone(headers)
}

func located(source string, err error) error {
	if source == "" || err == nil {
		return err
	}
	return fmt.Errorf("%s: %w", source, err)
}
