// Package sandbox contains the execution policy shared by ghg's tools.
//
// The policy is deliberately independent from the TUI and from the operating
// system backend. Native tools use it for canonical path authorization even
// when no OS sandbox is available; subprocess tools use the same policy to
// build a restricted child process.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Mode controls the filesystem authority granted to a command.
type Mode string

const (
	ModeReadOnly       Mode = "read-only"
	ModeWorkspaceWrite Mode = "workspace-write"
	ModeDangerFull     Mode = "danger-full-access"
)

// NetworkMode controls whether a sandboxed subprocess can use the network.
type NetworkMode string

const (
	NetworkDeny NetworkMode = "deny"
	NetworkHost NetworkMode = "host"
)

// Access is the native-tool capability being requested.
type Access string

const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
)

var (
	ErrOutsideRoot        = errors.New("path is outside the execution roots")
	ErrWriteDenied        = errors.New("write access is not permitted by the execution policy")
	ErrSandboxUnavailable = errors.New("no supported OS sandbox backend is available")
	ErrNetworkDenied      = errors.New("network access is not permitted by the execution policy")
)

// Denial is returned for a policy decision that does not authorize an
// operation. It retains the capability and canonical path for a typed
// approval request without leaking command output or environment values.
type Denial struct {
	Access Access
	Path   string
	Reason string
}

func (d *Denial) Error() string {
	if d == nil {
		return "execution policy denied operation"
	}
	if d.Path == "" {
		return "execution policy denied " + string(d.Access) + " access: " + d.Reason
	}
	return fmt.Sprintf("execution policy denied %s access to %s: %s", d.Access, d.Path, d.Reason)
}

func (d *Denial) Unwrap() error {
	if d == nil {
		return nil
	}
	switch d.Access {
	case AccessWrite:
		if d.Reason == ErrWriteDenied.Error() {
			return ErrWriteDenied
		}
		return errors.Join(ErrWriteDenied, ErrOutsideRoot)
	default:
		return ErrOutsideRoot
	}
}

// PolicyConfig constructs a policy. Workspace must already exist and be a
// directory. Additional roots may be absent; their nearest existing parent is
// resolved so a symlinked parent cannot be used to escape the grant later.
type PolicyConfig struct {
	Workspace      string
	Mode           Mode
	Network        NetworkMode
	BubblewrapPath string
	ReadRoots      []string
	WriteRoots     []string
	CacheRoots     []string
	ImmutableRoots []string
	TempRoots      []string
	ProtectedRoots []string
}

type compiledPolicy struct {
	seatbeltProfile string
	bubblewrapBase  []string
}

// Policy is immutable after construction. Grant returns a new policy instead
// of mutating one that may be shared by concurrent child agents.
type Policy struct {
	workspace      string
	mode           Mode
	network        NetworkMode
	bubblewrapPath string
	readRoots      []string
	writeRoots     []string
	cacheRoots     []string
	immutableRoots []string
	tempRoots      []string
	protectedRoots []string
	// protectedWriteRoots is populated only on a call-scoped policy after an
	// explicit human approval. The session policy never grants this access.
	protectedWriteRoots []string
	compiled            *compiledPolicy
}

// ParseMode validates a user/configuration value. Empty mode selects the
// default workspace-write policy.
func ParseMode(value string) (Mode, error) {
	switch Mode(strings.TrimSpace(strings.ToLower(value))) {
	case "":
		return ModeWorkspaceWrite, nil
	case ModeReadOnly:
		return ModeReadOnly, nil
	case ModeWorkspaceWrite:
		return ModeWorkspaceWrite, nil
	case ModeDangerFull:
		return ModeDangerFull, nil
	default:
		return "", fmt.Errorf("unknown sandbox mode %q (want read-only, workspace-write, or danger-full-access)", value)
	}
}

// ParseNetworkMode validates a user/configuration value. Empty mode denies
// network access.
func ParseNetworkMode(value string) (NetworkMode, error) {
	switch NetworkMode(strings.TrimSpace(strings.ToLower(value))) {
	case "", NetworkDeny:
		return NetworkDeny, nil
	case NetworkHost:
		return NetworkHost, nil
	default:
		return "", fmt.Errorf("unknown sandbox network mode %q (want deny or host)", value)
	}
}

