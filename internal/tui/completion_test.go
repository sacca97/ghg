package tui

import (
	"encoding/json"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/search"
	"github.com/sacca97/ghg/internal/skills"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func texts(cs []cand) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Text
	}
	return out
}

func TestCompletions(t *testing.T) {
	models := []cand{{"kimi-k3-fast", ""}, {"glm-5.2-fast", ""}}
	provs := []cand{{"inference", ""}}

	// slash commands
	head, cs := completions("/m", models, provs, provs, nil, nil)
	if head != "" || len(cs) != 4 || cs[0].Text != "/mcp" || cs[1].Text != "/me" || cs[2].Text != "/memory" || cs[3].Text != "/model" {
		t.Fatalf("command completion: %q %v", head, texts(cs))
	}
	// export kinds
	head, cs = completions("/export-result ", models, provs, provs, nil, nil)
	if head != "/export-result " || len(cs) != 4 || cs[0].Text != "chat" || cs[1].Text != "last" || cs[2].Text != "plan" || cs[3].Text != "review" {
		t.Fatalf("export completion: %q %v", head, texts(cs))
	}
	// every slash command in the switch must be completable — the "I can't
	// see /context-doctor" regression class: the command exists but the
	// completion table was never told.
	for _, cmd := range []string{"/context-doctor", "/mcp"} {
		found := false
		for _, c := range commands {
			if c.Text == cmd {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from completion table", cmd)
		}
	}
	// /model first arg
	head, cs = completions("/model k", models, provs, provs, nil, nil)
	if head != "/model " || len(cs) != 1 || cs[0].Text != "kimi-k3-fast" {
		t.Fatalf("model completion: %q %v", head, texts(cs))
	}
	// /model second arg
	_, cs = completions("/model kimi-k3-fast inf", models, provs, provs, nil, nil)
	if len(cs) != 1 || cs[0].Text != "inference" {
		t.Fatalf("provider completion: %v", texts(cs))
	}
	// paths
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "alpha.txt"), nil, 0o644)
	os.Mkdir(filepath.Join(dir, "alphadir"), 0o755)
	head, cs = completions("fix "+dir+"/al", models, provs, provs, nil, nil)
	if head != "fix " || len(cs) != 2 {
		t.Fatalf("path completion: %q %v", head, texts(cs))
	}
	if cs[1].Text != filepath.Join(dir, "alphadir")+"/" {
		t.Fatalf("dir should get trailing slash: %v", texts(cs))
	}
	// no match
	_, cs = completions("/nope", models, provs, provs, nil, nil)
	if len(cs) != 0 {
		t.Fatalf("expected no candidates, got %v", texts(cs))
	}
	// slash-command args must never fall through to path completion
	for _, in := range []string{"/goal make yourself better", "/goal make yourself better ", "/resume ab"} {
		if _, cs = completions(in, models, provs, provs, nil, nil); len(cs) != 0 {
			t.Fatalf("%q: expected no candidates, got %v", in, texts(cs))
		}
	}
	// but @ mentions inside slash args still complete
	if _, cs = completions("/goal fix @"+dir+"/al", models, provs, provs, nil, nil); len(cs) != 2 {
		t.Fatalf("@ inside slash args: %v", texts(cs))
	}
}

// chdir runs the test in a fresh tree so completion uses the production cwd.
func chdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	search.InvalidateFileIndex(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })
	return dir
}

