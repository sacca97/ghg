// Package config loads and saves ghg's JSONC configuration from ~/.ghg.
package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sacca97/ghg/internal/sandbox"
)

// Provider is an API endpoint that can serve models.
type Provider struct {
	Name      string `json:"name,omitempty"`
	Profile   string `json:"profile,omitempty"` // reusable YAML profile; empty keeps legacy anonymous behavior
	BaseURL   string `json:"baseUrl"`
	API       string `json:"api"`              // legacy protocol selector; profiles use canonical protocol names
	APIKey    string `json:"apiKey,omitempty"` // literal key or a secret reference ("$VAR"/"${VAR}"/"!cmd"); apiKeyEnv is another option
	APIKeyEnv string `json:"apiKeyEnv,omitempty"`
}

// Key returns the resolved API key for the provider, "" when none is
// configured. Unresolvable secret references degrade to "" like a missing
// key; ResolveKey reports the error for callers that can surface it.
func (p Provider) Key() string {
	k, _ := p.ResolveKey()
	return k
}

// ResolveKey is Key with error detail: apiKey/apiKeyEnv may hold a secret
// reference (see ResolveSecret), resolved here at the point of use so the
// config file and session store hold only references and a missing var only
// errors when the provider is actually used. The resolved value never enters
// the event log.
func (p Provider) ResolveKey() (string, error) {
	if p.APIKeyEnv != "" {
		if v := os.Getenv(p.APIKeyEnv); v != "" {
			return v, nil
		}
	}
	if p.APIKey != "" {
		k, err := ResolveSecret(p.APIKey)
		if err != nil {
			return "", fmt.Errorf("provider %q apiKey: %w", p.Name, err)
		}
		return k, nil
	}
	// ponytail: special-case fallback to the inf CLI's stored key; generalize to apiKeyFile if more providers need it
	// when the profile or legacy URL identifies the built-in default service.
	if p.Profile == "inference" || strings.Contains(p.BaseURL, "api.inference.net") {
		return infKey(), nil
	}
	return "", nil
}

// infKey reads apiKey/codingAgentApiKey from ~/.inf/config.json (written by `inf auth set-key`).
func infKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".inf", "config.json"))
	if err != nil {
		return ""
	}
	var c struct {
		APIKey            string `json:"apiKey"`
		CodingAgentAPIKey string `json:"codingAgentApiKey"`
	}
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	if c.APIKey != "" {
		return c.APIKey
	}
	return c.CodingAgentAPIKey
}

// Model routes a model to one or more providers that serve it.
type Model struct {
	Name      string   `json:"name,omitempty"`
	Providers []string `json:"providers"`    // provider keys, first is the default
	ID        string   `json:"id,omitempty"` // model id sent to the API; defaults to the map key
	// API overrides a profile route for this model. It is useful as an escape
	// hatch when a provider announces a model before its profile route table is
	// updated.
	API string `json:"api,omitempty"`
	// Context is the model's context window (max INPUT tokens). The provider's
	// /models context_length overrides it when advertised; this is the fallback
	// and the value shown for providers that don't report one.
	Context int `json:"context,omitempty"`
	// MaxOut caps OUTPUT tokens (the max_tokens request param). 0 uses the
	// provider's max_completion_tokens when advertised, else a sane default.
	MaxOut int `json:"maxOut,omitempty"`
	// MaxTokens is the legacy field name for Context (it was misnamed: it held
	// the context window, not an output cap). Read on load for back-compat.
	MaxTokens int `json:"maxTokens,omitempty"`
	// Vision reports whether the model accepts image inputs. When false (the
	// default), @image tags are NOT inlined as base64 vision parts — the model
	// gets a pointer note instead, so a text-only model isn't sent a request it
	// would reject. A provider-advertised input_modalities entry overrides this.
	Vision bool `json:"vision,omitempty"`
}

// ContextWindow returns the model's context (input) size, honoring the legacy
// maxTokens field for configs written before the rename.
func (m Model) ContextWindow() int {
	if m.Context > 0 {
		return m.Context
	}
	return m.MaxTokens
}

// DefaultCompactModel is the built-in compaction-model default: the
// deepseek-v4-flash route wired into the built-in default config. An
// empty compactModel resolves to this at apply time, falling back to the
// conversation's model when it's not in the user's config.
const DefaultCompactModel = "deepseek-v4-flash-0731"

