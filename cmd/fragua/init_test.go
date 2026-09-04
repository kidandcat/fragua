package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/agent"
)

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func TestInitWritesEveryOnboardingFile(t *testing.T) {
	dir := t.TempDir()
	if err := runInit([]string{dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, rel := range []string{
		"AGENTS.md", "CLAUDE.md", ".mcp.json",
		".claude/skills/fragua/SKILL.md", ".cursor/rules/fragua.mdc",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	if got := read(t, dir, "CLAUDE.md"); !strings.Contains(got, "AGENTS.md") {
		t.Errorf("CLAUDE.md should point at AGENTS.md, got %q", got)
	}
	if got := read(t, dir, "AGENTS.md"); !strings.Contains(got, agentSection) {
		t.Error("AGENTS.md has no ## Fragua section")
	}
	// The skill needs frontmatter or Claude Code will not load it.
	skill := read(t, dir, ".claude/skills/fragua/SKILL.md")
	if !strings.HasPrefix(skill, "---\n") || !strings.Contains(skill, "\nname: fragua\n") ||
		!strings.Contains(skill, "\ndescription: ") {
		t.Errorf("skill frontmatter is wrong: %.80q", skill)
	}
}

func TestInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := runInit([]string{dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	before := read(t, dir, "AGENTS.md")
	if err := runInit([]string{dir}); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if after := read(t, dir, "AGENTS.md"); after != before {
		t.Error("a second init rewrote AGENTS.md")
	}
}

func TestInitKeepsLocalEditsWithoutForce(t *testing.T) {
	dir := t.TempDir()
	if err := runInit([]string{dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	mine := "See AGENTS.md\n\nplus my own notes\n"
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{dir}); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if got := read(t, dir, "CLAUDE.md"); got != mine {
		t.Error("init clobbered a locally modified CLAUDE.md")
	}
	if err := runInit([]string{dir, "--force"}); err != nil {
		t.Fatalf("forced init: %v", err)
	}
	if got := read(t, dir, "CLAUDE.md"); got != claudeMD {
		t.Errorf("--force did not restore CLAUDE.md, got %q", got)
	}
}

func TestInitAppendsToExistingAgentsMD(t *testing.T) {
	dir := t.TempDir()
	existing := "# My project\n\nHouse rules.\n\n## Testing\n\nRun make test.\n"
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	got := read(t, dir, "AGENTS.md")
	if !strings.HasPrefix(got, existing) {
		t.Error("init clobbered the existing AGENTS.md")
	}
	if !strings.Contains(got, agentSection) {
		t.Error("no ## Fragua section appended")
	}
	if strings.Count(got, "\n"+agentSection+"\n") != 1 {
		t.Error("expected exactly one ## Fragua heading")
	}
}

// A "# comment" inside a fenced block must not be read as a heading, or the
// section boundary lands in the middle of the recipe.
func TestInitReplacesOnlyItsOwnSection(t *testing.T) {
	dir := t.TempDir()
	if err := runInit([]string{dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	path := filepath.Join(dir, "AGENTS.md")
	body := read(t, dir, "AGENTS.md") + "\n## After Fragua\n\nKeep me.\n"
	body = strings.Replace(body, "### Launch", "### Launch EDITED", 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(read(t, dir, "AGENTS.md"), "### Launch EDITED") {
		t.Error("init overwrote an edited section without --force")
	}
	if err := runInit([]string{dir, "--force"}); err != nil {
		t.Fatalf("forced init: %v", err)
	}
	got := read(t, dir, "AGENTS.md")
	if strings.Contains(got, "EDITED") {
		t.Error("--force did not refresh the Fragua section")
	}
	if !strings.Contains(got, "## After Fragua") || !strings.Contains(got, "Keep me.") {
		t.Error("--force ate the section following ## Fragua")
	}
	if !strings.Contains(got, agent.AGENTSMD) {
		t.Error("refreshed section is not the embedded document")
	}
}

func TestInitMergesMCPJSON(t *testing.T) {
	dir := t.TempDir()
	existing := `{"mcpServers":{"other":{"command":"othersrv"}},"extra":1}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
		Extra int `json:"extra"`
	}
	if err := json.Unmarshal([]byte(read(t, dir, ".mcp.json")), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := doc.MCPServers["other"]; !ok {
		t.Error("init dropped an unrelated MCP server")
	}
	if doc.Extra != 1 {
		t.Error("init dropped unrelated top-level keys")
	}
	f, ok := doc.MCPServers["fragua"]
	if !ok {
		t.Fatal("no fragua server entry")
	}
	if f.Command != "fragua" || len(f.Args) != 1 || f.Args[0] != "mcp" {
		t.Errorf("bad fragua entry: %+v", f)
	}
}

func TestInitRejectsUnknownFlags(t *testing.T) {
	if err := runInit([]string{t.TempDir(), "--nope"}); err == nil {
		t.Error("expected an error for an unknown flag")
	}
}