func write(t *testing.T, dir, rel string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFuzzyFiles(t *testing.T) {
	dir := chdir(t)
	write(t, dir, "docs/roadmap.md")
	write(t, dir, "internal/tui/roadmap_notes.txt")
	write(t, dir, "README.md")
	write(t, dir, "cmd/ghg/main.go")

	// bare word finds a nested file
	hits := fuzzyFiles("roadmap", 8)
	if len(hits) != 2 || hits[0] != "docs/roadmap.md" {
		t.Fatalf("roadmap: %v", hits)
	}
	if hits = fuzzyFiles("ROADMAP", 8); len(hits) != 2 {
		t.Fatalf("case-insensitive roadmap: %v", hits)
	}
	// base-name substring beats full-path match
	hits = fuzzyFiles("main", 8)
	if len(hits) != 1 || hits[0] != "cmd/ghg/main.go" {
		t.Fatalf("main: %v", hits)
	}
	// subsequence match
	hits = fuzzyFiles("rdmp", 8)
	if len(hits) != 2 {
		t.Fatalf("rdmp subsequence: %v", hits)
	}
	// empty query lists everything, sorted
	hits = fuzzyFiles("", 8)
	if len(hits) != 4 || hits[0] != "README.md" {
		t.Fatalf("empty: %v", hits)
	}
	// no match
	if hits = fuzzyFiles("zzz", 8); len(hits) != 0 {
		t.Fatalf("zzz: %v", hits)
	}
	// hidden and vendor dirs are skipped
	write(t, dir, ".git/config")
	write(t, dir, "vendor/pkg/mod.go")
	search.InvalidateFileIndex(dir)
	if hits = fuzzyFiles("", 32); len(hits) != 4 {
		t.Fatalf("hidden/vendor should be skipped: %v", hits)
	}
}

func TestAtMentionFuzzyCompletion(t *testing.T) {
	dir := chdir(t)
	write(t, dir, "docs/roadmap.md")
	write(t, dir, "alpha.txt")

	_, cs := completions("fix @road", nil, nil, nil, nil, nil)
	if len(cs) != 1 || cs[0].Text != "@docs/roadmap.md" {
		t.Fatalf("fuzzy @ completion: %v", texts(cs))
	}
	// path-like queries still glob
	_, cs = completions("fix @al", nil, nil, nil, nil, nil)
	if len(cs) != 1 || cs[0].Text != "@alpha.txt" {
		t.Fatalf("glob @ completion: %v", texts(cs))
	}
	_, cs = completions("fix @docs/r", nil, nil, nil, nil, nil)
	if len(cs) != 1 || cs[0].Text != "@docs/roadmap.md" {
		t.Fatalf("slash query: %v", texts(cs))
	}
}

func TestExpandMentionsFuzzy(t *testing.T) {
	dir := chdir(t)
	write(t, dir, "docs/roadmap.md")

	// resolveMentionPath stats against the real cwd, so run from the fixture
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	out := expandMentions("see @roadmap")
	abs := filepath.Join(dir, "docs", "roadmap.md")
	if !strings.Contains(out, abs) {
		t.Fatalf("fuzzy mention should resolve to %q: %q", abs, out)
	}
	// ambiguous bare word stays untouched
	write(t, dir, "plans/roadmap.md")
	search.InvalidateFileIndex(dir)
	if got := expandMentions("see @roadmap"); got != "see @roadmap" {
		t.Fatalf("ambiguous should be unchanged: %q", got)
	}
	// a partial path is never fuzzy-resolved
	if got := expandMentions("see @docs/road"); got != "see @docs/road" {
		t.Fatalf("partial path should be unchanged: %q", got)
	}
}

// The completion path must work from a package test binary too: the mention
// root is the model's working dir, not os.Getwd of the test process.
func TestCompletionUsesRootNotTestCwd(t *testing.T) {
	dir := chdir(t)
	write(t, dir, "docs/roadmap.md")
	// the real cwd (package dir) also has files; they must not leak in
	_, cs := completions("fix @roadmap", nil, nil, nil, nil, nil)
	if len(cs) != 1 || cs[0].Text != "@docs/roadmap.md" {
		t.Fatalf("rooted completion: %v", texts(cs))
	}
}

func TestExpandMentions(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "main.go")
	os.WriteFile(f, []byte("x"), 0o644)

	// absolute path with a range, trailing punctuation stripped
	out := expandMentions("look at @" + f + "#10-40, please")
	if !strings.Contains(out, f+" (lines 10-40)") || !strings.Contains(out, "not inlined") {
		t.Fatalf("absolute+range: %q", out)
	}
	// single-line range
	out = expandMentions("check @" + f + "#7")
	if !strings.Contains(out, "(lines 7)") {
		t.Fatalf("single line: %q", out)
	}
	// relative path resolves against cwd
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)
	out = expandMentions("see @main.go here")
	if !strings.Contains(out, f) {
		t.Fatalf("relative: %q", out)
	}
	// ~ expansion
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.WriteFile(filepath.Join(home, "notes.md"), []byte("x"), 0o644)
	out = expandMentions("read @~/notes.md")
	if !strings.Contains(out, filepath.Join(home, "notes.md")) {
		t.Fatalf("tilde: %q", out)
	}
	// nonexistent paths and bare @ are left alone
	for _, in := range []string{"email me @ 5pm", "ping @nonexistent-file-xyz", "no mentions at all"} {
		if got := expandMentions(in); got != in {
			t.Fatalf("should be unchanged: %q -> %q", in, got)
		}
	}
}