// NewPolicy canonicalizes all configured roots once. This is the only place
// the workspace is selected; child agents receive the resulting policy.
func NewPolicy(cfg PolicyConfig) (*Policy, error) {
	mode, err := ParseMode(string(cfg.Mode))
	if err != nil {
		return nil, err
	}
	network, err := ParseNetworkMode(string(cfg.Network))
	if err != nil {
		return nil, err
	}
	workspace, err := canonicalExistingDir(cfg.Workspace)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}

	p := &Policy{workspace: workspace, mode: mode, network: network, bubblewrapPath: cfg.BubblewrapPath}
	// The workspace is always readable. Native writes are enabled there only
	// for workspace-write and danger-full-access.
	p.readRoots = appendUnique(p.readRoots, workspace)
	if mode != ModeReadOnly {
		p.writeRoots = appendUnique(p.writeRoots, workspace)
	}
	for _, root := range cfg.ReadRoots {
		canonical, err := canonicalRoot(root)
		if err != nil {
			return nil, fmt.Errorf("read root %q: %w", root, err)
		}
		p.readRoots = appendUnique(p.readRoots, canonical)
	}
	for _, root := range cfg.WriteRoots {
		canonical, err := canonicalRoot(root)
		if err != nil {
			return nil, fmt.Errorf("write root %q: %w", root, err)
		}
		p.readRoots = appendUnique(p.readRoots, canonical)
		if mode != ModeReadOnly {
			p.writeRoots = appendUnique(p.writeRoots, canonical)
		}
	}
	for _, root := range cfg.CacheRoots {
		canonical, err := canonicalRoot(root)
		if err != nil {
			return nil, fmt.Errorf("cache root %q: %w", root, err)
		}
		p.cacheRoots = appendUnique(p.cacheRoots, canonical)
		p.readRoots = appendUnique(p.readRoots, canonical)
	}
	for _, root := range cfg.ImmutableRoots {
		canonical, err := canonicalRoot(root)
		if err != nil {
			return nil, fmt.Errorf("immutable root %q: %w", root, err)
		}
		p.immutableRoots = appendUnique(p.immutableRoots, canonical)
		p.readRoots = appendUnique(p.readRoots, canonical)
	}
	for _, root := range cfg.TempRoots {
		canonical, err := canonicalRoot(root)
		if err != nil {
			return nil, fmt.Errorf("temp root %q: %w", root, err)
		}
		p.tempRoots = appendUnique(p.tempRoots, canonical)
		p.readRoots = appendUnique(p.readRoots, canonical)
	}
	for _, root := range cfg.ProtectedRoots {
		canonical, err := canonicalRoot(root)
		if err != nil {
			return nil, fmt.Errorf("protected root %q: %w", root, err)
		}
		p.protectedRoots = appendUnique(p.protectedRoots, canonical)
	}
	// Repository metadata is readable for ordinary git tooling but native
	// write/edit operations must not mutate it through a path-shaped request.
	for _, root := range []string{filepath.Join(workspace, ".git"), filepath.Join(workspace, ".ghg")} {
		if canonical, rootErr := canonicalRoot(root); rootErr == nil {
			p.protectedRoots = appendUnique(p.protectedRoots, canonical)
		}
	}
	p.compiled = compilePolicy(p)
	return p, nil
}

// Workspace returns the canonical workspace root.
func (p *Policy) Workspace() string {
	if p == nil {
		return ""
	}
	return p.workspace
}

// Mode returns the configured sandbox mode.
func (p *Policy) Mode() Mode {
	if p == nil {
		return ""
	}
	return p.mode
}

// Network returns the configured subprocess network mode.
func (p *Policy) Network() NetworkMode {
	if p == nil {
		return ""
	}
	return p.network
}

// ReadRoots returns a defensive copy of the roots available to reads and
// sandboxed subprocesses.
func (p *Policy) ReadRoots() []string { return slices.Clone(p.readRoots) }

// WriteRoots returns a defensive copy of the roots available to writes.
func (p *Policy) WriteRoots() []string { return slices.Clone(p.writeRoots) }

// CacheRoots returns canonical cache roots granted to subprocesses.
func (p *Policy) CacheRoots() []string { return slices.Clone(p.cacheRoots) }

// ImmutableRoots returns canonical executable and toolchain roots that remain
// readable but are never writable through ordinary policy grants.
func (p *Policy) ImmutableRoots() []string { return slices.Clone(p.immutableRoots) }

// TempRoots returns canonical private temporary roots granted to subprocesses.
func (p *Policy) TempRoots() []string { return slices.Clone(p.tempRoots) }

// ProtectedRoots returns canonical metadata/state roots that native writes
// may not modify.
func (p *Policy) ProtectedRoots() []string { return slices.Clone(p.protectedRoots) }

// NetworkAllowed reports whether a child may use normal host networking.
func (p *Policy) NetworkAllowed() bool { return p != nil && p.network == NetworkHost }

