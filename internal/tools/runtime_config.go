package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/tempdir"
)

type cacheLeaf struct {
	Path        string
	AllowedBase string
	Source      string
}

// NewConfiguredRuntime builds the process-wide execution boundary for one
// workspace. Cache discovery happens once here, during trusted startup; tool
// calls never broaden roots by inspecting the user's home directory.
//
// The returned cleanup removes the private temporary root created for this
// runtime. It is safe to call more than once.
func NewConfiguredRuntime(workspace string, cfg *config.ExecutionConfig, headless bool, postEdit ...[]config.PostEditConfig) (*ToolRuntime, func(), error) {
	mode := sandbox.ModeWorkspaceWrite
	network := sandbox.NetworkDeny
	approval := ApprovalAsk
	if headless {
		approval = ApprovalNever
	}
	if cfg != nil {
		var err error
		mode, err = sandbox.ParseMode(cfg.Sandbox)
		if err != nil {
			return nil, func() {}, err
		}
		network, err = sandbox.ParseNetworkMode(cfg.Network)
		if err != nil {
			return nil, func() {}, err
		}
		if strings.TrimSpace(cfg.Approval) != "" {
			approval, err = ParseApprovalMode(cfg.Approval)
			if err != nil {
				return nil, func() {}, err
			}
		}
	}
	// An interactive human prompt is not available to headless/goal/scheduled
	// runs. Only an explicit auto-review setting can authorize escalation there.
	if headless && approval != ApprovalAutoReview {
		approval = ApprovalNever
	}

	tempRoot, err := os.MkdirTemp(tempdir.Base(), "ghg-runtime-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("execution temp root: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempRoot) }

	envOverrides, err := discoveredCacheEnvironment(tempRoot)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	privateRoot, err := configuredPrivateRoot(nil)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	privateCanonical := ""
	if privateRoot != "" {
		privateCanonical, err = sandbox.CanonicalPath(privateRoot, true)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("private state root: %w", err)
		}
	}
	cacheLeaves := discoveredCacheRoots(envOverrides)
	immutableRoots := discoveredToolchainRoots(envOverrides)
	cacheRoots := make([]string, 0, len(cacheLeaves))
	canonicalCachePaths := make(map[string]string, len(cacheLeaves))
	var readRoots []string
	var writeRoots, configuredTemp []string
	protected := []string{}
	workspaceRoot, err := sandbox.CanonicalPath(workspace, true)
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("workspace root: %w", err)
	}
	if privateCanonical != "" && cacheRootsOverlap(workspaceRoot, privateCanonical) {
		cleanup()
		return nil, func() {}, errors.New("workspace overlaps ghg private state")
	}
	protected = append(protected, filepath.Join(workspaceRoot, ".git"), filepath.Join(workspaceRoot, ".ghg"))
	if skillsRoot := configuredSkillsRoot(privateCanonical); skillsRoot != "" {
		readRoots = append(readRoots, skillsRoot)
	}
	if cfg != nil {
		if err := rejectPrivateRoots(privateCanonical, "read", cfg.ReadRoots); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		if err := rejectPrivateRoots(privateCanonical, "write", cfg.WriteRoots); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		if err := rejectPrivateRoots(privateCanonical, "cache", cfg.CacheRoots); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		if err := rejectPrivateRoots(privateCanonical, "temp", cfg.TempRoots); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		if err := rejectPrivateRoots(privateCanonical, "protected", cfg.ProtectedRoots); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		readRoots = append(readRoots, cfg.ReadRoots...)
		writeRoots = append(writeRoots, cfg.WriteRoots...)
		configuredTemp = append(configuredTemp, cfg.TempRoots...)
	}
	for _, root := range immutableRoots {
		if privateCanonical != "" && cacheRootsOverlap(root, privateCanonical) {
			cleanup()
			return nil, func() {}, fmt.Errorf("immutable root %q overlaps ghg private state", root)
		}
	}
	for _, leaf := range cacheLeaves {
		if privateCanonical != "" {
			canonical, canonicalErr := sandbox.CanonicalPath(leaf.Path, true)
			if canonicalErr == nil && cacheRootsOverlap(canonical, privateCanonical) {
				cleanup()
				return nil, func() {}, fmt.Errorf("cache %s overlaps ghg private state", leaf.Source)
			}
		}
		if validateErr := validateDiscoveredCacheLeaf(leaf.Path, workspaceRoot, protected, immutableRoots, envOverrides); validateErr != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("cache %s: %w", leaf.Source, validateErr)
		}
		canonical, ensureErr := ensureCacheLeaf(leaf.Path, leaf.AllowedBase)
		if ensureErr != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("cache %s: %w", leaf.Source, ensureErr)
		}
		cacheRoots = appendUniquePath(cacheRoots, canonical)
		canonicalCachePaths[filepath.Clean(leaf.Path)] = canonical
	}
	if cfg != nil {
		for _, root := range cfg.CacheRoots {
			if privateCanonical != "" {
				canonical, canonicalErr := sandbox.CanonicalPath(root, true)
				if canonicalErr == nil && cacheRootsOverlap(canonical, privateCanonical) {
					cleanup()
					return nil, func() {}, fmt.Errorf("configured cache root %q overlaps ghg private state", root)
				}
			}
			canonical, validateErr := validateConfiguredCacheRoot(root, workspaceRoot, protected, immutableRoots, envOverrides)
			if validateErr != nil {
				cleanup()
				return nil, func() {}, fmt.Errorf("configured cache root: %w", validateErr)
			}
			cacheRoots = appendUniquePath(cacheRoots, canonical)
		}
	}
	for _, sysTemp := range []string{"/tmp", "/var/tmp"} {
		if canonical, err := sandbox.CanonicalPath(sysTemp, false); err == nil && canonical != "" {
			if privateCanonical == "" || !cacheRootsOverlap(canonical, privateCanonical) {
				configuredTemp = appendUniquePath(configuredTemp, canonical)
			}
		}
	}
	configuredTemp = appendUniquePath(configuredTemp, tempRoot)

	for key, value := range envOverrides {
		if canonical, ok := canonicalCachePaths[filepath.Clean(value)]; ok {
			envOverrides[key] = canonical
		}
	}

	policy, err := sandbox.NewPolicy(sandbox.PolicyConfig{
		Workspace:      workspace,
		Mode:           mode,
		Network:        network,
		BubblewrapPath: executionBubblewrapPath(cfg),
		ReadRoots:      readRoots,
		WriteRoots:     writeRoots,
		CacheRoots:     cacheRoots,
		ImmutableRoots: immutableRoots,
		TempRoots:      configuredTemp,
		ProtectedRoots: protected,
	})
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	runtime, err := NewToolRuntime(policy, approval, headless)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	runtime.TempDir = tempRoot
	runtime.SecretNames = append([]string(nil), executionSecretNames(cfg)...)
	runtime.PostEditHooks = configuredPostEditHooks(postEdit...)
	runtime.envOverrides = envOverrides
	runtime.envOverrides["TMPDIR"] = tempRoot
	runtime.envOverrides["TMP"] = tempRoot
	runtime.envOverrides["TEMP"] = tempRoot
	return runtime, cleanup, nil
}

