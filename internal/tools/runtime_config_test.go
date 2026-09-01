package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	goRuntime "runtime"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/sandbox"
)

func TestNewConfiguredRuntimeProvidesPrivateTempAndDefaultCaches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GHG_HOME", filepath.Join(home, ".ghg"))
	for _, key := range []string{"GOPATH", "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "BUN_INSTALL", "NPM_CONFIG_CACHE", "XDG_CACHE_HOME"} {
		t.Setenv(key, "")
	}

	runtime, cleanup, err := NewConfiguredRuntime(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if runtime.TempDir == "" {
		t.Fatal("runtime did not provision a private temp directory")
	}
	if info, err := os.Stat(runtime.TempDir); err != nil || !info.IsDir() {
		t.Fatalf("private temp directory=%q err=%v", runtime.TempDir, err)
	}

	env := make(map[string]string)
	for _, pair := range runtime.ChildEnv(nil) {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			env[key] = value
		}
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if env[key] != runtime.TempDir {
			t.Fatalf("child %s=%q, want private temp %q", key, env[key], runtime.TempDir)
		}
	}
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "BUN_INSTALL", "NPM_CONFIG_CACHE"} {
		root := env[key]
		if root == "" {
			t.Fatalf("child cache/tool home %s is unset", key)
		}
		if key == "GOCACHE" || key == "GOMODCACHE" || key == "NPM_CONFIG_CACHE" {
			if info, err := os.Stat(root); err != nil || !info.IsDir() {
				t.Fatalf("child cache %s=%q err=%v", key, root, err)
			}
		}
	}

	gopath := strings.Split(env["GOPATH"], string(os.PathListSeparator))[0]
	expected := []string{
		env["GOCACHE"], env["GOMODCACHE"],
		filepath.Join(env["CARGO_HOME"], "registry"), filepath.Join(env["CARGO_HOME"], "git"),
		filepath.Join(env["RUSTUP_HOME"], "downloads"), filepath.Join(env["BUN_INSTALL"], "install", "cache"),
		env["NPM_CONFIG_CACHE"], filepath.Join(gopath, "pkg", "sumdb"),
	}
	for _, want := range expected {
		canonical, err := filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatal(err)
		}
		if !containsPath(runtime.Policy.CacheRoots(), canonical) {
			t.Fatalf("policy does not contain expected cache leaf %q: %v", canonical, runtime.Policy.CacheRoots())
		}
	}
	for _, cacheRoot := range runtime.Policy.CacheRoots() {
		if containsPath(runtime.Policy.WriteRoots(), cacheRoot) {
			t.Fatalf("cache root %q was folded into ordinary write roots", cacheRoot)
		}
	}
}