// Authorize canonicalizes name and checks the requested native-tool access.
// allowMissing is intended for write targets; the existing parent is resolved
// through symlinks before the final nonexistent suffix is appended.
func (p *Policy) Authorize(name string, access Access, allowMissing bool) (string, error) {
	if p == nil {
		return canonicalPath(name, allowMissing)
	}
	canonical, err := canonicalPath(name, allowMissing)
	if err != nil {
		return "", err
	}
	if p.mode == ModeDangerFull {
		return canonical, nil
	}
	if access == AccessWrite {
		if anyWithin(canonical, p.immutableRoots) {
			return "", &Denial{Access: access, Path: canonical, Reason: "immutable toolchain/executable root"}
		}
		for _, root := range p.protectedRoots {
			if (canonical == root || within(canonical, root)) && !anyWithin(canonical, p.protectedWriteRoots) {
				return "", &Denial{Access: access, Path: canonical, Reason: "protected metadata/state"}
			}
		}
		if p.mode == ModeReadOnly {
			if anyWithin(canonical, p.cacheRoots) || anyWithin(canonical, p.tempRoots) {
				return canonical, nil
			}
			return "", &Denial{Access: access, Path: canonical, Reason: ErrWriteDenied.Error()}
		}
		if !anyWithin(canonical, p.writeRoots) && !anyWithin(canonical, p.cacheRoots) && !anyWithin(canonical, p.tempRoots) {
			return "", &Denial{Access: access, Path: canonical, Reason: ErrOutsideRoot.Error()}
		}
		return canonical, nil
	}
	if !anyWithin(canonical, p.readRoots) {
		return "", &Denial{Access: access, Path: canonical, Reason: ErrOutsideRoot.Error()}
	}
	return canonical, nil
}

// CanonicalPath resolves a path using the same symlink/nonexistent-parent
// rules as Authorize without checking a capability. It is used to describe a
// typed external-root request before asking for a one-shot grant.
func CanonicalPath(name string, allowMissing bool) (string, error) {
	return canonicalPath(name, allowMissing)
}

// Grant returns a policy widened only by the supplied canonical roots and/or
// network bit. Existing restrictions, including protected metadata, remain.
// A caller should keep this policy scoped to the single approved operation.
func (p *Policy) Grant(readRoots, writeRoots []string, network bool) (*Policy, error) {
	if p == nil {
		return nil, errors.New("cannot grant capabilities to a nil policy")
	}
	if p.mode == ModeReadOnly && len(writeRoots) > 0 {
		return nil, errors.New("read-only policy cannot grant write roots")
	}
	clone := *p
	clone.readRoots = slices.Clone(p.readRoots)
	clone.writeRoots = slices.Clone(p.writeRoots)
	clone.cacheRoots = slices.Clone(p.cacheRoots)
	clone.immutableRoots = slices.Clone(p.immutableRoots)
	clone.tempRoots = slices.Clone(p.tempRoots)
	clone.protectedRoots = slices.Clone(p.protectedRoots)
	clone.protectedWriteRoots = slices.Clone(p.protectedWriteRoots)
	for _, root := range readRoots {
		canonical, err := canonicalRoot(root)
		if err != nil {
			return nil, err
		}
		clone.readRoots = appendUnique(clone.readRoots, canonical)
	}
	for _, root := range writeRoots {
		canonical, err := canonicalRoot(root)
		if err != nil {
			return nil, err
		}
		clone.readRoots = appendUnique(clone.readRoots, canonical)
		clone.writeRoots = appendUnique(clone.writeRoots, canonical)
	}
	if network {
		clone.network = NetworkHost
	}
	clone.compiled = compilePolicy(&clone)
	return &clone, nil
}

// GrantProtected returns a call-scoped policy that permits writes to the
// exact protected roots listed. Only roots already protected by this policy
// may be granted; callers cannot use this method to turn an arbitrary path
// into a privileged root. The session policy remains unchanged.
func (p *Policy) GrantProtected(writeRoots []string) (*Policy, error) {
	if p == nil {
		return nil, errors.New("cannot grant protected capabilities to a nil policy")
	}
	if p.mode == ModeReadOnly && len(writeRoots) > 0 {
		return nil, errors.New("read-only policy cannot grant protected writes")
	}
	clone := *p
	clone.readRoots = slices.Clone(p.readRoots)
	clone.writeRoots = slices.Clone(p.writeRoots)
	clone.cacheRoots = slices.Clone(p.cacheRoots)
	clone.immutableRoots = slices.Clone(p.immutableRoots)
	clone.tempRoots = slices.Clone(p.tempRoots)
	clone.protectedRoots = slices.Clone(p.protectedRoots)
	clone.protectedWriteRoots = slices.Clone(p.protectedWriteRoots)
	for _, root := range writeRoots {
		canonical, err := canonicalRoot(root)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(p.protectedRoots, canonical) {
			return nil, fmt.Errorf("%s is not a protected policy root", canonical)
		}
		clone.protectedWriteRoots = appendUnique(clone.protectedWriteRoots, canonical)
	}
	clone.compiled = compilePolicy(&clone)
	return &clone, nil
}