// DefaultCompactPct is the built-in compaction threshold: compact once the
// estimated context use crosses this percent of the model's context window.
// 40% keeps compaction deterministic instead of letting the context bloat (see Uber guidelines of compacting at ~400K, we use mostly 1M).
const DefaultCompactPct = 40

// CompactThreshold returns the configured compaction fraction, clamped to the
// supported 10–90% range. Zero selects DefaultCompactPct.
func CompactThreshold(c *Config) float64 {
	pct := DefaultCompactPct
	if c != nil && c.CompactPct != 0 {
		pct = c.CompactPct
	}
	return float64(min(max(pct, 10), 90)) / 100
}

// Config is the root of ~/.ghg/config.json (JSONC: comments allowed).
type Config struct {
	DefaultModel    string                `json:"defaultModel"`
	DefaultProvider string                `json:"defaultProvider,omitempty"` // override the model's first provider
	DefaultEffort   string                `json:"defaultEffort,omitempty"`   // reasoning effort for new sessions: "", "low", "medium", "high"
	CompactModel    string                `json:"compactModel,omitempty"`    // model for compaction summaries; "" = the built-in default
	CompactProvider string                `json:"compactProvider,omitempty"` // provider for the compaction model; "" = the model's default routing
	CompactPct      int                   `json:"compactPct,omitempty"`      // compact at this % of the context window; 0 = DefaultCompactPct
	Theme           string                `json:"theme,omitempty"`           // "light", "dark", or "" (auto-detect at startup)
	Mouse           *bool                 `json:"mouse,omitempty"`           // false disables capture so native terminal selection works
	Thinking        *bool                 `json:"thinking,omitempty"`        // nil defaults to on; false hides reasoning tokens (ctrl+o)
	CollapsePaste   *bool                 `json:"collapsePaste,omitempty"`   // nil/false: pastes land verbatim; true collapses ≥3-line pastes into a [Pasted ~N lines] placeholder
	GoalMaxRounds   int                   `json:"goalMaxRounds,omitempty"`   // global goal-loop round cap; 0 = DefaultGoalMaxRounds; projects.json may override per folder
	MaxRetries      int                   `json:"maxRetries,omitempty"`      // attempts per provider request on transient failures (429/5xx/network); 0 = llm.DefaultMaxAttempts, 1 = no retries
	Artifacts       *ArtifactConfig       `json:"artifacts,omitempty"`       // bounded tool-result persistence; nil/enabled nil uses defaults
	Execution       *ExecutionConfig      `json:"execution,omitempty"`       // filesystem/network/approval policy for tool subprocesses
	Providers       map[string]Provider   `json:"providers"`
	Models          map[string]Model      `json:"models"`
	Roles           map[string]RoleConfig `json:"roles,omitempty"`
	// MCPServers is ghg's own MCP server block (ghg-native shape; see
	// internal/mcp.ServerConfig for the normalized semantics). On load it is
	// merged over imported claude/codex configs: ghg always wins per name.
	MCPServers map[string]MCPServer `json:"mcp,omitempty"`
	// MCPImport gates which imported MCP server definitions ghg picks up
	// (claude-style .mcp.json, codex-style ~/.codex/config.toml). nil imports
	// both sources, preserving the pre-gating behavior.
	MCPImport *MCPImport `json:"mcpImport,omitempty"`
	// LSPServers is ghg's own LSP server block (ghg-native shape; see
	// internal/lsp.FromConfigMap for the merge semantics). Entries extend or
	// disable the built-in registry (gopls).
	LSPServers map[string]LSPServer `json:"lsp,omitempty"`
	// PostEdit contains trusted argv-style commands run after successful
	// mutations and before their final readback.
	PostEdit []PostEditConfig `json:"postEdit,omitempty"`
}

// ExecutionConfig controls the shared Phase 3 execution boundary. Empty
// fields select the safe defaults: workspace-write, network deny, and an
// interactive human approval layer for exceptional capabilities.
type ExecutionConfig struct {
	Sandbox        string   `json:"sandbox,omitempty"`        // read-only, workspace-write, danger-full-access
	Network        string   `json:"network,omitempty"`        // deny or host
	Approval       string   `json:"approval,omitempty"`       // ask, auto-review, never
	BubblewrapPath string   `json:"bubblewrapPath,omitempty"` // trusted absolute bwrap path on Linux
	SecretNames    []string `json:"secretNames,omitempty"`    // additional secret-name glob patterns
	ReadRoots      []string `json:"readRoots,omitempty"`      // explicit additional read roots
	WriteRoots     []string `json:"writeRoots,omitempty"`     // explicit additional write roots
	CacheRoots     []string `json:"cacheRoots,omitempty"`     // trusted canonical cache roots
	TempRoots      []string `json:"tempRoots,omitempty"`      // private temporary roots
	ProtectedRoots []string `json:"protectedRoots,omitempty"` // additional read-only metadata/state roots
}

