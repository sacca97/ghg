package bashrun

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Bash children carry the agent markers so scripts can detect they're under ghg.
func TestChildMarkers(t *testing.T) {
	SetMarkers("sess123", "kimi-k3-fast")
	res := Run(context.Background(), Options{Command: "env", Timeout: 5 * time.Second})
	for _, want := range []string{"GHG=1", "GHG_SESSION_ID=sess123", "GHG_MODEL=kimi-k3-fast", "GHG_PID="} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("child env missing %q:\n%s", want, res.Output)
		}
	}
}
