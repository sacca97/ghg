package tui

import (
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/tools"
	"os"
	"testing"
)

func TestCommandRuleArity(t *testing.T) {
	cases := map[string]string{
		"git checkout main":             "git checkout",
		"git commit -m 'x'":             "git commit",
		"npm run dev":                   "npm run dev",
		"npm install lodash":            "npm install",
		"ls -la /tmp":                   "ls",
		"rm -rf build":                  "rm",
		"FOO=bar go test ./...":         "go test",
		"docker compose up":             "docker compose up",
		"git checkout main && rm -rf /": "git checkout main && rm -rf /", // compound commands use an exact rule
		"somescript.sh --flag":          "somescript.sh",
	}
	for in, want := range cases {
		if got := tools.CommandRule(in); got != want {
			t.Errorf("CommandRule(%q) = %q, want %q", in, got, want)
		}
	}
}

// The startup gate: a trusted cwd passes without a prompt; an untrusted one
// declines when there's no terminal to ask on.
func TestTrustGate(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	wd, _ := os.Getwd()
	if err := config.Trust(wd); err != nil {
		t.Fatal(err)
	}
	ok, err := config.CheckTrust(wd)
	if err != nil || !ok {
		t.Fatalf("trusted cwd should pass: %v %v", ok, err)
	}
}
