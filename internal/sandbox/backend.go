package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// WrapCommand applies the selected OS sandbox to an executable argv. A
// missing or unusable backend is an error for restricted modes; callers must
// not silently run the original command.
func (p *Policy) WrapCommand(spec CommandSpec) (WrappedCommand, error) {
	if p == nil {
		return WrappedCommand{}, fmt.Errorf("%w: no execution policy", ErrSandboxUnavailable)
	}
	if strings.TrimSpace(spec.Program) == "" {
		return WrappedCommand{}, fmt.Errorf("command program is empty")
	}
	if spec.Dir == "" {
		spec.Dir = p.workspace
	}
	dir, err := p.Authorize(spec.Dir, AccessRead, false)
	if err != nil {
		return WrappedCommand{}, fmt.Errorf("command working directory: %w", err)
	}
	spec.Dir = dir
	spec.Args = append([]string(nil), spec.Args...)
	spec.Env = append([]string(nil), spec.Env...)
	if p.mode == ModeDangerFull {
		return WrappedCommand{Program: spec.Program, Args: spec.Args, Dir: spec.Dir, Env: spec.Env, Backend: "unrestricted (explicit danger-full-access)"}, nil
	}

	switch runtime.GOOS {
	case "darwin":
		if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
			return WrappedCommand{}, fmt.Errorf("%w: macOS /usr/bin/sandbox-exec: %v", ErrSandboxUnavailable, err)
		}
		profile := seatbeltProfile(p)
		args := []string{"-p", profile, spec.Program}
		args = append(args, spec.Args...)
		return WrappedCommand{Program: "/usr/bin/sandbox-exec", Args: args, Dir: spec.Dir, Env: spec.Env, Backend: "macos-seatbelt"}, nil
	case "linux":
		bwrap, err := lookupBubblewrap(p.bubblewrapPath)
		if err != nil {
			return WrappedCommand{}, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
		}
		if reason := namespaceBlockReason(); reason != "" {
			return WrappedCommand{}, fmt.Errorf("%w: %s", ErrSandboxUnavailable, reason)
		}
		args := bubblewrapArgs(p, spec)
		args = append(args, "--", spec.Program)
		args = append(args, spec.Args...)
		return WrappedCommand{Program: bwrap, Args: args, Dir: spec.Dir, Env: spec.Env, Backend: "bubblewrap"}, nil
	default:
		return WrappedCommand{}, fmt.Errorf("%w: OS sandboxing is unsupported on %s", ErrSandboxUnavailable, runtime.GOOS)
	}
}