func configuredPostEditHooks(configs ...[]config.PostEditConfig) []PostEditHook {
	if len(configs) == 0 || len(configs[0]) == 0 {
		return nil
	}
	out := make([]PostEditHook, 0, len(configs[0]))
	for _, hook := range configs[0] {
		timeout := hook.TimeoutSeconds
		if timeout <= 0 {
			timeout = 10
		}
		out = append(out, PostEditHook{
			Command:    append([]string(nil), hook.Command...),
			Extensions: append([]string(nil), hook.Extensions...),
			Timeout:    time.Duration(timeout) * time.Second,
		})
	}
	return out
}

func discoveredCacheRoots(env map[string]string) []cacheLeaf {
	leaves := make(map[string]cacheLeaf)
	add := func(path, base, source string) {
		path = filepath.Clean(strings.TrimSpace(path))
		base = filepath.Clean(strings.TrimSpace(base))
		if path == "." || path == "" || base == "." || base == "" {
			return
		}
		candidate := cacheLeaf{Path: path, AllowedBase: base, Source: source}
		if previous, ok := leaves[path]; !ok || candidate.Source < previous.Source || candidate.AllowedBase < previous.AllowedBase {
			leaves[path] = candidate
		}
	}
	addEnvLeaf := func(key, source string) {
		value := envValue(env, key)
		if value != "" && value != "off" {
			add(value, filepath.Dir(value), source)
		}
	}
	addEnvLeaf("GOCACHE", "Go build cache")
	addEnvLeaf("GOMODCACHE", "Go module cache")
	for _, gopath := range splitPathList(envValue(env, "GOPATH")) {
		add(filepath.Join(gopath, "pkg", "sumdb"), gopath, "Go checksum database")
	}

	home := effectiveHome(env)
	cargoHome := effectiveHomePath(env, "CARGO_HOME", home, ".cargo")
	rustupHome := effectiveHomePath(env, "RUSTUP_HOME", home, ".rustup")
	bunInstall := effectiveHomePath(env, "BUN_INSTALL", home, ".bun")
	if cargoHome != "" {
		add(filepath.Join(cargoHome, "registry"), cargoHome, "Cargo registry cache")
		add(filepath.Join(cargoHome, "git"), cargoHome, "Cargo git cache")
	}
	if rustupHome != "" {
		add(filepath.Join(rustupHome, "downloads"), rustupHome, "Rustup download cache")
	}
	if bunInstall != "" {
		add(filepath.Join(bunInstall, "install", "cache"), bunInstall, "Bun install cache")
	}
	if value := envValue(env, "NPM_CONFIG_CACHE"); value == "off" {
		// An explicit disabled cache must remain disabled.
	} else if value != "" {
		add(value, filepath.Dir(value), "npm cache")
	} else if home != "" {
		add(filepath.Join(home, ".npm"), home, "npm cache")
	}

	out := make([]cacheLeaf, 0, len(leaves))
	for _, leaf := range leaves {
		out = append(out, leaf)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].AllowedBase != out[j].AllowedBase {
			return out[i].AllowedBase < out[j].AllowedBase
		}
		return out[i].Source < out[j].Source
	})
	return out
}

