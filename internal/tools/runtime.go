package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/sandbox"
)

// ApprovalMode selects who may approve a capability that the deterministic
// policy classifies as ambiguous.
type ApprovalMode string

const (
	ApprovalAsk        ApprovalMode = "ask"
	ApprovalAutoReview ApprovalMode = "auto-review"
	ApprovalNever      ApprovalMode = "never"
)

// ParseApprovalMode validates a user/configuration value. Empty is the
// interactive default; headless callers should explicitly select never or
// auto-review.
func ParseApprovalMode(value string) (ApprovalMode, error) {
	switch ApprovalMode(strings.TrimSpace(strings.ToLower(value))) {
	case "", ApprovalAsk:
		return ApprovalAsk, nil
	case ApprovalAutoReview:
		return ApprovalAutoReview, nil
	case ApprovalNever:
		return ApprovalNever, nil
	default:
		return "", fmt.Errorf("unknown approval mode %q (want ask, auto-review, or never)", value)
	}
}

// ApprovalDecision is the only model-reviewer output the execution layer
// accepts. The reviewer cannot grant a broader capability or persist a rule.
type ApprovalDecision string

const (
	ApprovalApproveOnce     ApprovalDecision = "approve_once"
	ApprovalDeny            ApprovalDecision = "deny"
	ApprovalEscalateToHuman ApprovalDecision = "escalate_to_user"
)

// CommandSegment is a bounded, best-effort shell unit. Operator is the shell
// operator immediately before this unit; argv is used for deterministic
// classification and the reviewer receives the same segmented shape.
type CommandSegment struct {
	Command  string   `json:"command"`
	Argv     []string `json:"argv"`
	Operator string   `json:"operator,omitempty"`
}

// ApprovalRequest is the redacted capability request sent to a human or the
// optional tiny reviewer. It contains no conversation, file contents, or
// child environment. Command and segment text are redacted before dispatch.
type ApprovalRequest struct {
	Tool                string           `json:"tool"`
	Command             string           `json:"command"`
	Segments            []CommandSegment `json:"segments"`
	CWD                 string           `json:"cwd"`
	ReadRoots           []string         `json:"read_roots,omitempty"`
	WriteRoots          []string         `json:"write_roots,omitempty"`
	RequestedReadRoots  []string         `json:"requested_read_roots,omitempty"`
	RequestedWriteRoots []string         `json:"requested_write_roots,omitempty"`
	Network             bool             `json:"network"`
	Classification      string           `json:"classification"`
	Goal                string           `json:"goal,omitempty"`
	Justification       string           `json:"justification,omitempty"`
	Fingerprint         string           `json:"fingerprint"`
}

// ApprovalResult is returned by an approval reviewer.
type ApprovalResult struct {
	Decision   ApprovalDecision `json:"decision"`
	Reason     string           `json:"reason"`
	Confidence float64          `json:"confidence"`
}