// ApplyExecutionOverrides applies one-shot CLI/session overrides without
// persisting them. Empty arguments leave the loaded configuration unchanged.
func (c *Config) ApplyExecutionOverrides(mode, network, approval string) error {
	if c == nil || (mode == "" && network == "" && approval == "") {
		return nil
	}
	settings := ExecutionConfig{}
	if c.Execution != nil {
		settings = *c.Execution
		settings.SecretNames = append([]string(nil), c.Execution.SecretNames...)
		settings.ReadRoots = append([]string(nil), c.Execution.ReadRoots...)
		settings.WriteRoots = append([]string(nil), c.Execution.WriteRoots...)
		settings.CacheRoots = append([]string(nil), c.Execution.CacheRoots...)
		settings.TempRoots = append([]string(nil), c.Execution.TempRoots...)
		settings.ProtectedRoots = append([]string(nil), c.Execution.ProtectedRoots...)
	}
	if mode != "" {
		settings.Sandbox = mode
	}
	if network != "" {
		settings.Network = network
	}
	if approval != "" {
		settings.Approval = approval
	}
	c.Execution = &settings
	return c.ValidateExecution()
}

// ValidateExecution rejects unknown policy values before a run can start with
// a silently different trust boundary.
func (c *Config) ValidateExecution() error {
	if c == nil || c.Execution == nil {
		return nil
	}
	if _, err := sandbox.ParseMode(c.Execution.Sandbox); err != nil {
		return err
	}
	if _, err := sandbox.ParseNetworkMode(c.Execution.Network); err != nil {
		return err
	}
	switch strings.TrimSpace(strings.ToLower(c.Execution.Approval)) {
	case "", "ask", "auto-review", "never":
	default:
		return fmt.Errorf("unknown approval mode %q (want ask, auto-review, or never)", c.Execution.Approval)
	}
	if pathValue := strings.TrimSpace(c.Execution.BubblewrapPath); pathValue != "" && !filepath.IsAbs(pathValue) {
		return fmt.Errorf("bubblewrapPath must be absolute: %q", c.Execution.BubblewrapPath)
	}
	for _, pattern := range c.Execution.SecretNames {
		if _, err := pathpkg.Match(pattern, "SECRET"); err != nil {
			return fmt.Errorf("invalid secretNames pattern %q: %w", pattern, err)
		}
	}
	return nil
}

// ArtifactConfig controls durable tool-result evidence. Persistence is on by
// default; Enabled is a pointer so an explicit false is distinguishable from
// an older config that has no artifacts block. MaxBytes is the per-result
// stored-payload ceiling and falls back to the artifact package default.
type ArtifactConfig struct {
	Enabled  *bool `json:"enabled,omitempty"`
	MaxBytes int64 `json:"maxBytes,omitempty"`
}

// LSPServer is the config-file form of an LSP server entry. It mirrors
// MCPServer minus the remote fields (LSP is stdio-only here).
type LSPServer struct {
	Command     []string          `json:"command,omitempty"`
	Extensions  []string          `json:"extensions,omitempty"`
	RootMarkers []string          `json:"rootMarkers,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
}

// PostEditConfig describes one trusted post-publication hook. Commands are
// argv arrays rather than shell strings; an extension without a leading dot
// is normalized during validation.
type PostEditConfig struct {
	Command        []string `json:"command"`
	Extensions     []string `json:"extensions,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
}