func configuredPrivateRoot(env map[string]string) (string, error) {
	root := strings.TrimSpace(envValue(env, "GHG_HOME"))
	if root == "" {
		home := effectiveHome(env)
		if home == "" {
			return "", nil
		}
		root = filepath.Join(home, ".ghg")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("private state root %q: %w", root, err)
	}
	return filepath.Clean(abs), nil
}

func configuredSkillsRoot(privateRoot string) string {
	if privateRoot == "" {
		return ""
	}
	path := filepath.Join(privateRoot, "skills")
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	canonical, err := sandbox.CanonicalPath(path, false)
	if err != nil || !strictPathWithin(canonical, privateRoot) {
		return ""
	}
	return canonical
}

func rejectPrivateRoots(privateRoot, kind string, roots []string) error {
	if privateRoot == "" {
		return nil
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		canonical, err := sandbox.CanonicalPath(root, true)
		if err != nil {
			continue // NewPolicy reports the original invalid root below.
		}
		if cacheRootsOverlap(canonical, privateRoot) {
			return fmt.Errorf("configured %s root %q overlaps ghg private state", kind, root)
		}
	}
	return nil
}

func discoveredToolchainRoots(env map[string]string) []string {
	home := effectiveHome(env)
	cargoHome := effectiveHomePath(env, "CARGO_HOME", home, ".cargo")
	rustupHome := effectiveHomePath(env, "RUSTUP_HOME", home, ".rustup")
	bunInstall := effectiveHomePath(env, "BUN_INSTALL", home, ".bun")
	var candidates []string
	if cargoHome != "" {
		candidates = append(candidates, filepath.Join(cargoHome, "bin"))
	}
	if rustupHome != "" {
		candidates = append(candidates, filepath.Join(rustupHome, "toolchains"))
	}
	if bunInstall != "" {
		candidates = append(candidates, filepath.Join(bunInstall, "bin"))
	}
	for _, gopath := range splitPathList(envValue(env, "GOPATH")) {
		candidates = append(candidates, filepath.Join(gopath, "bin"))
	}
	if goroot := strings.TrimSpace(runtime.GOROOT()); goroot != "" {
		candidates = append(candidates, goroot)
	}

	roots := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		canonical, err := sandbox.CanonicalPath(candidate, false)
		if err != nil {
			continue
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			continue
		}
		roots[filepath.Clean(canonical)] = struct{}{}
	}
	out := make([]string, 0, len(roots))
	for root := range roots {
		out = append(out, root)
	}
	sort.Strings(out)
	return out
}