// ReviewerCall accounts the optional tiny model separately from ordinary
// model-call telemetry.
type ReviewerCall struct {
	Role      string    `json:"role"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Protocol  string    `json:"protocol"`
	Purpose   string    `json:"purpose"`
	Usage     llm.Usage `json:"usage"`
	LatencyMS int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
}

// ApprovalReviewer is intentionally a function rather than an agent
// interface: the runtime can invoke one bounded, tool-less tiny-model call
// without importing the agent package and creating a cycle.
type ApprovalReviewer func(context.Context, ApprovalRequest) (ApprovalResult, error)

// ExecutionAudit describes one deterministic/reviewer decision. Callers may
// serialize it as telemetry; it deliberately contains only a fingerprint and
// redacted request metadata.
type ExecutionAudit struct {
	Request     ApprovalRequest `json:"request"`
	Disposition string          `json:"disposition"`
	Reviewer    ApprovalResult  `json:"reviewer,omitempty"`
	Granted     string          `json:"granted,omitempty"`
	LatencyMS   int64           `json:"latency_ms,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// ToolRuntime is the per-agent execution seam. Its policy is immutable; the
// approval state is session-local and shared by child runtimes. Child returns
// a narrowed view of the same boundary and never reconstructs a permissive
// default.
type ToolRuntime struct {
	Policy          *sandbox.Policy
	ApprovalMode    ApprovalMode
	Reviewer        ApprovalReviewer
	SecretNames     []string
	TempDir         string
	HumanGate       func(GateRequest) (GateDecision, string)
	LanguageService LanguageService
	PostEditHooks   []PostEditHook
	Headless        bool
	Goal            string
	Justification   string
	OnAudit         func(ExecutionAudit)
	OnReviewerCall  func(ReviewerCall)

	envOverrides map[string]string
	state        *runtimeState
}

// PostEditHook is a trusted, direct-argv command run after a successful
// publication. Extensions are normalized (for example, ".go") and an empty
// list matches every mutated file.
type PostEditHook struct {
	Command    []string
	Extensions []string
	Timeout    time.Duration
}

type runtimeState struct {
	mu       sync.Mutex
	audits   []ExecutionAudit
	approval map[string]*approvalFlight
}

type approvalFlight struct {
	done     chan struct{}
	decision GateDecision
	checked  bool
	err      error
}

// NewToolRuntime makes a runtime with the supplied immutable policy. A nil
// policy is allowed for tests/legacy callers but does not provide sandboxing.
func NewToolRuntime(policy *sandbox.Policy, mode ApprovalMode, headless bool) (*ToolRuntime, error) {
	parsed, err := ParseApprovalMode(string(mode))
	if err != nil {
		return nil, err
	}
	return &ToolRuntime{Policy: policy, ApprovalMode: parsed, Headless: headless, state: &runtimeState{approval: make(map[string]*approvalFlight)}}, nil
}

// Child returns the same policy and approval boundary for a delegated agent.
// A future narrowed child can replace Policy before use; this method never
// widens roots or changes approval mode.
func (r *ToolRuntime) Child() *ToolRuntime {
	if r == nil {
		return nil
	}
	child := *r
	child.SecretNames = slices.Clone(r.SecretNames)
	child.PostEditHooks = clonePostEditHooks(r.PostEditHooks)
	child.envOverrides = mapsClone(r.envOverrides)
	if child.state == nil {
		child.state = &runtimeState{approval: make(map[string]*approvalFlight)}
	}
	return &child
}

func mapsClone(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// WithPolicy returns a call-scoped child runtime using the supplied policy.
// It is used after a one-shot capability approval; the parent session policy
// remains unchanged for concurrent tool calls.
func (r *ToolRuntime) WithPolicy(policy *sandbox.Policy) *ToolRuntime {
	child := r.Child()
	if child == nil {
		return nil
	}
	child.Policy = policy
	return child
}

type runtimeContextKey struct{}

// WithRuntime attaches the runtime to a tool call context.
func WithRuntime(ctx context.Context, runtime *ToolRuntime) context.Context {
	if runtime == nil {
		return ctx
	}
	return context.WithValue(ctx, runtimeContextKey{}, runtime)
}

// RuntimeFromContext returns the runtime selected by the owning agent.
func RuntimeFromContext(ctx context.Context) *ToolRuntime {
	if ctx == nil {
		return nil
	}
	runtime, _ := ctx.Value(runtimeContextKey{}).(*ToolRuntime)
	return runtime
}

// WrapCommand exposes the same OS boundary to non-Bash subprocesses such as
// search's short git-status hint and future hooks.
func (r *ToolRuntime) WrapCommand(spec sandbox.CommandSpec) (sandbox.WrappedCommand, error) {
	if r == nil || r.Policy == nil {
		return sandbox.WrappedCommand{Program: spec.Program, Args: append([]string(nil), spec.Args...), Dir: spec.Dir, Env: append([]string(nil), spec.Env...), Backend: "none"}, nil
	}
	return r.Policy.WrapCommand(spec)
}

// AuthorizePath applies the native path policy. With no runtime it preserves
// the historical behavior, which keeps direct package tests and integrations
// that have not opted into a runtime source-compatible.
func AuthorizePath(ctx context.Context, name string, access sandbox.Access, allowMissing bool) (string, error) {
	if runtime := RuntimeFromContext(ctx); runtime != nil && runtime.Policy != nil {
		return runtime.Policy.Authorize(name, access, allowMissing)
	}
	return name, nil
}

// ChildEnv returns a minimal baseline environment. Explicit values are for a
// configured local MCP server; Bash/LSP callers pass nil and therefore never
// receive provider keys or secret-resolver inputs from ghg's process.
func (r *ToolRuntime) ChildEnv(explicit map[string]string) []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "SHELL": true, "TERM": true,
		"TERM_PROGRAM": true, "COLORTERM": true, "TMPDIR": true,
		"TMP": true, "TEMP": true, "USER": true, "LOGNAME": true,
		"LANG": true, "NO_COLOR": true, "CI": true, "GOCACHE": true,
		"GOMODCACHE": true, "GOPATH": true, "GOROOT": true,
		"GOTOOLCHAIN": true, "GOPROXY": true, "GOSUMDB": true,
		"GONOSUMDB": true, "GOPRIVATE": true, "XDG_CACHE_HOME": true,
		"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "CARGO_HOME": true,
		"RUSTUP_HOME": true, "NPM_CONFIG_CACHE": true, "BUN_INSTALL": true,
	}
	values := make(map[string]string)
	for _, pair := range os.Environ() {
		key, _, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if allowed[key] || strings.HasPrefix(key, "LC_") {
			values[key] = pair[strings.IndexByte(pair, '=')+1:]
		}
	}
	for key, value := range explicit {
		if key == "" || strings.ContainsRune(key, '=') {
			continue
		}
		values[key] = value
	}
	if r != nil {
		for key, value := range r.envOverrides {
			values[key] = value
		}
	}
	if _, ok := values["GIT_CONFIG_GLOBAL"]; !ok {
		values["GIT_CONFIG_GLOBAL"] = "/dev/null"
	}
	if _, ok := values["GIT_CONFIG_NOSYSTEM"]; !ok {
		values["GIT_CONFIG_NOSYSTEM"] = "1"
	}
	if _, ok := values["GIT_TERMINAL_PROMPT"]; !ok {
		values["GIT_TERMINAL_PROMPT"] = "0"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

// SafeExplicitEnv removes likely secret-bearing variables before an
// explicitly configured environment is attached to a restricted child.
func (r *ToolRuntime) SafeExplicitEnv(explicit map[string]string) map[string]string {
	return safeExplicitEnv(explicit, r.SecretNames)
}

// RedactText applies the same bounded secret redaction used for approval and
// audit payloads to a caller-owned diagnostic string.
func (r *ToolRuntime) RedactText(value string) string {
	if r == nil {
		return redactFreeText(value)
	}
	return redactFreeText(value, r.SecretNames)
}

// SafeExplicitEnv removes likely secret-bearing variables before an
// explicitly configured environment is attached to Bash/LSP/hooks. MCP uses
// ChildEnv directly because its configuration is the deliberate capability
// declaration for that server.
func SafeExplicitEnv(explicit map[string]string) map[string]string {
	return safeExplicitEnv(explicit)
}

func safeExplicitEnv(explicit map[string]string, secretNames ...[]string) map[string]string {
	filtered := make(map[string]string, len(explicit))
	for key, value := range explicit {
		var patterns []string
		if len(secretNames) > 0 {
			patterns = secretNames[0]
		}
		if !sensitiveEnvName(key, patterns...) {
			filtered[key] = value
		}
	}
	return filtered
}

func sensitiveEnvName(name string, configured ...string) bool {
	name = strings.ToUpper(name)
	for _, marker := range []string{"API_KEY", "API_TOKEN", "AUTH", "PASSWORD", "SECRET", "PRIVATE_KEY", "CREDENTIAL", "ACCESS_TOKEN", "_TOKEN"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	for _, pattern := range configured {
		pattern = strings.TrimSpace(strings.ToUpper(pattern))
		if pattern == "" {
			continue
		}
		if matched, err := path.Match(pattern, name); (err == nil && matched) || name == pattern || strings.Contains(name, pattern) {
			return true
		}
	}
	return false
}

type commandDisposition string

const (
	dispositionRoutine  commandDisposition = "routine"
	dispositionReview   commandDisposition = "review"
	dispositionHuman    commandDisposition = "human-only"
	dispositionHardDeny commandDisposition = "hard-deny"
)

// authorizeCommand decides whether a Bash call needs an additional
// capability approval. It returns the policy to use for the exact call and a
// bool indicating that an approval decision already covered the legacy TUI
// gate, preventing duplicate prompts.
func (r *ToolRuntime) authorizeCommand(ctx context.Context, tool, command, cwd string) (*sandbox.Policy, bool, error) {
	if r == nil || r.Policy == nil {
		return nil, false, nil
	}
	if r.Policy.Mode() == sandbox.ModeDangerFull {
		// Full access is an explicit configuration choice, not an approval
		// result. Do not parse or reinterpret shell syntax on this path.
		return r.Policy, false, nil
	}
	segments, parseErr := SegmentShell(command)
	if parseErr != nil {
		return r.denyApproval(ctx, ApprovalRequest{Tool: tool, Command: redactCommand(command, r.SecretNames), Classification: string(dispositionHardDeny), Fingerprint: operationFingerprint(tool, command, cwd)}, "opaque shell syntax cannot be safely segmented")
	}
	disposition, reason, network := classifyCommand(segments, r.Policy)
	requestedReadRoots, requestedWriteRoots, rootsErr := requestedCommandRoots(segments, r.Policy)
	if rootsErr != nil {
		request := r.approvalRequest(tool, command, cwd, segments, dispositionHardDeny, false, reason, nil, nil)
		return r.denyApproval(ctx, request, rootsErr.Error())
	}
	if disposition != dispositionHardDeny && (len(requestedReadRoots) > 0 || len(requestedWriteRoots) > 0) {
		disposition = dispositionHuman
		reason = "the command requests a capability outside the configured roots"
	}
	request := r.approvalRequest(tool, command, cwd, segments, disposition, network, reason, requestedReadRoots, requestedWriteRoots)
	switch disposition {
	case dispositionRoutine:
		return r.Policy, false, nil
	case dispositionHardDeny:
		return r.denyApproval(ctx, request, reason)
	}
	// Human-only operations must never be delegated to the tiny reviewer. The
	// reviewer is limited to the deterministic ambiguous middle, while an
	// external root or protected metadata capability is a direct user choice.
	decision, checked, err := r.reviewOrHuman(ctx, request, disposition == dispositionReview)
	if err != nil {
		return r.Policy, checked, err
	}
	if decision != GateAllowOnce && decision != GateAllowAlways {
		return r.Policy, true, errors.New("capability approval was denied")
	}
	granted, err := r.grantRequest(request)
	if err != nil {
		return r.Policy, true, err
	}
	r.audit(ExecutionAudit{Request: request, Disposition: string(disposition), Granted: grantedCapability(request)})
	return granted, true, nil
}

func (r *ToolRuntime) approvalRequest(tool, command, cwd string, segments []CommandSegment, disposition commandDisposition, network bool, reason string, requestedReadRoots, requestedWriteRoots []string) ApprovalRequest {
	canonicalCWD := cwd
	if r.Policy != nil && cwd != "" {
		if path, err := r.Policy.Authorize(cwd, sandbox.AccessRead, false); err == nil {
			canonicalCWD = path
		}
	}
	redacted := make([]CommandSegment, len(segments))
	for i, segment := range segments {
		redacted[i] = segment
		redacted[i].Command = redactCommand(segment.Command, r.SecretNames)
		redacted[i].Argv = redactArgv(segment.Argv, r.SecretNames)
	}
	return ApprovalRequest{
		Tool: tool, Command: redactCommand(command, r.SecretNames), Segments: redacted, CWD: canonicalCWD,
		ReadRoots: r.Policy.ReadRoots(), WriteRoots: r.Policy.WriteRoots(), Network: network,
		RequestedReadRoots: append([]string(nil), requestedReadRoots...), RequestedWriteRoots: append([]string(nil), requestedWriteRoots...),
		Classification: string(disposition), Goal: truncateApprovalText(redactFreeText(r.Goal, r.SecretNames), 1000),
		Justification: truncateApprovalText(redactFreeText(reason+" "+r.Justification, r.SecretNames), 1000), Fingerprint: operationFingerprint(tool, command, canonicalCWD),
	}
}

func (r *ToolRuntime) reviewOrHuman(ctx context.Context, request ApprovalRequest, allowAutoReview bool) (GateDecision, bool, error) {
	if r == nil {
		return GateReject, true, errors.New("capability approval runtime is nil")
	}
	if r.state == nil {
		return r.reviewOrHumanOnce(ctx, request, allowAutoReview)
	}
	key := request.Fingerprint + "\x00" + fmt.Sprint(allowAutoReview)
	r.state.mu.Lock()
	if r.state.approval == nil {
		r.state.approval = make(map[string]*approvalFlight)
	}
	if flight, ok := r.state.approval[key]; ok {
		r.state.mu.Unlock()
		select {
		case <-flight.done:
			return flight.decision, flight.checked, flight.err
		case <-ctx.Done():
			return GateReject, true, ctx.Err()
		}
	}
	flight := &approvalFlight{done: make(chan struct{})}
	r.state.approval[key] = flight
	r.state.mu.Unlock()

	decision, checked, err := r.reviewOrHumanOnce(ctx, request, allowAutoReview)
	r.state.mu.Lock()
	flight.decision, flight.checked, flight.err = decision, checked, err
	delete(r.state.approval, key)
	close(flight.done)
	r.state.mu.Unlock()
	return decision, checked, err
}

func (r *ToolRuntime) reviewOrHumanOnce(ctx context.Context, request ApprovalRequest, allowAutoReview bool) (GateDecision, bool, error) {
	approvalMode := r.ApprovalMode
	if approvalMode == "" {
		approvalMode = ApprovalAsk
	}
	if allowAutoReview && approvalMode == ApprovalAutoReview {
		if r.Reviewer == nil {
			return GateReject, true, errors.New("auto-review is enabled but no tiny reviewer is configured")
		}
		result, err := r.Reviewer(ctx, request)
		if err == nil && result.Decision == ApprovalApproveOnce && result.Confidence >= 0.70 && strings.TrimSpace(result.Reason) != "" {
			r.audit(ExecutionAudit{Request: request, Disposition: "reviewer", Reviewer: sanitizeApprovalResult(result, r.SecretNames)})
			return GateAllowOnce, true, nil
		}
		r.audit(ExecutionAudit{Request: request, Disposition: "reviewer", Reviewer: sanitizeApprovalResult(result, r.SecretNames), Error: approvalError(result, err, r.SecretNames)})
		if err == nil && result.Decision == ApprovalDeny {
			return GateReject, true, errors.New("approval reviewer denied the capability")
		}
		// Malformed/failed/low-confidence output is a human fallback only in
		// an interactive run. It is never converted into approval.
		if r.Headless {
			if err != nil {
				return GateReject, true, errors.New("approval reviewer failed closed: " + redactFreeText(err.Error(), r.SecretNames))
			}
			return GateReject, true, errors.New("approval reviewer returned low confidence")
		}
	}
	if r.Headless || r.HumanGate == nil {
		return GateReject, true, errors.New("capability approval requires an interactive human reviewer")
	}
	rule := CommandRule(request.Command)
	if request.Classification == string(dispositionHuman) {
		// Human-only operations include protected metadata and external roots;
		// never let their "always" choice collapse to a broad command prefix.
		rule = normalizeShellText(request.Command)
	}
	decision, redirect := r.HumanGate(GateRequest{Tool: request.Tool, Command: request.Command, Rule: rule})
	if decision == GateReject {
		if redirect == "" {
			redirect = "the user rejected this action"
		}
		return GateReject, true, errors.New(redactFreeText(redirect, r.SecretNames))
	}
	return decision, true, nil
}

func (r *ToolRuntime) denyApproval(ctx context.Context, request ApprovalRequest, reason string) (*sandbox.Policy, bool, error) {
	reason = redactFreeText(reason, r.SecretNames)
	request.Justification = truncateApprovalText(reason, 1000)
	r.audit(ExecutionAudit{Request: request, Disposition: string(dispositionHardDeny), Error: reason})
	return r.Policy, true, fmt.Errorf("execution policy denied: %s", reason)
}

func (r *ToolRuntime) audit(audit ExecutionAudit) {
	if r == nil {
		return
	}
	audit = r.redactAudit(audit)
	if r.state == nil {
		r.state = &runtimeState{approval: make(map[string]*approvalFlight)}
	}
	r.state.mu.Lock()
	if len(r.state.audits) >= 32 {
		copy(r.state.audits, r.state.audits[len(r.state.audits)-31:])
		r.state.audits = r.state.audits[:31]
	}
	r.state.audits = append(r.state.audits, audit)
	r.state.mu.Unlock()
	if r.OnAudit != nil {
		r.OnAudit(audit)
	}
}

func (r *ToolRuntime) redactAudit(audit ExecutionAudit) ExecutionAudit {
	audit.Request.Command = redactCommand(audit.Request.Command, r.SecretNames)
	audit.Request.Goal = redactFreeText(audit.Request.Goal, r.SecretNames)
	audit.Request.Justification = redactFreeText(audit.Request.Justification, r.SecretNames)
	if audit.Request.Segments != nil {
		audit.Request.Segments = slices.Clone(audit.Request.Segments)
		for i := range audit.Request.Segments {
			audit.Request.Segments[i].Command = redactCommand(audit.Request.Segments[i].Command, r.SecretNames)
			audit.Request.Segments[i].Argv = redactArgv(audit.Request.Segments[i].Argv, r.SecretNames)
		}
	}
	audit.Reviewer = sanitizeApprovalResult(audit.Reviewer, r.SecretNames)
	audit.Error = redactFreeText(audit.Error, r.SecretNames)
	return audit
}

// Audits returns the bounded recent approval/denial history for context
// diagnostics. Requests are already redacted before they enter this list.
func (r *ToolRuntime) Audits() []ExecutionAudit {
	if r == nil || r.state == nil {
		return nil
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	return slices.Clone(r.state.audits)
}

func (r *ToolRuntime) grantRequest(request ApprovalRequest) (*sandbox.Policy, error) {
	regularWriteRoots := make([]string, 0, len(request.RequestedWriteRoots))
	protectedWriteRoots := make([]string, 0)
	for _, root := range request.RequestedWriteRoots {
		protected, ok, err := r.Policy.ProtectedRootFor(root, true)
		if err != nil {
			return nil, err
		}
		if ok && protected == root {
			protectedWriteRoots = appendUniqueString(protectedWriteRoots, root)
			continue
		}
		regularWriteRoots = appendUniqueString(regularWriteRoots, root)
	}
	granted, err := r.Policy.Grant(request.RequestedReadRoots, regularWriteRoots, request.Network)
	if err != nil {
		return nil, err
	}
	if len(protectedWriteRoots) > 0 {
		granted, err = granted.GrantProtected(protectedWriteRoots)
		if err != nil {
			return nil, err
		}
	}
	return granted, nil
}

func grantedCapability(request ApprovalRequest) string {
	var capabilities []string
	if request.Network {
		capabilities = append(capabilities, "network")
	}
	if len(request.RequestedReadRoots) > 0 {
		capabilities = append(capabilities, "read-roots")
	}
	if len(request.RequestedWriteRoots) > 0 {
		capabilities = append(capabilities, "write-roots")
	}
	if len(capabilities) == 0 {
		return "human-approved operation"
	}
	return strings.Join(capabilities, ", ")
}

func appendUniqueString(dst []string, values ...string) []string {
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

func approvalError(result ApprovalResult, err error, patterns ...[]string) string {
	if err != nil {
		return redactFreeText(err.Error(), patterns...)
	}
	return redactFreeText(fmt.Sprintf("decision=%s confidence=%.2f", result.Decision, result.Confidence), patterns...)
}

func sanitizeApprovalResult(result ApprovalResult, patterns []string) ApprovalResult {
	result.Reason = redactFreeText(result.Reason, patterns)
	return result
}

func operationFingerprint(tool, command, cwd string) string {
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write([]byte(command))
	h.Write([]byte{0})
	h.Write([]byte(cwd))
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func truncateApprovalText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

const redactedApprovalValue = "<redacted>"

func redactArgv(argv []string, patterns ...[]string) []string {
	out := slices.Clone(argv)
	configured := configuredSecretNames(patterns...)
	redactNext := false
	redactHeaderValue := false
	for i, token := range out {
		if redactHeaderValue {
			if isCredentialScheme(token) {
				out[i] = token
				redactNext = true
			} else {
				out[i] = redactedApprovalValue
			}
			redactHeaderValue = false
			continue
		}
		if redactNext {
			out[i] = redactedApprovalValue
			if strings.HasSuffix(token, ":") && isSensitiveField(strings.TrimSuffix(token, ":"), configured) {
				redactHeaderValue = true
			}
			redactNext = false
			continue
		}
		if key, _, ok := strings.Cut(token, "="); ok && isSensitiveField(key, configured) {
			out[i] = key + "=" + redactedApprovalValue
			continue
		}
		if key, _, ok := strings.Cut(token, "="); ok && isAssignment(key) && sensitiveEnvName(key, configured...) {
			out[i] = key + "=" + redactedApprovalValue
			continue
		}
		if redacted := redactInlineToken(token, configured); redacted != token {
			out[i] = redacted
			continue
		}
		if strings.HasSuffix(token, ":") && isSensitiveField(strings.TrimSuffix(token, ":"), configured) {
			out[i] = token
			redactHeaderValue = true
			continue
		}
		if isSensitiveField(token, configured) {
			if len(token) > 2 && (strings.HasPrefix(token, "-H") || strings.HasPrefix(token, "-u") || strings.HasPrefix(token, "-p")) {
				out[i] = token[:2] + redactedApprovalValue
			} else {
				redactNext = true
			}
			continue
		}
		out[i] = token
	}
	return out
}

func configuredSecretNames(patterns ...[]string) []string {
	if len(patterns) == 0 {
		return nil
	}
	return patterns[0]
}

func isSensitiveField(field string, configured []string) bool {
	field = strings.TrimSpace(field)
	if field == "-H" || field == "-u" || field == "-p" {
		return true
	}
	if strings.HasPrefix(field, "-H") || strings.HasPrefix(field, "-u") || strings.HasPrefix(field, "-p") {
		return true
	}
	field = strings.TrimLeft(strings.ToLower(field), "-")
	if field == "" {
		return false
	}
	for _, name := range []string{"header", "user", "api-key", "api_key", "apikey", "token", "access-token", "access_token", "authorization", "auth", "password", "passwd", "secret", "credential", "private-key", "private_key", "cookie"} {
		if field == name || strings.Contains(field, name) {
			return true
		}
	}
	return sensitiveEnvName(field, configured...)
}

func redactInlineToken(token string, configured []string) string {
	if redacted, ok := redactCredentialURL(token, configured); ok {
		return redacted
	}
	trimmed := strings.TrimSpace(token)
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"bearer ", "basic ", "token "} {
		if strings.HasPrefix(lower, prefix) && len(trimmed) > len(prefix) {
			return trimmed[:len(prefix)] + redactedApprovalValue
		}
	}
	if colon := strings.IndexByte(token, ':'); colon > 0 && colon+1 < len(token) && isSensitiveField(token[:colon], configured) {
		return token[:colon] + ": " + redactedApprovalValue
	}
	return token
}

func redactCredentialURL(token string, configured []string) (string, bool) {
	u, err := url.Parse(token)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return token, false
	}
	changed := false
	if u.User != nil {
		u.User = url.User(redactedApprovalValue)
		changed = true
	}
	query := u.Query()
	for key := range query {
		if isSensitiveField(key, configured) {
			query.Set(key, redactedApprovalValue)
			changed = true
		}
	}
	if !changed {
		return token, false
	}
	u.RawQuery = query.Encode()
	return u.String(), true
}

func redactFreeText(value string, patterns ...[]string) string {
	fields := strings.Fields(value)
	configured := configuredSecretNames(patterns...)
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if strings.HasSuffix(field, ":") && isSensitiveField(strings.TrimSuffix(field, ":"), configured) {
			if i+1 >= len(fields) {
				continue
			}
			if i+2 < len(fields) && isCredentialScheme(fields[i+1]) {
				fields[i+2] = redactedApprovalValue
			} else {
				fields[i+1] = redactedApprovalValue
			}
			continue
		}
		if isCredentialScheme(field) && i+1 < len(fields) {
			fields[i+1] = redactedApprovalValue
			i++
		}
	}
	return strings.Join(redactArgv(fields, patterns...), " ")
}

func isCredentialScheme(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "bearer", "basic", "token":
		return true
	default:
		return false
	}
}

func redactCommand(command string, patterns ...[]string) string {
	segments, err := SegmentShell(command)
	if err != nil {
		return opaqueApprovalCommand(command)
	}
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		argv := redactArgv(segment.Argv, patterns...)
		if len(argv) == 0 {
			continue
		}
		if segment.Operator != "" && len(parts) > 0 {
			parts[len(parts)-1] += " " + segment.Operator
		}
		parts = append(parts, strings.Join(argv, " "))
	}
	if len(parts) == 0 {
		return opaqueApprovalCommand(command)
	}
	return truncateApprovalText(strings.Join(parts, " "), 2000)
}

func opaqueApprovalCommand(command string) string {
	return "<opaque-shell-command:" + operationFingerprint("shell", command, "") + ">"
}