func seatbeltProfile(p *Policy) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")
	b.WriteString("(allow process*)\n")
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow mach-lookup)\n")
	// These are platform/toolchain reads, not user data roots. User-specific
	// caches and the workspace are added below from the explicit policy.
	platformRoots := []string{"/bin", "/sbin", "/usr", "/opt", "/System", "/Library", "/private/etc", "/private/var/db", "/private/var/select", "/dev"}
	readRoots := appendUnique([]string{}, platformRoots...)
	readRoots = appendUnique(readRoots, p.ReadRoots()...)
	readRoots = appendUnique(readRoots, p.WriteRoots()...)
	readRoots = appendUnique(readRoots, p.CacheRoots()...)
	readRoots = appendUnique(readRoots, p.ImmutableRoots()...)
	readRoots = appendUnique(readRoots, p.TempRoots()...)
	readRoots = appendUnique(readRoots, p.ProtectedRoots()...)
	readRoots = appendUnique(readRoots, p.protectedWriteRoots...)
	for _, root := range readRoots {
		seatbeltRule(&b, "allow", "file-read*", root)
	}
	// Seatbelt needs access to the root inode and metadata access to every other
	// directory on the way to an explicitly allowed root. Literal rules keep
	// traversal possible without turning an ancestor such as /Users or /private
	// into a readable subtree.
	for _, root := range seatbeltAncestorRoots(readRoots) {
		if root == string(filepath.Separator) {
			seatbeltLiteralRule(&b, "allow", "file-read*", root)
			continue
		}
		seatbeltLiteralRule(&b, "allow", "file-read-metadata", root)
	}
	writeRoots := appendUnique([]string{}, p.WriteRoots()...)
	writeRoots = appendUnique(writeRoots, p.CacheRoots()...)
	writeRoots = appendUnique(writeRoots, p.TempRoots()...)
	for _, root := range writeRoots {
		seatbeltRule(&b, "allow", "file-write*", root)
	}
	for _, dev := range []string{"/dev/null", "/dev/zero", "/dev/dtracehelper", "/dev/tty", "/dev/ptmx", "/dev/stdin", "/dev/stdout", "/dev/stderr"} {
		seatbeltLiteralRule(&b, "allow", "file-write*", dev)
	}
	// An allow above may include repository metadata because the workspace is
	// a parent root. Explicit deny rules restore the protected boundary.
	for _, root := range p.ProtectedRoots() {
		if !anyWithin(root, p.protectedWriteRoots) {
			seatbeltRule(&b, "deny", "file-write*", root)
		}
	}
	for _, root := range p.protectedWriteRoots {
		seatbeltRule(&b, "allow", "file-write*", root)
	}
	for _, root := range p.ImmutableRoots() {
		seatbeltRule(&b, "deny", "file-write*", root)
		seatbeltRule(&b, "allow", "process-exec", root)
	}
	if p.NetworkAllowed() {
		b.WriteString("(allow network*)\n")
	} else {
		b.WriteString("(allow network* (local unix-socket))\n")
		b.WriteString("(allow network* (remote unix-socket))\n")
	}
	return b.String()
}

func seatbeltRule(b *strings.Builder, verb, operation, root string) {
	if root == "" || !filepath.IsAbs(root) {
		return
	}
	fmt.Fprintf(b, "(%s %s (subpath %s))\n", verb, operation, strconv.Quote(filepath.Clean(root)))
}

func seatbeltLiteralRule(b *strings.Builder, verb, operation, root string) {
	if root == "" || !filepath.IsAbs(root) {
		return
	}
	fmt.Fprintf(b, "(%s %s (literal %s))\n", verb, operation, strconv.Quote(filepath.Clean(root)))
}

func seatbeltAncestorRoots(roots []string) []string {
	seen := make(map[string]struct{})
	for _, root := range roots {
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		current := filepath.Clean(root)
		for {
			if current == string(filepath.Separator) {
				seen[current] = struct{}{}
				break
			}
			current = filepath.Dir(current)
			seen[current] = struct{}{}
		}
	}

	ancestors := make([]string, 0, len(seen))
	for root := range seen {
		ancestors = append(ancestors, root)
	}
	sort.Strings(ancestors)
	return ancestors
}

func bubblewrapArgs(p *Policy, spec CommandSpec) []string {
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-uts",
		"--unshare-ipc",
		"--cap-drop", "ALL",
		"--no-new-privs",
		"--tmpfs", "/",
		"--proc", "/proc",
		"--dev", "/dev",
	}
	if !p.NetworkAllowed() {
		args = append(args, "--unshare-net")
	}
	// The root starts empty: --ro-bind / / would expose every host-readable
	// credential and repository. Mount only the runtime paths and policy roots.
	mounts := newBubblewrapMounts(&args)
	mounts.mkdir("/tmp")
	args = append(args, "--tmpfs", "/tmp")
	for _, root := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/opt"} {
		mounts.bind("--ro-bind", root)
	}
	for _, root := range p.ReadRoots() {
		mounts.bind("--ro-bind", root)
	}
	for _, root := range p.WriteRoots() {
		mounts.bind("--bind", root)
	}
	for _, root := range p.CacheRoots() {
		mounts.bind("--bind", root)
	}
	for _, root := range p.TempRoots() {
		mounts.bind("--bind", root)
	}
	for _, root := range p.ProtectedRoots() {
		if !anyWithin(root, p.protectedWriteRoots) {
			mounts.bind("--ro-bind", root)
		}
	}
	for _, root := range orderedMountRoots(p.ImmutableRoots()) {
		mounts.bind("--ro-bind", root)
	}
	if spec.Dir != "" {
		args = append(args, "--chdir", spec.Dir)
	}
	return args
}