func discoveredCacheEnvironment(tempRoot string) (map[string]string, error) {
	effective := make(map[string]string)
	// Relative cache values are redirected into this run's private root so
	// tools cannot create cache directories in the workspace.
	tempCachePath := func(key string, index ...int) string {
		path := filepath.Join(tempRoot, "cache", strings.ToLower(key))
		if len(index) > 0 {
			path = filepath.Join(path, fmt.Sprintf("%d", index[0]))
		}
		return path
	}
	setPath := func(key, value string) error {
		if value == "" {
			return nil
		}
		if value == "off" {
			effective[key] = value
			return nil
		}
		if !filepath.IsAbs(value) {
			value = tempCachePath(key)
		}
		canonical, err := sandbox.CanonicalPath(value, true)
		if err != nil {
			return fmt.Errorf("cache path %s=%q: %w", key, value, err)
		}
		effective[key] = canonical
		return nil
	}
	setPathList := func(key, value string) error {
		parts := splitPathList(value)
		if len(parts) == 0 {
			return nil
		}
		canonical := make([]string, 0, len(parts))
		for i, part := range parts {
			if !filepath.IsAbs(part) {
				part = tempCachePath(key, i)
			}
			resolved, err := sandbox.CanonicalPath(part, true)
			if err != nil {
				return fmt.Errorf("cache path %s=%q: %w", key, part, err)
			}
			canonical = append(canonical, resolved)
		}
		value = strings.Join(canonical, string(os.PathListSeparator))
		effective[key] = value
		return nil
	}
	home := effectiveHome(nil)
	cache, cacheErr := os.UserCacheDir()
	if cacheErr != nil && home != "" {
		cache = filepath.Join(home, ".cache")
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" && home != "" {
		gopath = filepath.Join(home, "go")
	}
	if err := setPathList("GOPATH", gopath); err != nil {
		return nil, err
	}
	if canonical := envValue(effective, "GOPATH"); canonical != "" {
		gopath = canonical
	}
	if cache != "" {
		gocache := os.Getenv("GOCACHE")
		if gocache == "" {
			gocache = filepath.Join(cache, "go-build")
		}
		if err := setPath("GOCACHE", gocache); err != nil {
			return nil, err
		}
	}
	gomodcache := os.Getenv("GOMODCACHE")
	if gomodcache == "" {
		if first := splitPathList(gopath); len(first) > 0 {
			gomodcache = filepath.Join(first[0], "pkg", "mod")
		}
	}
	if gomodcache != "" {
		if err := setPath("GOMODCACHE", gomodcache); err != nil {
			return nil, err
		}
	}
	for _, setting := range []struct{ key, value string }{
		{key: "CARGO_HOME", value: homePath(home, ".cargo")},
		{key: "RUSTUP_HOME", value: homePath(home, ".rustup")},
		{key: "BUN_INSTALL", value: homePath(home, ".bun")},
		{key: "NPM_CONFIG_CACHE", value: homePath(home, ".npm")},
	} {
		value := os.Getenv(setting.key)
		if value == "" {
			value = setting.value
		}
		if err := setPath(setting.key, value); err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"XDG_CACHE_HOME"} {
		if value := os.Getenv(key); value != "" {
			if err := setPath(key, value); err != nil {
				return nil, err
			}
		}
	}
	if goroot := strings.TrimSpace(runtime.GOROOT()); goroot != "" {
		if err := setPath("GOROOT", goroot); err != nil {
			return nil, err
		}
	}
	return effective, nil
}

func envValue(overrides map[string]string, key string) string {
	if value, ok := overrides[key]; ok {
		return value
	}
	return os.Getenv(key)
}

func effectiveHome(env map[string]string) string {
	if home := strings.TrimSpace(envValue(env, "HOME")); home != "" {
		return filepath.Clean(home)
	}
	home, _ := os.UserHomeDir()
	home = strings.TrimSpace(home)
	if home == "" {
		return ""
	}
	return filepath.Clean(home)
}

func effectiveHomePath(env map[string]string, key, home, suffix string) string {
	if value := strings.TrimSpace(envValue(env, key)); value != "" {
		return filepath.Clean(value)
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, suffix)
}

func homePath(home, suffix string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, suffix)
}