// ValidatePostEdit normalizes and validates the trusted hook configuration.
func (c *Config) ValidatePostEdit() error {
	if c == nil {
		return nil
	}
	for i := range c.PostEdit {
		hook := &c.PostEdit[i]
		if len(hook.Command) == 0 {
			return fmt.Errorf("postEdit[%d].command must not be empty", i)
		}
		for j, arg := range hook.Command {
			if arg == "" {
				return fmt.Errorf("postEdit[%d].command[%d] must not be empty", i, j)
			}
			if strings.IndexByte(arg, 0) >= 0 {
				return fmt.Errorf("postEdit[%d].command[%d] contains NUL", i, j)
			}
		}
		if hook.TimeoutSeconds == 0 {
			hook.TimeoutSeconds = 10
		}
		if hook.TimeoutSeconds < 1 || hook.TimeoutSeconds > 60 {
			return fmt.Errorf("postEdit[%d].timeoutSeconds must be between 1 and 60", i)
		}
		for j, extension := range hook.Extensions {
			normalized, err := normalizePostEditExtension(extension)
			if err != nil {
				return fmt.Errorf("postEdit[%d].extensions[%d]: %w", i, j, err)
			}
			hook.Extensions[j] = normalized
		}
	}
	return nil
}

func normalizePostEditExtension(extension string) (string, error) {
	extension = strings.TrimSpace(strings.ToLower(extension))
	if extension == "" {
		return "", fmt.Errorf("extension must not be empty")
	}
	if !strings.HasPrefix(extension, ".") {
		extension = "." + extension
	}
	if len(extension) < 2 || strings.ContainsAny(extension, `/\\`) || strings.IndexByte(extension, 0) >= 0 {
		return "", fmt.Errorf("invalid extension %q", extension)
	}
	for _, r := range extension[1:] {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return "", fmt.Errorf("invalid extension %q", extension)
		}
	}
	return extension, nil
}

// MCPImport selects which claude/codex MCP server definitions ghg imports.
// A nil source entry (or nil Enabled) leaves that source on. Example:
//
//	"mcpImport": {
//	  "codex": { "enabled": true, "exclude": ["node_repl"] }
//	}
type MCPImport struct {
	Claude *MCPImportSource `json:"claude,omitempty"`
	Codex  *MCPImportSource `json:"codex,omitempty"`
}

// MCPImportSource gates one import source. Enabled nil means on; Only, when
// non-empty, is an allowlist of server names; Exclude is a denylist and wins
// over Only when both are set (documented behavior, no validation error).
type MCPImportSource struct {
	Enabled *bool    `json:"enabled,omitempty"`
	Only    []string `json:"only,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// MCPServer is the config-file form of an MCP server entry. It mirrors
// mcp.ServerConfig without importing that package (config is a leaf).
type MCPServer struct {
	Command        []string          `json:"command,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Enabled        *bool             `json:"enabled,omitempty"`
	Note           string            `json:"note,omitempty"`
	StartupTimeout int               `json:"startupTimeout,omitempty"`
	ToolTimeout    int               `json:"toolTimeout,omitempty"`
}

// Dir returns the ghg home directory (~/.ghg), creating it if needed.
// GHG_HOME overrides the location — used by tests to keep fixture writes
// far away from the real config.
func Dir() (string, error) {
	if d := os.Getenv("GHG_HOME"); d != "" {
		return d, os.MkdirAll(d, 0o700)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".ghg")
	return dir, os.MkdirAll(dir, 0o700)
}

func path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// fingerprint summarizes a config for the operation log: enough to spot a
// clobbering write (providers/models collapsing, fixture values appearing)
// without logging secrets.
func (c *Config) fingerprint() string {
	return fmt.Sprintf("providers=%d models=%d default=%q compact=%q",
		len(c.Providers), len(c.Models), c.DefaultModel, c.CompactModel)
}

