package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func testBash(ctx context.Context, command string, timeout time.Duration, update func(string)) bashResult {
	return runBashCommand(ctx, bashOptions{Command: command, Timeout: timeout, OnUpdate: update})
}

func TestBashDoesNotHangOnTTYRead(t *testing.T) {
	res := testBash(context.Background(), `exec 3< /dev/tty; read -r line <&3; echo "got: $line"`, 5*time.Second, nil)
	if res.TimedOut {
		t.Fatalf("command hung and timed out: %+v", res)
	}
	if res.Output == "" && res.Exit == "" {
		t.Fatalf("expected a fast non-zero exit: %+v", res)
	}
}

func TestBashCapture(t *testing.T) {
	res := testBash(context.Background(), `echo hi; echo err >&2; exit 3`, 0, nil)
	if !strings.Contains(res.Output, "hi") || !strings.Contains(res.Output, "err") {
		t.Fatalf("output missing: %q", res.Output)
	}
	if !strings.Contains(res.Exit, "exit") || !strings.Contains(res.Exit, "3") {
		t.Fatalf("exit status wrong: %q", res.Exit)
	}
	if res.TimedOut {
		t.Fatalf("should not time out: %+v", res)
	}
}

func TestBashOnUpdateSnapshots(t *testing.T) {
	var snapshots []string
	res := testBash(context.Background(), `i=0; while [ "$i" -lt 4 ]; do echo "line-$i"; i=$((i+1)); sleep 0.12; done`, 5*time.Second, func(snapshot string) {
		snapshots = append(snapshots, snapshot)
	})
	if res.Exit != "" || res.TimedOut {
		t.Fatalf("snapshot command failed: %+v", res)
	}
	if len(snapshots) < 2 || !strings.Contains(snapshots[len(snapshots)-1], "line-3") {
		t.Fatalf("unexpected snapshots: %q", snapshots)
	}
	if !strings.Contains(res.Output, "line-3") {
		t.Fatalf("result should retain complete output: %q", res.Output)
	}
}

func TestBashCleanExit(t *testing.T) {
	res := testBash(context.Background(), `true`, 0, nil)
	if res.Output != "" || res.Exit != "" {
		t.Fatalf("clean exit should be empty: %+v", res)
	}
}

func TestBashCaptureIsBounded(t *testing.T) {
	const extra = 1024
	res := testBash(context.Background(), `yes x | head -c 10486784`, 10*time.Second, nil)
	if res.OriginalBytes != 10485760+extra {
		t.Fatalf("original byte count = %d, want %d", res.OriginalBytes, 10485760+extra)
	}
	if res.Complete || len(res.Output) > 10485760 {
		t.Fatalf("oversized output was not bounded: %+v", res)
	}
}

func TestBashTimeoutAndCancellation(t *testing.T) {
	res := testBash(context.Background(), `sleep 5`, 100*time.Millisecond, nil)
	if !res.TimedOut || !res.Killed || !strings.Contains(res.Exit, "timed out") {
		t.Fatalf("expected timeout: %+v", res)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	res = testBash(ctx, `sleep 5`, 10*time.Second, nil)
	if !res.Killed {
		t.Fatalf("cancellation should kill: %+v", res)
	}
}

func TestUserShellResolution(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	if sh := userShell(); sh != "/bin/zsh" {
		t.Fatalf("$SHELL should win, got %q", sh)
	}

	t.Setenv("SHELL", "")
	if sh := userShell(); sh == "" {
		t.Fatal("empty $SHELL must fall back to a shell")
	}

	t.Setenv("SHELL", "/bin/sh")
	res := testBash(context.Background(), "echo shell-ok", 0, nil)
	if !strings.Contains(res.Output, "shell-ok") || res.Exit != "" {
		t.Fatalf("run via user shell: %+v", res)
	}
}

func TestOutputCapturePreviewRollingUTF8(t *testing.T) {
	c := NewOutputCapture(100, true)
	_, _ = c.Write([]byte("hello"))
	_, _ = c.Write([]byte{0xe4})
	_, _ = c.Write([]byte{0xb8, 0x96})
	_, _ = c.Write([]byte{0xb8, 0x96})
	if !strings.Contains(c.Preview(10), "世") {
		t.Fatalf("expected valid UTF-8 in preview, got %q", c.Preview(10))
	}

	chunk := []byte(strings.Repeat("abcdefghij", 1000))
	for i := 0; i < 100; i++ {
		_, _ = c.Write(chunk)
	}
	if c.total != 1000000+10 || len(c.Preview(50)) > 50 {
		t.Fatalf("unexpected capture accounting: total=%d preview=%d", c.total, len(c.Preview(50)))
	}
}

func TestPipefailPreserved(t *testing.T) {
	t.Setenv("SHELL", "bash")
	if res := testBash(context.Background(), "false | tail -n 1", 0, nil); res.Exit == "" {
		t.Fatalf("expected pipeline failure: %+v", res)
	}
	if res := testBash(context.Background(), "true | tail -n 1", 0, nil); res.Exit != "" {
		t.Fatalf("expected successful pipeline: %+v", res)
	}
}