func appendUniquePath(dst []string, values ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(values))
	for _, value := range dst {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		dst = append(dst, value)
	}
	return dst
}

func ensureCacheLeaf(path, allowedBase string) (string, error) {
	path = strings.TrimSpace(path)
	allowedBase = strings.TrimSpace(allowedBase)
	if path == "" || allowedBase == "" {
		return "", fmt.Errorf("cache leaf and allowed base are required")
	}
	if !filepath.IsAbs(path) || !filepath.IsAbs(allowedBase) {
		return "", fmt.Errorf("cache leaf and allowed base must be absolute")
	}
	path = filepath.Clean(path)
	allowedBase = filepath.Clean(allowedBase)
	base, err := canonicalNearestExistingDir(allowedBase)
	if err != nil {
		return "", fmt.Errorf("allowed base %q: %w", allowedBase, err)
	}
	canonical, err := sandbox.CanonicalPath(path, true)
	if err != nil {
		return "", fmt.Errorf("cache leaf %q: %w", path, err)
	}
	if !strictPathWithin(canonical, base) {
		return "", fmt.Errorf("cache leaf %q is not a strict descendant of allowed base %q", path, base)
	}
	if broadCacheRoot(canonical) {
		return "", fmt.Errorf("cache leaf %q is too broad", path)
	}
	if err := validateCacheComponents(path, allowedBase, base); err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("create cache leaf %q: %w", path, err)
	}
	if err := validateCacheComponents(path, allowedBase, base); err != nil {
		return "", err
	}
	canonical, err = sandbox.CanonicalPath(path, true)
	if err != nil {
		return "", fmt.Errorf("canonicalize cache leaf %q: %w", path, err)
	}
	if !strictPathWithin(canonical, base) {
		return "", fmt.Errorf("cache leaf %q escaped allowed base %q", path, base)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat cache leaf %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cache leaf %q is not a directory", path)
	}
	return canonical, nil
}

func canonicalNearestExistingDir(name string) (string, error) {
	name = filepath.Clean(name)
	missing := []string{}
	current := name
	for {
		_, err := os.Lstat(current)
		if err == nil {
			canonical, err := sandbox.CanonicalPath(current, false)
			if err != nil {
				return "", err
			}
			resolvedInfo, err := os.Stat(canonical)
			if err != nil {
				return "", err
			}
			if !resolvedInfo.IsDir() {
				return "", fmt.Errorf("%q is not a directory", current)
			}
			for i := len(missing) - 1; i >= 0; i-- {
				canonical = filepath.Join(canonical, missing[i])
			}
			return filepath.Clean(canonical), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("cannot resolve %q", name)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func validateCacheComponents(path, allowedBase, canonicalBase string) error {
	rel, err := filepath.Rel(allowedBase, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("cache leaf %q is not beneath allowed base %q", path, allowedBase)
	}
	var components []string
	current := path
	for current != allowedBase {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("cache leaf %q has no usable allowed base", path)
		}
		current = parent
	}
	for i := len(components) - 1; i >= 0; i-- {
		current := components[i]
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return fmt.Errorf("inspect cache component %q: %w", current, err)
		}
		resolved := current
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err = filepath.EvalSymlinks(current)
			if err != nil {
				return fmt.Errorf("resolve cache component %q: %w", current, err)
			}
			if !pathEqualOrWithin(filepath.Clean(resolved), canonicalBase) {
				return fmt.Errorf("cache component %q resolves outside allowed base", current)
			}
			info, err = os.Stat(resolved)
			if err != nil {
				return fmt.Errorf("stat cache component %q: %w", current, err)
			}
		}
		if !info.IsDir() {
			return fmt.Errorf("cache component %q is not a directory", current)
		}
		if !cachePathOwned(info) {
			return fmt.Errorf("cache component %q has unexpected ownership", current)
		}
	}
	return nil
}

