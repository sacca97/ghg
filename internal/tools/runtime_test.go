package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sacca97/ghg/internal/sandbox"
)

type approvalJoinContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *approvalJoinContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func testRuntime(t *testing.T, mode ApprovalMode) (*ToolRuntime, string) {
	t.Helper()
	workspace := t.TempDir()
	policy, err := sandbox.NewPolicy(sandbox.PolicyConfig{Workspace: workspace, Mode: sandbox.ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewToolRuntime(policy, mode, mode == ApprovalNever)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, workspace
}

func TestRuntimeApprovalReviewsAndGrantsOnlyNetworkForOneCall(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalAutoReview)
	var reviewed ApprovalRequest
	runtime.Reviewer = func(_ context.Context, request ApprovalRequest) (ApprovalResult, error) {
		reviewed = request
		return ApprovalResult{Decision: ApprovalApproveOnce, Reason: "the user requested a one-shot fetch", Confidence: 0.95}, nil
	}
	policy, covered, err := runtime.authorizeCommand(context.Background(), "bash", "curl https://example.test", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !covered || policy.Network() != sandbox.NetworkHost || !reviewed.Network {
		t.Fatalf("policy=%v covered=%v reviewed=%+v", policy.Network(), covered, reviewed)
	}
	if runtime.Policy.Network() != sandbox.NetworkDeny {
		t.Fatalf("one-shot grant changed session policy to %q", runtime.Policy.Network())
	}
}

func TestRuntimeNeverFailsClosedAndChildDoesNotWiden(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalNever)
	if _, _, err := runtime.authorizeCommand(context.Background(), "bash", "curl https://example.test", workspace); err == nil {
		t.Fatal("never mode should deny network escalation")
	}
	child := runtime.Child()
	if child.Policy != runtime.Policy || child.ApprovalMode != runtime.ApprovalMode {
		t.Fatalf("child runtime lost inherited boundary: parent=%+v child=%+v", runtime, child)
	}
}

func TestRuntimeExternalRedirectRequiresHumanAndGrantsOneCallRoot(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalAutoReview)
	outside := t.TempDir()
	target := filepath.Join(outside, "result.txt")
	var reviewerCalls, humanCalls int
	runtime.Reviewer = func(_ context.Context, _ ApprovalRequest) (ApprovalResult, error) {
		reviewerCalls++
		return ApprovalResult{Decision: ApprovalApproveOnce, Reason: "reviewed", Confidence: 1}, nil
	}
	runtime.HumanGate = func(request GateRequest) (GateDecision, string) {
		humanCalls++
		if !strings.Contains(request.Command, ">") || !strings.Contains(request.Command, target) {
			t.Fatalf("human request lost the exact redirect: %+v", request)
		}
		return GateAllowOnce, ""
	}
	granted, covered, err := runtime.authorizeCommand(context.Background(), "bash", "printf ok > "+target, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !covered || reviewerCalls != 0 || humanCalls != 1 {
		t.Fatalf("covered=%v reviewerCalls=%d humanCalls=%d", covered, reviewerCalls, humanCalls)
	}
	if _, err := runtime.Policy.Authorize(target, sandbox.AccessWrite, true); err == nil {
		t.Fatal("external redirect widened the session policy")
	}
	if _, err := granted.Authorize(target, sandbox.AccessWrite, true); err != nil {
		t.Fatalf("human-approved external redirect was not granted: %v", err)
	}
}

func TestRuntimeExternalRemovalRequiresHumanAndGrantsOneCallRoot(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalAutoReview)
	outside := t.TempDir()
	target := filepath.Join(outside, "artifact.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reviewerCalls, humanCalls int
	runtime.Reviewer = func(_ context.Context, _ ApprovalRequest) (ApprovalResult, error) {
		reviewerCalls++
		return ApprovalResult{Decision: ApprovalApproveOnce, Reason: "not used for external removal", Confidence: 1}, nil
	}
	runtime.HumanGate = func(request GateRequest) (GateDecision, string) {
		humanCalls++
		if request.Command != "rm -f "+target && request.Command != "env rm -f "+target {
			t.Fatalf("human request command = %q", request.Command)
		}
		return GateAllowOnce, ""
	}
	granted, covered, err := runtime.authorizeCommand(context.Background(), "bash", "rm -f "+target, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !covered || reviewerCalls != 0 || humanCalls != 1 {
		t.Fatalf("covered=%v reviewerCalls=%d humanCalls=%d", covered, reviewerCalls, humanCalls)
	}
	if _, err := runtime.Policy.Authorize(target, sandbox.AccessWrite, true); err == nil {
		t.Fatal("external removal widened the session policy")
	}
	if _, err := granted.Authorize(target, sandbox.AccessWrite, true); err != nil {
		t.Fatalf("human-approved external removal was not granted: %v", err)
	}
	wrapped, covered, err := runtime.authorizeCommand(context.Background(), "bash", "env rm -f "+target, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !covered || reviewerCalls != 0 || humanCalls != 2 {
		t.Fatalf("wrapped removal covered=%v reviewerCalls=%d humanCalls=%d", covered, reviewerCalls, humanCalls)
	}
	if _, err := wrapped.Authorize(target, sandbox.AccessWrite, true); err != nil {
		t.Fatalf("human-approved wrapped removal was not granted: %v", err)
	}
}

func TestRuntimeHardDeniedCompoundCannotBeWidenedByRemovalGrant(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalAutoReview)
	outside := t.TempDir()
	target := filepath.Join(outside, "artifact.txt")
	var reviewerCalls, humanCalls int
	runtime.Reviewer = func(_ context.Context, _ ApprovalRequest) (ApprovalResult, error) {
		reviewerCalls++
		return ApprovalResult{Decision: ApprovalApproveOnce, Reason: "must not be called", Confidence: 1}, nil
	}
	runtime.HumanGate = func(GateRequest) (GateDecision, string) {
		humanCalls++
		return GateAllowOnce, ""
	}
	if _, _, err := runtime.authorizeCommand(context.Background(), "bash", "rm -rf / && rm -f "+target, workspace); err == nil {
		t.Fatal("hard-denied compound was approved")
	}
	if reviewerCalls != 0 || humanCalls != 0 {
		t.Fatalf("hard-denied compound reached an approval path: reviewer=%d human=%d", reviewerCalls, humanCalls)
	}
}

func TestRuntimeHumanOnlyGitMutationBypassesTinyReviewer(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalAutoReview)
	gitRoot := filepath.Join(workspace, ".git")
	if err := os.Mkdir(gitRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	var reviewerCalls, humanCalls int
	runtime.Reviewer = func(_ context.Context, _ ApprovalRequest) (ApprovalResult, error) {
		reviewerCalls++
		return ApprovalResult{Decision: ApprovalApproveOnce, Reason: "never use me for human-only work", Confidence: 1}, nil
	}
	runtime.HumanGate = func(request GateRequest) (GateDecision, string) {
		humanCalls++
		if request.Rule != "git reset --hard HEAD" {
			t.Fatalf("human gate rule = %q", request.Rule)
		}
		return GateAllowOnce, ""
	}
	granted, covered, err := runtime.authorizeCommand(context.Background(), "bash", "git reset --hard HEAD", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !covered || reviewerCalls != 0 || humanCalls != 1 {
		t.Fatalf("covered=%v reviewerCalls=%d humanCalls=%d", covered, reviewerCalls, humanCalls)
	}
	if _, err := runtime.Policy.Authorize(filepath.Join(gitRoot, "index"), sandbox.AccessWrite, true); err == nil {
		t.Fatal("session policy unexpectedly permits git metadata writes")
	}
	if _, err := granted.Authorize(filepath.Join(gitRoot, "index"), sandbox.AccessWrite, true); err != nil {
		t.Fatalf("human-approved git metadata was not granted: %v", err)
	}
	runtime.HumanGate = func(request GateRequest) (GateDecision, string) {
		humanCalls++
		if request.Rule != "env git reset --hard HEAD" {
			t.Fatalf("wrapped human gate rule = %q", request.Rule)
		}
		return GateAllowOnce, ""
	}
	wrapped, covered, err := runtime.authorizeCommand(context.Background(), "bash", "env git reset --hard HEAD", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !covered || reviewerCalls != 0 || humanCalls != 2 {
		t.Fatalf("wrapped git covered=%v reviewerCalls=%d humanCalls=%d", covered, reviewerCalls, humanCalls)
	}
	if _, err := wrapped.Authorize(filepath.Join(gitRoot, "index"), sandbox.AccessWrite, true); err != nil {
		t.Fatalf("human-approved wrapped git metadata was not granted: %v", err)
	}
}

func TestRuntimeHereDocFailsClosed(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalAsk)
	if _, _, err := runtime.authorizeCommand(context.Background(), "bash", "cat <<EOF\nsecret\nEOF", workspace); err == nil {
		t.Fatal("here-doc should fail closed")
	}
}

func TestRuntimeCoalescesConcurrentReviewerDecisions(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalAutoReview)
	started := make(chan struct{})
	release := make(chan struct{})
	var reviewerCalls atomic.Int32
	runtime.Reviewer = func(_ context.Context, _ ApprovalRequest) (ApprovalResult, error) {
		if reviewerCalls.Add(1) == 1 {
			close(started)
		}
		<-release
		return ApprovalResult{Decision: ApprovalApproveOnce, Reason: "one-shot network access", Confidence: 0.95}, nil
	}
	const command = "curl https://example.test"
	ctx := &approvalJoinContext{Context: context.Background(), joined: make(chan struct{})}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, covered, err := runtime.authorizeCommand(ctx, "bash", command, workspace)
			if err == nil && !covered {
				err = errors.New("approval was not marked covered")
			}
			results <- err
		}()
	}
	<-started
	<-ctx.joined
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := reviewerCalls.Load(); got != 1 {
		t.Fatalf("reviewer calls = %d, want one in-flight call", got)
	}
}

func TestChildEnvStripsProviderSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "secret")
	t.Setenv("GHG_TEST_RUNTIME", "should-not-pass")
	t.Setenv("TERM", "baseline")
	runtime, _ := testRuntime(t, ApprovalNever)
	env := strings.Join(runtime.ChildEnv(map[string]string{"MCP_TOKEN": "explicit", "TERM": "override"}), "\n")
	if strings.Contains(env, "OPENAI_API_KEY") || strings.Contains(env, "GHG_TEST_RUNTIME") {
		t.Fatalf("secret env leaked into child environment:\n%s", env)
	}
	if !strings.Contains(env, "MCP_TOKEN=explicit") {
		t.Fatalf("explicit MCP environment missing:\n%s", env)
	}
	if !strings.Contains(env, "TERM=override") {
		t.Fatalf("explicit environment did not override its baseline:\n%s", env)
	}
}

func TestApprovalRedactionCoversStructuredSecretForms(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalAutoReview)
	runtime.SecretNames = []string{"DEPLOY_*"}
	commands := []string{
		"TOKEN=approval-sentinel curl https://example.test",
		"curl --api-key approval-sentinel",
		"curl --api-key=approval-sentinel",
		`curl -H "Authorization: Bearer approval-sentinel"`,
		`curl --header "X-Api-Key: approval-sentinel"`,
		"curl --header Authorization: Bearer approval-sentinel",
		"curl https://user:approval-sentinel@example.test",
		"DEPLOY_SECRET=approval-sentinel curl https://example.test",
		"curl https://example.test/?access_token=approval-sentinel",
	}
	for _, command := range commands {
		segments, err := SegmentShell(command)
		if err != nil {
			t.Fatal(err)
		}
		request := runtime.approvalRequest("bash", command, workspace, segments, dispositionReview, true, "network", nil, nil)
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "approval-sentinel") {
			t.Fatalf("secret leaked in approval request for %q: %s", command, encoded)
		}
	}

	var callback ExecutionAudit
	runtime.OnAudit = func(audit ExecutionAudit) { callback = audit }
	runtime.audit(ExecutionAudit{
		Request:  ApprovalRequest{Command: `curl --token approval-sentinel`},
		Reviewer: ApprovalResult{Reason: "Authorization: Bearer approval-sentinel"},
		Error:    "request failed for --password approval-sentinel",
	})
	audits, err := json.Marshal(runtime.Audits())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(audits), "approval-sentinel") {
		t.Fatalf("secret leaked in audit history: %s", audits)
	}
	callbackJSON, err := json.Marshal(callback)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(callbackJSON), "approval-sentinel") {
		t.Fatalf("secret leaked in audit callback: %s", callbackJSON)
	}
	for _, diagnostic := range []string{
		"request failed for --password approval-sentinel",
		"Authorization: Bearer approval-sentinel",
		"curl --api-key=approval-sentinel",
	} {
		if safe := runtime.RedactText(diagnostic); strings.Contains(safe, "approval-sentinel") {
			t.Fatalf("secret leaked in diagnostic %q: %q", diagnostic, safe)
		}
	}
}