func TestAtMentionCompletion(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "alpha.txt"), nil, 0o644)
	_, cs := completions("fix @"+dir+"/al", nil, nil, nil, nil, nil)
	if len(cs) != 1 || cs[0].Text != "@"+filepath.Join(dir, "alpha.txt") {
		t.Fatalf("@ completion: %v", texts(cs))
	}
}

func TestExpandSkills(t *testing.T) {
	sk := []skills.Skill{{Name: "go-style", Description: "d", Path: "/x/go-style/SKILL.md"}}
	out := expandSkills("apply $go-style, thanks", sk)
	if !strings.Contains(out, "go-style (/x/go-style/SKILL.md)") || !strings.Contains(out, "follow its instructions") {
		t.Fatalf("skill note: %q", out)
	}
	for _, in := range []string{"costs $5 now", "run $unknown-skill", "no tokens"} {
		if got := expandSkills(in, sk); got != in {
			t.Fatalf("should be unchanged: %q -> %q", in, got)
		}
	}
}

func TestSkillCompletion(t *testing.T) {
	sk := []cand{{"$go-style", "style rules"}, {"$go-testing", "test rules"}, {"$other", ""}}
	_, cs := completions("apply $go-", nil, nil, nil, sk, nil)
	if len(cs) != 2 || cs[0].Text != "$go-style" && cs[1].Text != "$go-testing" {
		t.Fatalf("$ completion: %v", texts(cs))
	}
}

func TestPrepareTurnReloadsSkillsEveryTurn(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)
	t.Setenv("HOME", t.TempDir()) // isolate ~/.ghg/skills

	os.MkdirAll(filepath.Join(dir, ".agents/skills/demo"), 0o755)
	os.WriteFile(filepath.Join(dir, ".agents/skills/demo/SKILL.md"),
		[]byte("---\nname: demo\ndescription: live demo skill\n---\n"), 0o644)

	m := &model{sysPrompt: "BASE"}
	out, _ := m.prepareTurn("use $demo now")
	if m.skillsLoaded != 1 || m.skillsCache[0].Name != "demo" {
		t.Fatalf("skills were not refreshed: %+v", m.skillsCache)
	}
	if !strings.Contains(out, "invoked skill(s): demo") {
		t.Fatalf("expansion: %q", out)
	}

	// a skill added AFTER startup appears on the next turn — no restart
	os.MkdirAll(filepath.Join(dir, ".agents/skills/fresh"), 0o755)
	os.WriteFile(filepath.Join(dir, ".agents/skills/fresh/SKILL.md"),
		[]byte("---\nname: fresh\ndescription: added mid-session\n---\n"), 0o644)
	m.prepareTurn("hello")
	if m.skillsLoaded != 2 || m.skillsCache[1].Name != "fresh" {
		t.Fatalf("new skill not picked up: %+v", m.skillsCache)
	}
}