func validateConfiguredCacheRoot(root, workspace string, protected, immutable []string, env map[string]string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("cache root is empty")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("cache root %q must be absolute", root)
	}
	candidate, err := sandbox.CanonicalPath(root, true)
	if err != nil {
		return "", err
	}
	if broadCacheRoot(candidate) {
		return "", fmt.Errorf("cache root %q is too broad", root)
	}
	home := effectiveHome(env)
	sensitiveHomes := []string{home, effectiveHomePath(env, "CARGO_HOME", home, ".cargo"), effectiveHomePath(env, "RUSTUP_HOME", home, ".rustup"), effectiveHomePath(env, "BUN_INSTALL", home, ".bun")}
	for _, sensitive := range sensitiveHomes {
		if sensitive == "" {
			continue
		}
		resolved, resolveErr := sandbox.CanonicalPath(sensitive, true)
		if resolveErr == nil && pathEqualOrAncestor(candidate, resolved) {
			return "", fmt.Errorf("cache root %q is equal to or contains protected home %q", root, resolved)
		}
	}
	if candidate == workspace {
		return "", fmt.Errorf("cache root %q is the workspace root", root)
	}
	for _, forbidden := range append(append([]string(nil), protected...), immutable...) {
		if forbidden == "" {
			continue
		}
		resolved, resolveErr := sandbox.CanonicalPath(forbidden, true)
		if resolveErr == nil && cacheRootsOverlap(candidate, resolved) {
			return "", fmt.Errorf("cache root %q overlaps protected root %q", root, resolved)
		}
	}
	canonical, err := ensureCacheLeaf(root, filepath.Dir(filepath.Clean(root)))
	if err != nil {
		return "", err
	}
	return canonical, nil
}

func validateDiscoveredCacheLeaf(path, workspace string, protected, immutable []string, env map[string]string) error {
	candidate, err := sandbox.CanonicalPath(path, true)
	if err != nil {
		return err
	}
	if broadCacheRoot(candidate) {
		return fmt.Errorf("cache leaf %q is too broad", path)
	}
	home := effectiveHome(env)
	sensitiveHomes := []string{home, effectiveHomePath(env, "CARGO_HOME", home, ".cargo"), effectiveHomePath(env, "RUSTUP_HOME", home, ".rustup"), effectiveHomePath(env, "BUN_INSTALL", home, ".bun")}
	for _, sensitive := range sensitiveHomes {
		if sensitive == "" {
			continue
		}
		resolved, resolveErr := sandbox.CanonicalPath(sensitive, true)
		if resolveErr == nil && pathEqualOrAncestor(candidate, resolved) {
			return fmt.Errorf("cache leaf %q is equal to or contains protected home %q", path, resolved)
		}
	}
	if candidate == workspace {
		return fmt.Errorf("cache leaf %q is the workspace root", path)
	}
	for _, forbidden := range append(append([]string(nil), protected...), immutable...) {
		if forbidden == "" {
			continue
		}
		resolved, resolveErr := sandbox.CanonicalPath(forbidden, true)
		if resolveErr == nil && cacheRootsOverlap(candidate, resolved) {
			return fmt.Errorf("cache leaf %q overlaps protected root %q", path, resolved)
		}
	}
	return nil
}

func broadCacheRoot(path string) bool {
	path = filepath.Clean(path)
	if path == string(filepath.Separator) || (filepath.VolumeName(path) != "" && path == filepath.VolumeName(path)+string(filepath.Separator)) {
		return true
	}
	if home := effectiveHome(nil); home != "" {
		if resolved, err := sandbox.CanonicalPath(home, true); err == nil && pathEqualOrAncestor(path, resolved) {
			return true
		}
	}
	for _, broad := range []string{"/Users", "/home", "/root", "/var", "/tmp", "/private"} {
		resolved := filepath.Clean(broad)
		if candidate, err := sandbox.CanonicalPath(resolved, false); err == nil {
			resolved = candidate
		}
		if path == resolved {
			return true
		}
	}
	return false
}

func pathEqualOrWithin(path, root string) bool {
	return path == root || strictPathWithin(path, root)
}

func pathEqualOrAncestor(path, descendant string) bool {
	return path == descendant || strictPathWithin(descendant, path)
}

func strictPathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func cacheRootsOverlap(left, right string) bool {
	return pathEqualOrWithin(left, right) || pathEqualOrWithin(right, left)
}

func executionBubblewrapPath(cfg *config.ExecutionConfig) string {
	if cfg == nil {
		return ""
	}
	return cfg.BubblewrapPath
}

func executionSecretNames(cfg *config.ExecutionConfig) []string {
	if cfg == nil {
		return nil
	}
	return cfg.SecretNames
}

func splitPathList(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, string(os.PathListSeparator))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}