func TestNewConfiguredRuntimeRedirectsRelativeScratchPathsAndCleans(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GHG_HOME", filepath.Join(home, ".ghg"))
	t.Setenv("TMPDIR", ".")
	t.Setenv("TMP", ".")
	t.Setenv("TEMP", ".")
	t.Setenv("GOPATH", ".gopath-one"+string(os.PathListSeparator)+".gopath-two")
	t.Setenv("GOCACHE", ".gocache")
	t.Setenv("GOMODCACHE", ".gomodcache")
	t.Setenv("CARGO_HOME", ".cargo")
	t.Setenv("RUSTUP_HOME", ".rustup")
	t.Setenv("BUN_INSTALL", ".bun")
	t.Setenv("NPM_CONFIG_CACHE", ".npm")
	t.Setenv("XDG_CACHE_HOME", ".xdg-cache")

	rt, cleanup, err := NewConfiguredRuntime(workspace, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(rt.TempDir) || pathEqualOrWithin(filepath.Clean(rt.TempDir), filepath.Clean(wd)) {
		t.Fatalf("runtime temp directory=%q must be outside the working directory", rt.TempDir)
	}
	canonicalTemp, err := sandbox.CanonicalPath(rt.TempDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !containsPath(rt.Policy.TempRoots(), canonicalTemp) {
		t.Fatalf("policy does not allow runtime temp root %q: %v", canonicalTemp, rt.Policy.TempRoots())
	}
	env := make(map[string]string)
	for _, pair := range rt.ChildEnv(nil) {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			env[key] = value
		}
	}
	for _, key := range []string{"GOPATH", "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "BUN_INSTALL", "NPM_CONFIG_CACHE", "XDG_CACHE_HOME"} {
		for _, value := range splitPathList(env[key]) {
			if !pathEqualOrWithin(filepath.Clean(value), filepath.Clean(rt.TempDir)) {
				t.Fatalf("child %s=%q escaped runtime temp %q", key, value, rt.TempDir)
			}
			if _, err := rt.Policy.Authorize(filepath.Join(value, "probe"), sandbox.AccessWrite, true); err != nil {
				t.Fatalf("policy rejected child %s scratch path %q: %v", key, value, err)
			}
		}
	}
	cleanup()
	cleanup()
	if _, err := os.Stat(rt.TempDir); !os.IsNotExist(err) {
		t.Fatalf("runtime temp directory=%q still exists after cleanup: %v", rt.TempDir, err)
	}
}

func TestCacheRootsAreNarrowAndSiblingStateIsProtected(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	gopathOne := filepath.Join(home, "go-one")
	gopathTwo := filepath.Join(home, "go-two")
	cargoHome := filepath.Join(home, ".cargo-custom")
	rustupHome := filepath.Join(home, ".rustup-custom")
	bunInstall := filepath.Join(home, ".bun-custom")
	npmCache := filepath.Join(home, "npm-cache")
	for _, dir := range []string{
		filepath.Join(cargoHome, "bin"), filepath.Join(cargoHome, "registry"), filepath.Join(cargoHome, "git"),
		filepath.Join(rustupHome, "toolchains", "stable", "bin"), filepath.Join(rustupHome, "downloads"),
		filepath.Join(bunInstall, "bin"), filepath.Join(bunInstall, "install", "cache"),
		filepath.Join(gopathOne, "bin"), filepath.Join(gopathOne, "pkg", "mod"),
		filepath.Join(gopathTwo, "bin"), filepath.Join(gopathTwo, "pkg", "mod"), npmCache,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{
		filepath.Join(cargoHome, "credentials.toml"), filepath.Join(cargoHome, "config.toml"),
		filepath.Join(rustupHome, "settings.toml"), filepath.Join(bunInstall, "bin", "bun"),
		filepath.Join(gopathOne, "bin", "go-tool"), filepath.Join(home, "npm-adjacent"),
	} {
		if err := os.WriteFile(file, []byte("sentinel"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("GHG_HOME", filepath.Join(home, ".ghg"))
	t.Setenv("GOPATH", gopathOne+string(os.PathListSeparator)+gopathTwo)
	t.Setenv("GOCACHE", filepath.Join(home, "go-build"))
	t.Setenv("GOMODCACHE", filepath.Join(home, "go-mod"))
	t.Setenv("CARGO_HOME", cargoHome)
	t.Setenv("RUSTUP_HOME", rustupHome)
	t.Setenv("BUN_INSTALL", bunInstall)
	t.Setenv("NPM_CONFIG_CACHE", npmCache)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg-cache"))

	rt, cleanup, err := NewConfiguredRuntime(workspace, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	expectedCache := []string{
		filepath.Join(home, "go-build"), filepath.Join(home, "go-mod"),
		filepath.Join(gopathOne, "pkg", "sumdb"), filepath.Join(gopathTwo, "pkg", "sumdb"),
		filepath.Join(cargoHome, "registry"), filepath.Join(cargoHome, "git"),
		filepath.Join(rustupHome, "downloads"), filepath.Join(bunInstall, "install", "cache"), npmCache,
	}
	if got := len(rt.Policy.CacheRoots()); got != len(expectedCache) {
		t.Fatalf("cache roots=%v, want %d narrow leaves", rt.Policy.CacheRoots(), len(expectedCache))
	}
	for _, want := range expectedCache {
		canonical, err := filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatal(err)
		}
		if !containsPath(rt.Policy.CacheRoots(), canonical) {
			t.Fatalf("cache leaf %q missing from %v", canonical, rt.Policy.CacheRoots())
		}
	}
	for _, broad := range []string{home, cargoHome, rustupHome, bunInstall, gopathOne, gopathTwo} {
		canonical, err := filepath.EvalSymlinks(broad)
		if err != nil {
			t.Fatal(err)
		}
		for _, cacheRoot := range rt.Policy.CacheRoots() {
			if cacheRoot == canonical {
				t.Fatalf("broad state root %q was granted as a cache", cacheRoot)
			}
		}
	}

	for _, immutable := range []string{
		filepath.Join(cargoHome, "bin"), filepath.Join(rustupHome, "toolchains"), filepath.Join(bunInstall, "bin"),
		filepath.Join(gopathOne, "bin"), filepath.Join(gopathTwo, "bin"),
	} {
		canonical, err := filepath.EvalSymlinks(immutable)
		if err != nil {
			t.Fatal(err)
		}
		if !containsPath(rt.Policy.ImmutableRoots(), canonical) {
			t.Fatalf("immutable root %q missing from %v", canonical, rt.Policy.ImmutableRoots())
		}
		if _, err := rt.Policy.Authorize(filepath.Join(immutable, "new-file"), sandbox.AccessWrite, true); err == nil {
			t.Fatalf("write beneath immutable root %q was authorized", immutable)
		}
		if _, err := rt.Policy.Authorize(immutable, sandbox.AccessRead, false); err != nil {
			t.Fatalf("read beneath immutable root %q was denied: %v", immutable, err)
		}
	}
	for _, path := range []string{
		filepath.Join(cargoHome, "credentials.toml"), filepath.Join(cargoHome, "config.toml"),
		filepath.Join(rustupHome, "settings.toml"), filepath.Join(bunInstall, "bin", "bun"),
		filepath.Join(gopathOne, "bin", "go-tool"), filepath.Join(home, "npm-adjacent"),
	} {
		if _, err := rt.Policy.Authorize(path, sandbox.AccessWrite, false); err == nil {
			t.Fatalf("sibling state write %q was authorized", path)
		}
	}
	for _, cacheRoot := range rt.Policy.CacheRoots() {
		if _, err := rt.Policy.Authorize(filepath.Join(cacheRoot, "write-me"), sandbox.AccessWrite, true); err != nil {
			t.Fatalf("cache write %q was denied: %v", cacheRoot, err)
		}
	}
	t.Setenv("GOCACHE", cargoHome)
	if _, cleanup, err := NewConfiguredRuntime(workspace, nil, false); err == nil {
		cleanup()
		t.Fatal("GOCACHE equal to Cargo home was accepted")
	}
	t.Setenv("GOCACHE", filepath.Join(home, "go-build"))
	if _, cleanup, err := NewConfiguredRuntime(workspace, &config.ExecutionConfig{CacheRoots: []string{home}}, false); err == nil {
		cleanup()
		t.Fatal("explicit HOME cache root was accepted")
	}
}

func TestEnsureCacheLeafRejectsUnsafeRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, "base")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(base, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	safeTarget := filepath.Join(base, "safe-target")
	if err := os.Mkdir(safeTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	safeLink := filepath.Join(base, "safe-link")
	if err := os.Symlink(safeTarget, safeLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cases := []struct {
		name, path, allowedBase string
		wantErr                 bool
	}{
		{"root", string(filepath.Separator), base, true},
		{"home", home, base, true},
		{"home ancestor", filepath.Dir(home), base, true},
		{"broad /Users", "/Users", string(filepath.Separator), true},
		{"broad /home", "/home", string(filepath.Separator), true},
		{"broad /root", "/root", string(filepath.Separator), true},
		{"broad /var", "/var", string(filepath.Separator), true},
		{"broad /tmp", "/tmp", string(filepath.Separator), true},
		{"base equality", base, base, true},
		{"file component", file, base, true},
		{"escaping symlink", filepath.Join(escape, "cache"), base, true},
		{"safe symlink", filepath.Join(safeLink, "cache"), base, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ensureCacheLeaf(tc.path, tc.allowedBase)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ensureCacheLeaf(%q, %q) error=%v, wantErr=%v", tc.path, tc.allowedBase, err, tc.wantErr)
			}
		})
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func TestCachedGoTestStaysWithinTheRoutinePolicy(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalNever)
	policy, covered, err := runtime.authorizeCommand(context.Background(), "bash", "go test ./...", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if covered || policy != runtime.Policy {
		t.Fatalf("cached go test policy=%p covered=%v, want session policy and no approval", policy, covered)
	}
}

func TestNewConfiguredRuntimeCanonicalizesConfiguredCachePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GHG_HOME", filepath.Join(home, ".ghg"))
	realCache := filepath.Join(home, "cache-real")
	if err := os.Mkdir(realCache, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "cache-link")
	if err := os.Symlink(realCache, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("GOCACHE", link)
	t.Setenv("GOMODCACHE", link)

	runtime, cleanup, err := NewConfiguredRuntime(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	canonical, err := filepath.EvalSymlinks(realCache)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range runtime.ChildEnv(nil) {
		key, value, ok := strings.Cut(pair, "=")
		if ok && (key == "GOCACHE" || key == "GOMODCACHE") && value != canonical {
			t.Fatalf("child %s=%q, want canonical %q", key, value, canonical)
		}
	}
	for _, root := range runtime.Policy.CacheRoots() {
		if root == canonical {
			return
		}
	}
	t.Fatalf("canonical cache root %q missing from %v", canonical, runtime.Policy.CacheRoots())
}

func TestConfigDirectoryIsNotReadableByToolsOrChildren(t *testing.T) {
	home := t.TempDir()
	ghgHome := filepath.Join(home, ".ghg")
	if err := os.MkdirAll(ghgHome, 0o700); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(ghgHome, "skills", "fixture", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("skill-sentinel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinelConfig := filepath.Join(ghgHome, "config.json")
	configJSON := `{
		"defaultModel": "custom-model",
		"providers": {
			"custom": {
				"name": "custom",
				"baseUrl": "https://api.custom.test/v1",
				"apiKey": "sentinel-literal-key",
				"apiKeyEnv": "SENTINEL_VAR"
			}
		}
	}`
	if err := os.WriteFile(sentinelConfig, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionCreds := filepath.Join(ghgHome, "session_credentials.json")
	if err := os.WriteFile(sessionCreds, []byte(`{"token":"sentinel-session-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("GHG_HOME", ghgHome)
	t.Setenv("SENTINEL_VAR", "sentinel-referenced-secret")
	for _, key := range []string{"GOPATH", "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "NPM_CONFIG_CACHE"} {
		t.Setenv(key, "")
	}

	// 1. Trusted config loading within the ghg application continues to work.
	loadedCfg, err := config.Load()
	if err != nil {
		t.Fatalf("trusted config.Load failed: %v", err)
	}
	provider, ok := loadedCfg.Providers["custom"]
	if !ok || provider.APIKey != "sentinel-literal-key" {
		t.Fatalf("loaded Provider=%+v, want apiKey=sentinel-literal-key", provider)
	}
	if resolvedKey := provider.Key(); resolvedKey != "sentinel-referenced-secret" {
		t.Fatalf("resolved key=%q, want sentinel-referenced-secret", resolvedKey)
	}

	workspace := t.TempDir()
	rt, cleanup, err := NewConfiguredRuntime(workspace, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// 2. The private state root is absent from every capability list. Only
	// ~/.ghg/skills is exposed, and only as a read root.
	canonical, err := filepath.EvalSymlinks(ghgHome)
	if err != nil {
		t.Fatal(err)
	}
	canonicalConfig, err := filepath.EvalSymlinks(sentinelConfig)
	if err != nil {
		t.Fatal(err)
	}
	canonicalCreds, err := filepath.EvalSymlinks(sessionCreds)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSkill, err := filepath.EvalSymlinks(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSkills, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(skillPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !containsPath(rt.Policy.ReadRoots(), canonicalSkills) {
		t.Fatalf("user skills root %q is missing from read roots: %v", canonicalSkills, rt.Policy.ReadRoots())
	}
	for kind, roots := range map[string][]string{
		"read": rt.Policy.ReadRoots(), "write": rt.Policy.WriteRoots(), "cache": rt.Policy.CacheRoots(),
		"temp": rt.Policy.TempRoots(), "immutable": rt.Policy.ImmutableRoots(), "protected": rt.Policy.ProtectedRoots(),
	} {
		for _, root := range roots {
			if root == canonicalSkills && kind == "read" {
				continue
			}
			if cacheRootsOverlap(root, canonical) {
				t.Fatalf("private state root overlaps %s root %q", kind, root)
			}
		}
	}

	// 3. Native read tool must deny access to config and session credentials.
	ctx := WithRuntime(context.Background(), rt)
	readConfig := ExecuteResult(ctx, All(), "read", json.RawMessage(`{"path":"`+sentinelConfig+`"}`))
	if !strings.Contains(readConfig.Preview, "outside the execution roots") {
		t.Fatalf("native read of config was not denied: %q", readConfig.Preview)
	}
	readCreds := ExecuteResult(ctx, All(), "read", json.RawMessage(`{"path":"`+sessionCreds+`"}`))
	if !strings.Contains(readCreds.Preview, "outside the execution roots") {
		t.Fatalf("native read of session credentials was not denied: %q", readCreds.Preview)
	}
	readSkill := ExecuteResult(ctx, All(), "read", json.RawMessage(`{"path":"`+skillPath+`"}`))
	if !strings.Contains(readSkill.Preview, "skill-sentinel") {
		t.Fatalf("native read of user skill was not allowed: %q", readSkill.Preview)
	}

	// 4. Sandboxed children must not read private state, while the narrow skill
	// root remains readable. A wrapping error is never treated as a pass.
	if goRuntime.GOOS == "darwin" || goRuntime.GOOS == "linux" {
		requireRuntimeBackend(t, rt, workspace)
		runCat := func(path string) ([]byte, error) {
			t.Helper()
			wrapped, wrapErr := rt.WrapCommand(sandbox.CommandSpec{
				Program: "/bin/cat", Args: []string{path}, Dir: workspace,
				Env: []string{"PATH=/usr/bin:/bin"},
			})
			if wrapErr != nil {
				t.Fatal(wrapErr)
			}
			cmd := exec.Command(wrapped.Program, wrapped.Args...)
			cmd.Dir = wrapped.Dir
			cmd.Env = wrapped.Env
			return cmd.CombinedOutput()
		}
		for _, path := range []string{canonicalConfig, canonicalCreds} {
			output, runErr := runCat(path)
			for _, sentinel := range []string{"sentinel-literal-key", "sentinel-referenced-secret", "sentinel-session-token"} {
				if strings.Contains(string(output), sentinel) {
					t.Fatalf("sandboxed child leaked %q from %s with err=%v: %s", sentinel, path, runErr, output)
				}
			}
			if runErr == nil {
				t.Fatalf("sandboxed child read private state %s", path)
			}
		}
		output, runErr := runCat(canonicalSkill)
		if runErr != nil || !strings.Contains(string(output), "skill-sentinel") {
			t.Fatalf("sandboxed child skill read output=%q err=%v", output, runErr)
		}
	}
}

func requireRuntimeBackend(t *testing.T, rt *ToolRuntime, workspace string) {
	t.Helper()
	wrapped, err := rt.WrapCommand(sandbox.CommandSpec{
		Program: "/usr/bin/true", Dir: workspace, Env: []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		if errors.Is(err, sandbox.ErrSandboxUnavailable) && os.Getenv("GHG_REQUIRE_SANDBOX_BACKEND") != "1" {
			t.Skipf("native sandbox backend unavailable: %v", err)
		}
		t.Fatalf("native sandbox backend preflight wrap failed: %v", err)
	}
	cmd := exec.Command(wrapped.Program, wrapped.Args...)
	cmd.Dir = wrapped.Dir
	cmd.Env = wrapped.Env
	output, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	var exitErr *exec.ExitError
	text := strings.TrimSpace(string(output))
	seatbeltUnavailable := goRuntime.GOOS == "darwin" && errors.As(err, &exitErr) && exitErr.ExitCode() == 71 && strings.HasPrefix(text, "sandbox-exec: sandbox_apply:")
	namespaceUnavailable := goRuntime.GOOS == "linux" && strings.HasPrefix(strings.ToLower(text), "bwrap: creating new namespace failed:")
	if seatbeltUnavailable || namespaceUnavailable {
		if os.Getenv("GHG_REQUIRE_SANDBOX_BACKEND") != "1" {
			t.Skipf("native sandbox backend unavailable: %s", text)
		}
	}
	t.Fatalf("native sandbox backend preflight failed: output=%q err=%v", output, err)
}