// Load reads ~/.ghg/config.json, writing a default config on first run. The
// file is JSONC: comments and trailing commas are allowed.
func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		cfg := Default()
		logf("config.load", "missing file, writing defaults (%s)", cfg.fingerprint())
		return cfg, cfg.Save()
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := parseJSONC(data, &cfg); err != nil {
		logf("config.load", "PARSE FAILURE %s: %v (%d bytes)", p, err, len(data))
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if err := cfg.ValidateRoles(); err != nil {
		logf("config.load", "ROLE VALIDATION FAILURE %s: %v", p, err)
		return nil, fmt.Errorf("validate %s: %w", p, err)
	}
	if err := cfg.ValidateExecution(); err != nil {
		logf("config.load", "EXECUTION VALIDATION FAILURE %s: %v", p, err)
		return nil, fmt.Errorf("validate %s: %w", p, err)
	}
	if err := cfg.ValidatePostEdit(); err != nil {
		logf("config.load", "POST-EDIT VALIDATION FAILURE %s: %v", p, err)
		return nil, fmt.Errorf("validate %s: %w", p, err)
	}
	// Recover from a clobbered/empty config: no providers and no models is
	// never a usable state, so prefer the backup, else regenerate defaults —
	// BUT preserve any MCP server/import entries: an mcp-only config is valid
	// (the user may configure servers before providers), and regenerating
	// defaults would silently wipe them.
	if len(cfg.Providers) == 0 && len(cfg.Models) == 0 {
		logf("config.load", "CLOBBERED/EMPTY config detected (%d bytes on disk), attempting recovery", len(data))
		if bak, err := os.ReadFile(p + ".bak"); err == nil {
			var restored Config
			if parseJSONC(bak, &restored) == nil && (len(restored.Providers) > 0 || len(restored.Models) > 0) {
				logf("config.load", "restored from .bak (%s)", restored.fingerprint())
				if len(restored.MCPServers) == 0 && len(cfg.MCPServers) > 0 {
					restored.MCPServers = cfg.MCPServers // keep the user's servers
				}
				if restored.MCPImport == nil {
					restored.MCPImport = cfg.MCPImport // keep import gating too
				}
				if len(restored.PostEdit) == 0 && len(cfg.PostEdit) > 0 {
					restored.PostEdit = cfg.PostEdit // keep trusted hooks too
				}
				return &restored, restored.Save()
			}
		}
		def := Default()
		def.MCPServers = cfg.MCPServers // mcp-only configs are valid; keep them
		def.MCPImport = cfg.MCPImport
		def.PostEdit = cfg.PostEdit
		logf("config.load", "no usable .bak; regenerated defaults (%s), keeping %d mcp entries", def.fingerprint(), len(cfg.MCPServers))
		return def, def.Save()
	}
	logf("config.load", "ok (%s)", cfg.fingerprint())
	return &cfg, nil
}

// Save writes the config back to ~/.ghg/config.json. The write is atomic
// (temp file + rename) and the previous contents are kept in config.json.bak.
// As a safety net, Save refuses to overwrite an existing healthy config (one
// with providers/models) with a structurally empty one — that path has only
// ever been reached by a bug, never intentionally.
func (c *Config) Save() error {
	if err := c.ValidateRoles(); err != nil {
		return err
	}
	if err := c.ValidateExecution(); err != nil {
		return err
	}
	if err := c.ValidatePostEdit(); err != nil {
		return err
	}
	p, err := path()
	if err != nil {
		return err
	}
	if len(c.Providers) == 0 && len(c.Models) == 0 {
		if existing, err := os.ReadFile(p); err == nil {
			var cur Config
			if parseJSONC(existing, &cur) == nil && (len(cur.Providers) > 0 || len(cur.Models) > 0) {
				logf("config.save", "REFUSED empty overwrite of healthy config (disk had providers=%d models=%d)", len(cur.Providers), len(cur.Models))
				return fmt.Errorf("refusing to overwrite %s: existing config has providers/models but the value being saved is empty", p)
			}
		}
	}
	data, err := marshalConfig(c)
	if err != nil {
		return err
	}
	// log the before/after fingerprint so a bad write is attributable
	if existing, err := os.ReadFile(p); err == nil && len(existing) > 0 {
		var cur Config
		if parseJSONC(existing, &cur) == nil {
			logf("config.save", "before=(%s) after=(%s)", cur.fingerprint(), c.fingerprint())
		} else {
			logf("config.save", "before=(unparseable, %d bytes) after=(%s)", len(existing), c.fingerprint())
		}
		// best-effort backup before replacing
		_ = os.WriteFile(p+".bak", existing, 0o600)
	} else {
		logf("config.save", "first write (%s)", c.fingerprint())
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		logf("config.save", "write tmp failed: %v", err)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		logf("config.save", "rename failed: %v", err)
		return err
	}
	return nil
}