// ProtectedRootFor resolves name and reports the protected root containing
// it. The root, rather than the requested file, is returned because both
// Seatbelt and bubblewrap need a directory capability for a process that may
// update metadata atomically.
func (p *Policy) ProtectedRootFor(name string, allowMissing bool) (string, bool, error) {
	if p == nil {
		return "", false, errors.New("cannot inspect protected roots on a nil policy")
	}
	canonical, err := canonicalPath(name, allowMissing)
	if err != nil {
		return "", false, err
	}
	for _, root := range p.protectedRoots {
		if canonical == root || within(canonical, root) {
			return root, true, nil
		}
	}
	return canonical, false, nil
}

// CommandSpec is an executable argv plus the working directory and child
// environment selected by a ToolRuntime.
type CommandSpec struct {
	Program string
	Args    []string
	Dir     string
	Env     []string
}

// WrappedCommand is the executable argv after applying the OS sandbox. The
// caller should use Program, Args, Dir, and Env to build exec.Cmd.
type WrappedCommand struct {
	Program string
	Args    []string
	Dir     string
	Env     []string
	Backend string
}

// Status describes the backend that would be used for this policy. It is safe
// to show to users: it contains roots and capability state, never secrets.
type Status struct {
	Mode           Mode
	Backend        string
	Network        NetworkMode
	Workspace      string
	ReadRoots      []string
	WriteRoots     []string
	CacheRoots     []string
	ImmutableRoots []string
	TempRoots      []string
	ProtectedRoots []string
	Degraded       bool
	Reason         string
}

// Status returns policy and backend availability without starting a child.
func (p *Policy) Status() Status {
	if p == nil {
		return Status{Degraded: true, Reason: "no execution policy configured"}
	}
	status := Status{
		Mode:           p.mode,
		Network:        p.network,
		Workspace:      p.workspace,
		ReadRoots:      p.ReadRoots(),
		WriteRoots:     p.WriteRoots(),
		CacheRoots:     p.CacheRoots(),
		ImmutableRoots: p.ImmutableRoots(),
		TempRoots:      p.TempRoots(),
		ProtectedRoots: p.ProtectedRoots(),
	}
	if p.mode == ModeDangerFull {
		status.Backend = "unrestricted (explicit danger-full-access)"
		return status
	}
	status.Backend, status.Reason = backendStatus(p)
	status.Degraded = status.Backend == ""
	return status
}

func canonicalExistingDir(name string) (string, error) {
	resolved, err := canonicalRoot(name)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", name)
	}
	return resolved, nil
}

func canonicalRoot(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("path is empty")
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if _, statErr := os.Lstat(abs); statErr == nil {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", err
		}
		return filepath.Clean(resolved), nil
	}
	// Resolve the nearest existing parent and append only the lexical suffix.
	// This prevents a future symlinked parent from changing the grant's meaning.
	missing := []string{}
	current := abs
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("cannot resolve root %q", name)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func canonicalPath(name string, allowMissing bool) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("path is empty")
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(abs); err == nil {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", err
		}
		return filepath.Clean(resolved), nil
	} else if !allowMissing {
		return "", err
	}
	return canonicalRoot(abs)
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ""
}

func anyWithin(path string, roots []string) bool {
	for _, root := range roots {
		if path == root || within(path, root) {
			return true
		}
	}
	return false
}

func appendUnique(dst []string, values ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(values))
	for _, item := range dst {
		seen[item] = struct{}{}
	}
	for _, item := range values {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		dst = append(dst, item)
	}
	return dst
}

func backendStatus(p *Policy) (string, string) {
	switch runtime.GOOS {
	case "darwin":
		if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
			return "", "macOS /usr/bin/sandbox-exec is unavailable"
		}
		return "macos-seatbelt", ""
	case "linux":
		path := ""
		if p != nil {
			path = p.bubblewrapPath
		}
		if _, err := lookupBubblewrap(path); err != nil {
			return "", err.Error()
		}
		if reason := namespaceBlockReason(); reason != "" {
			return "", reason
		}
		return "bubblewrap", ""
	default:
		return "", fmt.Sprintf("OS sandboxing is unsupported on %s", runtime.GOOS)
	}
}
