package memory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
)

// Tools returns the remember and forget tools for the supplied session scope.
func Tools(sessionID func() string) []tools.Tool {
	scopes := func() (installation, session Scope) {
		id := ""
		if sessionID != nil {
			id = sessionID()
		}
		return Installation(), Session(id)
	}
	resolve := func(name string) (Scope, error) {
		inst, sess := scopes()
		switch name {
		case "", "installation", "install", "global":
			return inst, nil
		case "session":
			if sess.Path == "" {
				return sess, fmt.Errorf("no session yet — use scope \"installation\"")
			}
			return sess, nil
		default:
			return Scope{}, fmt.Errorf("scope must be \"installation\" or \"session\", got %q", name)
		}
	}
	scopeProp := `"scope":{"type":"string","enum":["installation","session"],"description":"installation (default) = every session, remembers facts about the user; session = notes for this conversation only"}`
	remember := tools.Tool{
		Def: models.NewTool("remember",
			"Save a durable fact as one bullet in a markdown memory file that is injected back into your context every turn. Use it when the user shares a lasting preference or fact about themselves (scope=installation), or to leave a note for this conversation (scope=session). Phrase it as a standalone statement. The files are plain markdown the user can edit directly — keep entries short.",
			`{"type":"object","properties":{"text":{"type":"string","description":"The fact, phrased to stand on its own"},`+scopeProp+`},"required":["text"]}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Text  string `json:"text"`
				Scope string `json:"scope"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			s, err := resolve(in.Scope)
			if err != nil {
				return "", err
			}
			if err := s.Remember(in.Text); err != nil {
				return "", err
			}
			return fmt.Sprintf("Remembered (%s memory).", s.Name), nil
		},
	}
	forget := tools.Tool{
		Def: models.NewTool("forget",
			"Mark a saved memory entry done by its number (entries are numbered in the memory block of your context). Use it when a fact is stale or the user asks you to stop remembering something.",
			`{"type":"object","properties":{"n":{"type":"number","description":"The entry number"},`+scopeProp+`},"required":["n"]}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				N     int    `json:"n"`
				Scope string `json:"scope"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			s, err := resolve(in.Scope)
			if err != nil {
				return "", err
			}
			if err := s.Forget(in.N); err != nil {
				return "", err
			}
			return fmt.Sprintf("Forgot entry %d (%s memory).", in.N, s.Name), nil
		},
	}
	return []tools.Tool{remember, forget}
}