// marshalConfig renders the config as JSONC with a header comment.
func marshalConfig(c *Config) ([]byte, error) {
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	header := "// ghg configuration — JSONC: comments and trailing commas are allowed.\n" +
		"// providers: declare each API endpoint once; optional profile selects non-secret YAML metadata.\n" +
		"// models: route each model to one or\n" +
		"// more providers (first is the default). defaultModel/defaultProvider pick the route.\n" +
		"// mcp: ghg's own MCP servers; mcpImport: gate claude/codex imports, e.g.\n" +
		"//   \"mcpImport\": { \"codex\": { \"enabled\": true, \"exclude\": [\"node_repl\"] } }\n"
	out := append([]byte(header), body...)
	return append(out, '\n'), nil
}

// ResolvedRoute contains the canonical provider and model resolution result.
type ResolvedRoute struct {
	Provider     Provider
	Model        Model
	ProviderName string
	ModelName    string
	APIID        string
}

// Resolve picks the provider and API model id for a model name.
// provider may be "" to use the config default routing.
func (c *Config) Resolve(model, provider string) (ResolvedRoute, error) {
	if model == "" {
		model = c.DefaultModel
	}
	m, ok := c.Models[model]
	if !ok {
		// Catalog fallback: a provider-advertised model needs no config entry;
		// config entries stay authoritative overrides when present.
		var err error
		m, provider, err = c.resolveFromCatalog(model, provider)
		if err != nil {
			return ResolvedRoute{}, err
		}
	}
	if provider == "" {
		provider = c.DefaultProvider
	}
	if provider == "" && len(m.Providers) > 0 {
		provider = m.Providers[0]
	}
	p, ok := c.Providers[provider]
	if !ok {
		return ResolvedRoute{}, fmt.Errorf("unknown provider %q (providers: %s)", provider, keys(c.Providers))
	}
	id := m.ID
	if id == "" {
		id = model
	}
	return ResolvedRoute{
		Provider:     p,
		Model:        m,
		ProviderName: provider,
		ModelName:    model,
		APIID:        id,
	}, nil
}

// resolveFromCatalog synthesizes a Model for an id advertised in a provider's
// cached /models catalog but absent from cfg.Models. Capabilities (context,
// max output, vision) come from the catalog entry; the provider routing is the
// catalog's owner. provider may pin the choice ("" scans all providers); when
// several providers advertise the id and none is pinned, it errors naming the
// candidates so the user can disambiguate with -p / a provider argument.
func (c *Config) resolveFromCatalog(model, provider string) (Model, string, error) {
	type hit struct {
		prov string
		mi   *ModelInfoLite
	}
	var hits []hit
	for name, cat := range LoadCatalogs() {
		if provider != "" && name != provider {
			continue
		}
		if _, ok := c.Providers[name]; !ok {
			continue // catalog for a provider no longer configured
		}
		if mi := cat.Find(model); mi != nil {
			hits = append(hits, hit{name, mi})
		}
	}
	if len(hits) == 0 {
		return Model{}, "", fmt.Errorf("unknown model %q (models: %s)", model, keys(c.Models))
	}
	if len(hits) > 1 {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.prov
		}
		return Model{}, "", fmt.Errorf("model %q is advertised by multiple providers (%s); pass a provider to disambiguate (-p / /model %s <provider>)",
			model, strings.Join(names, ", "), model)
	}
	h := hits[0]
	m := Model{
		Providers: []string{h.prov},
		ID:        model,
		Context:   h.mi.ContextLength,
		MaxOut:    h.mi.MaxCompletionTokens,
		Vision:    slices.Contains(h.mi.InputModalities, "image"),
	}
	return m, h.prov, nil
}

func keys[V any](m map[string]V) string {
	keys := slices.Sorted(maps.Keys(m))
	return strings.Join(keys, ", ")
}

// Default returns the first-run config, wired for the built-in default service.
func Default() *Config {
	return &Config{
		DefaultModel: "kimi-k3-fast",
		CompactModel: DefaultCompactModel,
		Providers: map[string]Provider{
			"inference": {
				Name:      "Inference",
				Profile:   "inference",
				BaseURL:   "https://api.inference.net/v1",
				API:       "openai-completions",
				APIKeyEnv: "INFERENCE_API_KEY",
			},
		},
		Models: map[string]Model{
			"kimi-k3":                {Providers: []string{"inference"}, Context: 1048576, Vision: true},
			"kimi-k3-fast":           {Providers: []string{"inference"}, Context: 1048576, Vision: true},
			"glm-5.2-fast":           {Providers: []string{"inference"}, Context: 128000},
			"deepseek-v4-flash-0731": {Providers: []string{"inference"}, Context: 384000},
		},
	}
}
