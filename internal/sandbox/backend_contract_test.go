package sandbox

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWrappedChildEnforcesFilesystemBoundary(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("OS backend contract is unsupported on %s", runtime.GOOS)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitRoot := filepath.Join(workspace, ".git")
	if err := os.Mkdir(gitRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(workspace, "source.txt")
	workspaceTarget := filepath.Join(workspace, "result.txt")
	if err := os.WriteFile(source, []byte("workspace-data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	external, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(external, "secret.txt")
	externalTarget := filepath.Join(external, "result.txt")
	if err := os.WriteFile(secret, []byte("external-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy(PolicyConfig{Workspace: workspace, Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeBackend(t, policy)
	env := []string{"PATH=/usr/bin:/bin"}
	base := CommandSpec{Program: "/bin/sh", Dir: workspace, Env: env}
	run := func(args ...string) ([]byte, error) {
		spec := base
		spec.Args = args
		wrapped, wrapErr := policy.WrapCommand(spec)
		if wrapErr != nil {
			t.Fatal(wrapErr)
		}
		cmd := exec.Command(wrapped.Program, wrapped.Args...)
		cmd.Dir = wrapped.Dir
		cmd.Env = wrapped.Env
		return cmd.CombinedOutput()
	}

	if output, err := run("-c", `/bin/cat "$1"`, "sh", source); err != nil || string(output) != "workspace-data\n" {
		t.Fatalf("workspace read output=%q err=%v", output, err)
	}
	if output, err := run("-c", `/usr/bin/printf child > "$1"`, "sh", workspaceTarget); err != nil {
		t.Fatalf("workspace write output=%q err=%v", output, err)
	}
	if got, err := os.ReadFile(workspaceTarget); err != nil || string(got) != "child" {
		t.Fatalf("workspace result=%q err=%v", got, err)
	}
	if output, err := run("-c", `/bin/sh -c '/bin/cat "$1" > "$2"' child "$1" "$2"`, "sh", source, workspaceTarget); err != nil {
		t.Fatalf("descendant write output=%q err=%v", output, err)
	}
	if output, err := run("-c", `/bin/cat "$1"`, "sh", secret); err == nil || strings.Contains(string(output), "external-secret") {
		t.Fatalf("external read output=%q err=%v", output, err)
	}
	if output, err := run("-c", `/usr/bin/printf child > "$1"`, "sh", externalTarget); err == nil {
		t.Fatalf("external write unexpectedly succeeded: %q", output)
	}
	if output, err := run("-c", `/usr/bin/printf child > "$1"`, "sh", filepath.Join(gitRoot, "index")); err == nil {
		t.Fatalf("protected metadata write unexpectedly succeeded: %q", output)
	}
}

func TestWrappedChildUsesPrivateTempAndCacheRoots(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("OS backend contract is unsupported on %s", runtime.GOOS)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tempRoot := filepath.Join(workspace, ".tmp")
	cacheRoot := filepath.Join(workspace, ".cache")
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy(PolicyConfig{
		Workspace:  workspace,
		Mode:       ModeWorkspaceWrite,
		CacheRoots: []string{cacheRoot},
		TempRoots:  []string{tempRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeBackend(t, policy)
	spec := CommandSpec{
		Program: "/bin/sh",
		Args: []string{
			"-c",
			`test "$TMPDIR" = "$1" && test "$TMP" = "$1" && test "$TEMP" = "$1" && test "$GOCACHE" = "$2" && test -d "$TMPDIR" && test -d "$GOCACHE" && /usr/bin/printf ok > "$TMPDIR/probe"`,
			"sh", tempRoot, cacheRoot,
		},
		Dir: workspace,
		Env: []string{
			"PATH=/usr/bin:/bin",
			"TMPDIR=" + tempRoot,
			"TMP=" + tempRoot,
			"TEMP=" + tempRoot,
			"GOCACHE=" + cacheRoot,
		},
	}
	wrapped, err := policy.WrapCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapped.Program, wrapped.Args...)
	cmd.Dir = wrapped.Dir
	cmd.Env = wrapped.Env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("private temp/cache probe output=%q err=%v", output, err)
	}
	if got, err := os.ReadFile(filepath.Join(tempRoot, "probe")); err != nil || string(got) != "ok" {
		t.Fatalf("private temp probe=%q err=%v", got, err)
	}
}

func TestReadOnlyChildKeepsCacheAndTempWritable(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("OS backend contract is unsupported on %s", runtime.GOOS)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Join(workspace, "cache")
	tempRoot := filepath.Join(workspace, "temp")
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy(PolicyConfig{
		Workspace: workspace, Mode: ModeReadOnly,
		CacheRoots: []string{cacheRoot}, TempRoots: []string{tempRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeBackend(t, policy)
	workspaceTarget := filepath.Join(workspace, "blocked")
	cacheTarget := filepath.Join(cacheRoot, "cache-value")
	tempTarget := filepath.Join(tempRoot, "temp-value")
	script := `set -eu
/usr/bin/printf cache > "$1"
/usr/bin/printf temp > "$2"
if /usr/bin/printf blocked > "$3" 2>/dev/null; then exit 10; fi
`
	wrapped, err := policy.WrapCommand(CommandSpec{
		Program: "/bin/sh",
		Args:    []string{"-c", script, "sh", cacheTarget, tempTarget, workspaceTarget},
		Dir:     workspace,
		Env:     []string{"PATH=/usr/bin:/bin", "TMPDIR=" + tempRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapped.Program, wrapped.Args...)
	cmd.Dir = wrapped.Dir
	cmd.Env = wrapped.Env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read-only cache/temp probe output=%q err=%v", output, err)
	}
	for path, want := range map[string]string{cacheTarget: "cache", tempTarget: "temp"} {
		if got, readErr := os.ReadFile(path); readErr != nil || string(got) != want {
			t.Fatalf("read-only write %q = %q, %v; want %q", path, got, readErr, want)
		}
	}
	if _, err := os.Stat(workspaceTarget); !os.IsNotExist(err) {
		t.Fatalf("read-only workspace target exists or returned unexpected error: %v", err)
	}
}

func TestWrappedChildUsesNarrowCachesAndImmutableToolchains(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("OS backend contract is unsupported on %s", runtime.GOOS)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "one", "two", "three", "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	cargoHome := filepath.Join(home, ".cargo")
	rustupHome := filepath.Join(home, ".rustup")
	bunInstall := filepath.Join(home, ".bun")
	gopath := filepath.Join(home, "go")
	cacheRoots := []string{
		filepath.Join(cargoHome, "registry"), filepath.Join(cargoHome, "git"),
		filepath.Join(rustupHome, "downloads"), filepath.Join(bunInstall, "install", "cache"),
		filepath.Join(gopath, "pkg", "sumdb"), filepath.Join(home, "go-build"), filepath.Join(home, ".npm"),
	}
	immutableRoots := []string{
		filepath.Join(cargoHome, "bin"), filepath.Join(rustupHome, "toolchains"),
		filepath.Join(bunInstall, "bin"), filepath.Join(gopath, "bin"), runtime.GOROOT(),
	}
	for _, dir := range append(append([]string{}, cacheRoots...), immutableRoots[:len(immutableRoots)-1]...) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range immutableRoots {
		if root == runtime.GOROOT() {
			continue
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fakeTools := []string{
		filepath.Join(cargoHome, "bin", "fake-cargo"),
		filepath.Join(rustupHome, "toolchains", "stable-tool"),
		filepath.Join(bunInstall, "bin", "fake-bun"),
		filepath.Join(gopath, "bin", "fake-go-tool"),
	}
	for _, tool := range fakeTools {
		if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf 'immutable-tool\n'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	credentials := filepath.Join(cargoHome, "credentials.toml")
	cargoConfig := filepath.Join(cargoHome, "config.toml")
	rustupSettings := filepath.Join(rustupHome, "settings.toml")
	adjacent := filepath.Join(home, "cache-adjacent-secret")
	for _, path := range []string{credentials, cargoConfig, rustupSettings, adjacent} {
		if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	policy, err := NewPolicy(PolicyConfig{
		Workspace:      workspace,
		Mode:           ModeWorkspaceWrite,
		CacheRoots:     cacheRoots,
		ImmutableRoots: immutableRoots,
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeBackend(t, policy)
	baseEnv := []string{
		"PATH=" + filepath.Join(cargoHome, "bin") + ":" + filepath.Join(rustupHome, "toolchains") + ":" + filepath.Join(bunInstall, "bin") + ":" + filepath.Join(gopath, "bin") + ":/usr/bin:/bin",
		"HOME=" + home,
		"CARGO_HOME=" + cargoHome,
		"RUSTUP_HOME=" + rustupHome,
		"BUN_INSTALL=" + bunInstall,
		"GOPATH=" + gopath,
		"NPM_CONFIG_CACHE=" + filepath.Join(home, ".npm"),
	}
	run := func(spec CommandSpec) ([]byte, error) {
		t.Helper()
		wrapped, wrapErr := policy.WrapCommand(spec)
		if wrapErr != nil {
			t.Fatal(wrapErr)
		}
		cmd := exec.Command(wrapped.Program, wrapped.Args...)
		cmd.Dir = wrapped.Dir
		cmd.Env = wrapped.Env
		output, runErr := cmd.CombinedOutput()
		return output, runErr
	}

	for _, tool := range fakeTools {
		output, runErr := run(CommandSpec{Program: tool, Dir: workspace, Env: baseEnv})
		if runErr != nil || string(output) != "immutable-tool\n" {
			t.Fatalf("immutable tool %q output=%q err=%v", tool, output, runErr)
		}
	}
	for _, tool := range fakeTools {
		original, err := os.ReadFile(tool)
		if err != nil {
			t.Fatal(err)
		}
		_, runErr := run(CommandSpec{
			Program: "/bin/sh",
			Args:    []string{"-c", `/usr/bin/printf changed > "$1"`, "sh", tool},
			Dir:     workspace,
			Env:     baseEnv,
		})
		if runErr == nil {
			t.Fatalf("immutable tool overwrite unexpectedly succeeded: %q", tool)
		}
		updated, readErr := os.ReadFile(tool)
		if readErr != nil || string(updated) != string(original) {
			t.Fatalf("immutable tool %q changed: %q err=%v", tool, updated, readErr)
		}
	}
	for _, path := range []string{credentials, cargoConfig, rustupSettings, adjacent} {
		_, runErr := run(CommandSpec{
			Program: "/bin/sh",
			Args:    []string{"-c", `/usr/bin/printf changed > "$1"`, "sh", path},
			Dir:     workspace,
			Env:     baseEnv,
		})
		if runErr == nil {
			t.Fatalf("protected sibling write unexpectedly succeeded: %q", path)
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil || string(contents) != "unchanged" {
			t.Fatalf("protected sibling %q changed: %q err=%v", path, contents, readErr)
		}
	}
	for i, cacheRoot := range cacheRoots {
		target := filepath.Join(cacheRoot, fmt.Sprintf("write-%d", i))
		_, runErr := run(CommandSpec{
			Program: "/bin/sh",
			Args:    []string{"-c", `/usr/bin/printf cached > "$1"`, "sh", target},
			Dir:     workspace,
			Env:     baseEnv,
		})
		if runErr != nil {
			t.Fatalf("cache write %q failed: %v", target, runErr)
		}
	}
	if npmBinary, lookErr := exec.LookPath("npm"); lookErr == nil {
		npmBinary, evalErr := filepath.EvalSymlinks(npmBinary)
		if evalErr != nil {
			t.Fatal(evalErr)
		}
		npmEnv := append([]string(nil), baseEnv...)
		nodeBinary, nodeErr := exec.LookPath("node")
		if nodeErr != nil {
			t.Fatal(nodeErr)
		}
		npmEnv[0] = "PATH=" + filepath.Dir(npmBinary) + ":" + filepath.Dir(nodeBinary) + ":" + strings.TrimPrefix(baseEnv[0], "PATH=")
		output, runErr := run(CommandSpec{
			Program: npmBinary,
			Args:    []string{"cache", "verify", "--cache", filepath.Join(home, ".npm")},
			Dir:     workspace,
			Env:     npmEnv,
		})
		if runErr != nil {
			t.Fatalf("offline npm cache verification output=%q err=%v", output, runErr)
		}
	}
}

func TestWrappedChildRunsCachedGoTest(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("OS backend contract is unsupported on %s", runtime.GOOS)
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	goBinary, err = filepath.EvalSymlinks(goBinary)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module sandbox-contract\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "contract_test.go"), []byte("package contract\n\nimport \"testing\"\n\nfunc TestContract(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tempRoot := filepath.Join(workspace, ".tmp")
	cacheRoot := filepath.Join(workspace, ".cache")
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy(PolicyConfig{
		Workspace:  workspace,
		Mode:       ModeReadOnly,
		ReadRoots:  []string{runtime.GOROOT(), filepath.Dir(goBinary)},
		CacheRoots: []string{cacheRoot},
		TempRoots:  []string{tempRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeBackend(t, policy)
	spec := CommandSpec{
		Program: goBinary,
		Args:    []string{"test", "./..."},
		Dir:     workspace,
		Env: []string{
			"PATH=" + filepath.Dir(goBinary) + ":/usr/bin:/bin",
			"HOME=" + workspace,
			"GOCACHE=" + cacheRoot,
			"GOMODCACHE=" + filepath.Join(cacheRoot, "mod"),
			"GOPATH=" + filepath.Join(cacheRoot, "go"),
			"GOTOOLCHAIN=local",
			"TMPDIR=" + tempRoot,
			"TMP=" + tempRoot,
			"TEMP=" + tempRoot,
		},
	}
	for range 2 {
		wrapped, wrapErr := policy.WrapCommand(spec)
		if wrapErr != nil {
			t.Fatal(wrapErr)
		}
		cmd := exec.Command(wrapped.Program, wrapped.Args...)
		cmd.Dir = wrapped.Dir
		cmd.Env = wrapped.Env
		output, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Fatalf("cached go test output=%q err=%v", output, runErr)
		}
	}
}

func requireNativeBackend(t *testing.T, policy *Policy) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("OS backend contract is unsupported on %s", runtime.GOOS)
	}
	wrapped, err := policy.WrapCommand(CommandSpec{
		Program: "/usr/bin/true",
		Dir:     policy.Workspace(),
		Env:     []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			handleUnavailableBackend(t, err)
		}
		t.Fatalf("native backend preflight wrap failed: %v", err)
	}
	cmd := exec.Command(wrapped.Program, wrapped.Args...)
	cmd.Dir = wrapped.Dir
	cmd.Env = wrapped.Env
	output, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	if backendStartupFailure(output, err) {
		handleUnavailableBackend(t, fmt.Errorf("%s", strings.TrimSpace(string(output))))
	}
	t.Fatalf("native backend preflight failed: output=%q err=%v", output, err)
}

func handleUnavailableBackend(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("GHG_REQUIRE_SANDBOX_BACKEND") == "1" {
		t.Fatalf("required native sandbox backend unavailable: %v", err)
	}
	t.Skipf("native sandbox backend unavailable: %v", err)
}

func backendStartupFailure(output []byte, err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	text := strings.TrimSpace(string(output))
	switch runtime.GOOS {
	case "darwin":
		return exitErr.ExitCode() == 71 && strings.HasPrefix(text, "sandbox-exec: sandbox_apply:")
	case "linux":
		return strings.HasPrefix(strings.ToLower(text), "bwrap: creating new namespace failed:")
	default:
		return false
	}
}

func TestWrappedChildOutputCannotSpoofBackendSkip(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("OS backend contract is unsupported on %s", runtime.GOOS)
	}
	workspace := t.TempDir()
	policy, err := NewPolicy(PolicyConfig{Workspace: workspace, Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeBackend(t, policy)
	phrase := "sandbox-exec: sandbox_apply: spoofed"
	if runtime.GOOS == "linux" {
		phrase = "creating new namespace failed"
	}
	wrapped, err := policy.WrapCommand(CommandSpec{
		Program: "/bin/sh",
		Args:    []string{"-c", `printf '%s\n' "$1"; exit 23`, "sh", phrase},
		Dir:     workspace,
		Env:     []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapped.Program, wrapped.Args...)
	cmd.Dir = wrapped.Dir
	cmd.Env = wrapped.Env
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("noisy child unexpectedly succeeded: %q", output)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 || !strings.Contains(string(output), phrase) {
		t.Fatalf("noisy child was not observed as a normal failed child: output=%q err=%v", output, err)
	}
}

func TestWrappedChildNetworkBoundary(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("OS backend contract is unsupported on %s", runtime.GOOS)
	}
	if _, err := os.Stat("/usr/bin/curl"); err != nil {
		t.Skipf("curl unavailable: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	defer listener.Close()
	connections := make(chan struct{}, 4)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connections <- struct{}{}
			_, _ = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			_ = conn.Close()
		}
	}()

	workspace := t.TempDir()
	policy, err := NewPolicy(PolicyConfig{Workspace: workspace, Mode: ModeWorkspaceWrite, Network: NetworkDeny})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeBackend(t, policy)
	url := "http://" + listener.Addr().String()
	spec := CommandSpec{Program: "/usr/bin/curl", Args: []string{"-q", "-sS", "--noproxy", "*", "--max-time", "2", url}, Dir: workspace, Env: []string{"PATH=/usr/bin:/bin"}}
	wrapped, err := policy.WrapCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapped.Program, wrapped.Args...)
	cmd.Dir = wrapped.Dir
	cmd.Env = wrapped.Env
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("network-denied curl unexpectedly succeeded: %q", output)
	}
	select {
	case <-connections:
		t.Fatal("network-denied child reached loopback listener")
	case <-time.After(100 * time.Millisecond):
	}

	granted, err := policy.Grant(nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err = granted.WrapCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(wrapped.Program, wrapped.Args...)
	cmd.Dir = wrapped.Dir
	cmd.Env = wrapped.Env
	output, err = cmd.CombinedOutput()
	if err != nil || string(output) != "ok" {
		t.Fatalf("network-enabled curl output=%q err=%v", output, err)
	}
	select {
	case <-connections:
	case <-time.After(time.Second):
		t.Fatal("network-enabled child did not reach loopback listener")
	}
}