func TestImageParts(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(img, []byte("\x89PNG\r\n\x1a\nfake"), 0o644); err != nil {
		t.Fatal(err)
	}

	parts, note := imageParts("what is in @" + img + " ?")
	if len(parts) != 1 {
		t.Fatalf("expected 1 image part, got %d", len(parts))
	}
	p := parts[0]
	if p.Type != "image_url" || p.ImageURL == nil {
		t.Fatalf("part: %+v", p)
	}
	if !strings.HasPrefix(p.ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("data url: %q", p.ImageURL.URL)
	}
	if !strings.Contains(note, "attached image(s):") || !strings.Contains(note, img) {
		t.Fatalf("note: %q", note)
	}

	// text files are not inlined as images
	if parts, note := imageParts("see @foo.go"); parts != nil || note != "" {
		t.Fatalf("text tag should not produce image parts: %v %q", parts, note)
	}
}

func TestMessageMultimodalRoundTrip(t *testing.T) {
	parts := []models.ContentPart{models.ImagePart("png", []byte("fake-image-bytes"))}
	m := models.Message{Role: "user", Content: "describe", Parts: parts}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// wire form: content is an array beginning with the text part
	if !strings.Contains(string(data), `"type":"image_url"`) {
		t.Fatalf("marshaled: %s", data)
	}

	var back models.Message
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.TextContent() != "describe" {
		t.Fatalf("text content: %q", back.TextContent())
	}
	if len(back.Parts) != 1 || back.Parts[0].ImageURL == nil {
		t.Fatalf("parts: %+v", back.Parts)
	}
}

// supportsVision gates whether @image tags inline base64. A provider-advertised
// input_modalities entry wins over config; config's vision flag is the default.
func TestSupportsVisionGate(t *testing.T) {
	newModel := func(visionCfg bool, catalog *config.Catalog) *model {
		m := &model{
			modelName: "some-model",
			provName:  "inference",
			cfg:       &config.Config{Models: map[string]config.Model{"some-model": {Vision: visionCfg}}},
			catalogs:  map[string]config.Catalog{},
		}
		if catalog != nil {
			m.catalogs["inference"] = *catalog
		}
		return m
	}

	// config default off → no vision
	if newModel(false, nil).supportsVision() {
		t.Error("config vision=false should not enable vision")
	}
	// config on → vision
	if !newModel(true, nil).supportsVision() {
		t.Error("config vision=true should enable vision")
	}
	// provider advertises image → wins over config=false
	withImg := &config.Catalog{Models: []config.ModelInfoLite{{ID: "some-model", InputModalities: []string{"text", "image"}}}}
	if !newModel(false, withImg).supportsVision() {
		t.Error("provider-advertised image modality should override config vision=false")
	}
	// provider advertises text-only → wins over config=true
	textOnly := &config.Catalog{Models: []config.ModelInfoLite{{ID: "some-model", InputModalities: []string{"text"}}}}
	if newModel(true, textOnly).supportsVision() {
		t.Error("provider-advertised text-only should override config vision=true")
	}
	// provider entry without modalities → falls back to config
	noModal := &config.Catalog{Models: []config.ModelInfoLite{{ID: "some-model"}}}
	if !newModel(true, noModal).supportsVision() {
		t.Error("no advertised modalities should fall back to config vision=true")
	}
}

// prepareTurn leaves @image as a pointer note (no parts) for text-only models,
// and inlines it for vision models.
func TestPrepareTurnVisionGate(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(img, []byte("\x89PNG\r\n\x1a\nfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := func(vision bool) *model {
		return &model{
			modelName: "m",
			provName:  "p",
			cfg:       &config.Config{Models: map[string]config.Model{"m": {Vision: vision}}},
			catalogs:  map[string]config.Catalog{},
		}
	}

	_, parts := build(false).prepareTurn("look @" + img)
	if len(parts) != 0 {
		t.Errorf("text-only model should get no image parts, got %d", len(parts))
	}
	txt, parts := build(true).prepareTurn("look @" + img)
	if len(parts) != 1 {
		t.Fatalf("vision model should get 1 image part, got %d", len(parts))
	}
	if !strings.Contains(txt, "attached image(s):") {
		t.Errorf("vision note missing: %q", txt)
	}
}