func orderedMountRoots(roots []string) []string {
	ordered := append([]string(nil), roots...)
	sort.SliceStable(ordered, func(i, j int) bool {
		depth := func(root string) int {
			return len(splitMountPath(root))
		}
		leftDepth, rightDepth := depth(ordered[i]), depth(ordered[j])
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return ordered[i] < ordered[j]
	})
	return ordered
}

func splitMountPath(root string) []string {
	root = filepath.Clean(root)
	if root == string(filepath.Separator) {
		return nil
	}
	return strings.Split(strings.TrimPrefix(root, string(filepath.Separator)), string(filepath.Separator))
}

type bubblewrapMounts struct {
	args *([]string)
	dirs map[string]bool
}

func newBubblewrapMounts(args *[]string) *bubblewrapMounts {
	return &bubblewrapMounts{args: args, dirs: map[string]bool{"/": true}}
}

func (m *bubblewrapMounts) bind(kind, root string) {
	if root == "" || !filepath.IsAbs(root) || !pathExists(root) {
		return
	}
	root = filepath.Clean(root)
	m.mkdir(root)
	*m.args = append(*m.args, kind, root, root)
}

func (m *bubblewrapMounts) mkdir(root string) {
	if root == "" || !filepath.IsAbs(root) {
		return
	}
	root = filepath.Clean(root)
	mkdir := []string{}
	for current := root; !m.dirs[current]; current = filepath.Dir(current) {
		mkdir = append(mkdir, current)
	}
	for i := len(mkdir) - 1; i >= 0; i-- {
		*m.args = append(*m.args, "--dir", mkdir[i])
		m.dirs[mkdir[i]] = true
	}
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func lookupBubblewrap(explicit string) (string, error) {
	candidates := []string{explicit}
	if explicit == "" {
		candidates = []string{"/usr/bin/bwrap", "/bin/bwrap", "/usr/local/bin/bwrap"}
	}
	var lastErr error
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		resolved, err := trustedExecutable(candidate)
		if err == nil {
			return resolved, nil
		}
		lastErr = err
	}
	if explicit != "" {
		return "", fmt.Errorf("configured bubblewrap is not trusted: %w", lastErr)
	}
	return "", fmt.Errorf("trusted bubblewrap was not found in the system locations: %w", lastErr)
}

func trustedExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path is not absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", resolved)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not executable", resolved)
	}
	if !rootOwned(info) || info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%s is not root-owned and non-writable", resolved)
	}
	for current := filepath.Dir(resolved); ; current = filepath.Dir(current) {
		parentInfo, err := os.Stat(current)
		if err != nil {
			return "", err
		}
		if !parentInfo.IsDir() || !rootOwned(parentInfo) || parentInfo.Mode().Perm()&0o022 != 0 {
			return "", fmt.Errorf("backend parent %s is not root-owned and non-writable", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return resolved, nil
}

func namespaceBlockReason() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	if data, err := os.ReadFile("/proc/version"); err == nil {
		version := strings.ToLower(string(data))
		if strings.Contains(version, "microsoft") && !strings.Contains(version, "microsoft-standard") && !strings.Contains(version, "wsl2") {
			return "WSL1 does not provide the namespaces required by bubblewrap"
		}
	}
	for _, name := range []string{"/proc/sys/user/max_user_namespaces", "/proc/sys/kernel/unprivileged_userns_clone"} {
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) == "0" {
			return fmt.Sprintf("user namespaces are blocked by %s", name)
		}
	}
	return ""
}