func TestMalformedApprovalCommandUsesOpaquePlaceholder(t *testing.T) {
	runtime, _ := testRuntime(t, ApprovalNever)
	request := ApprovalRequest{Command: "echo $(printf malformed-sentinel)"}
	safe := runtime.redactAudit(ExecutionAudit{Request: request})
	if strings.Contains(safe.Request.Command, "malformed-sentinel") || !strings.Contains(safe.Request.Command, "opaque-shell-command:") {
		t.Fatalf("malformed command was not replaced safely: %q", safe.Request.Command)
	}
}

func TestNativeToolsUseRuntimePathBoundary(t *testing.T) {
	runtime, workspace := testRuntime(t, ApprovalNever)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := WithRuntime(context.Background(), runtime)
	read := ExecuteResult(ctx, All(), "read", json.RawMessage(`{"path":"`+outside+`"}`))
	if !strings.Contains(read.Preview, "outside the execution roots") {
		t.Fatalf("outside read was not denied: %q", read.Preview)
	}
	write := ExecuteResult(ctx, All(), "write", json.RawMessage(`{"path":"`+filepath.Join(outside, "new.txt")+`","content":"x"}`))
	if !strings.Contains(write.Preview, "outside the execution roots") {
		t.Fatalf("outside write was not denied: %q", write.Preview)
	}
	inside := filepath.Join(workspace, "ok.txt")
	write = ExecuteResult(ctx, All(), "write", json.RawMessage(`{"path":"`+inside+`","content":"ok"}`))
	if strings.HasPrefix(write.Preview, "Error:") {
		t.Fatalf("workspace write failed: %q", write.Preview)
	}
}
