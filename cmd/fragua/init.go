package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mentasystems/fragua/agent"
)

// agentSection is the heading fragua owns inside a shared AGENTS.md.
const agentSection = "## Fragua"

// initFlags are the parsed `fragua init` arguments.
type initFlags struct {
	dir   string
	force bool
}

// runInit writes the agent onboarding files into a project directory.
// Idempotent: an unmodified file is rewritten, a modified one is left alone
// unless --force. An existing AGENTS.md gets a "## Fragua" section appended.
func runInit(args []string) error {
	var f initFlags
	for _, a := range args {
		switch {
		case a == "--force" || a == "-f":
			f.force = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q — usage: fragua init [dir] [--force]", a)
		case f.dir == "":
			f.dir = a
		default:
			return fmt.Errorf("unexpected argument %q — usage: fragua init [dir] [--force]", a)
		}
	}
	if f.dir == "" {
		f.dir = "."
	}
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		return err
	}

	var notes []string
	note := func(format string, a ...any) { notes = append(notes, fmt.Sprintf(format, a...)) }

	if err := writeAgentsMD(f, agent.AGENTSMD, note); err != nil {
		return err
	}
	if err := writeFile(f, "CLAUDE.md", claudeMD, note); err != nil {
		return err
	}
	if err := writeFile(f, filepath.Join(".claude", "skills", "fragua", "SKILL.md"), agent.SkillMD, note); err != nil {
		return err
	}
	if err := writeFile(f, filepath.Join(".cursor", "rules", "fragua.mdc"), agent.CursorMDC, note); err != nil {
		return err
	}
	if err := writeMCPJSON(f, note); err != nil {
		return err
	}

	for _, n := range notes {
		fmt.Println(n)
	}
	fmt.Printf("\nFragua onboarding written to %s\n", f.dir)
	fmt.Println("Claude Code: run `claude` here — the skill and the MCP server are picked up automatically.")
	fmt.Println("Cursor: the rule in .cursor/rules/fragua.mdc applies to .fragua files.")
	return nil
}

// claudeMD points Claude Code at the shared guide rather than duplicating it.
const claudeMD = "See AGENTS.md\n"

// writeFile writes content unless the file exists with different content and
// was not written by us, in which case it is preserved without --force.
func writeFile(f initFlags, rel, content string, note func(string, ...any)) error {
	path := filepath.Join(f.dir, rel)
	old, err := os.ReadFile(path)
	switch {
	case err == nil && string(old) == content:
		note("  unchanged  %s", rel)
		return nil
	case err == nil && !f.force:
		note("  kept       %s (modified locally; --force to overwrite)", rel)
		return nil
	case err == nil:
		note("  overwrote  %s", rel)
	default:
		note("  wrote      %s", rel)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// writeAgentsMD creates AGENTS.md, or replaces just the "## Fragua" section of
// an existing one, leaving the rest of the human's file untouched.
func writeAgentsMD(f initFlags, section string, note func(string, ...any)) error {
	path := filepath.Join(f.dir, "AGENTS.md")
	old, err := os.ReadFile(path)
	if err != nil {
		if err := os.WriteFile(path, []byte("# Agent guide\n\n"+section), 0o644); err != nil {
			return err
		}
		note("  wrote      AGENTS.md")
		return nil
	}
	body := string(old)
	start := sectionStart(body)
	if start < 0 {
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if err := os.WriteFile(path, []byte(body+"\n"+section), 0o644); err != nil {
			return err
		}
		note("  appended   AGENTS.md (## Fragua section)")
		return nil
	}
	end := sectionEnd(body, start)
	if strings.TrimRight(body[start:end], "\n") == strings.TrimRight(section, "\n") {
		note("  unchanged  AGENTS.md")
		return nil
	}
	if !f.force {
		note("  kept       AGENTS.md (## Fragua section edited locally; --force to refresh)")
		return nil
	}
	updated := body[:start] + section + body[end:]
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return err
	}
	note("  refreshed  AGENTS.md (## Fragua section)")
	return nil
}

// sectionStart finds the offset of the "## Fragua" heading, or -1.
func sectionStart(body string) int {
	for _, line := range splitKeepOffsets(body) {
		if !line.fenced && strings.TrimSpace(line.text) == agentSection {
			return line.off
		}
	}
	return -1
}

// sectionEnd finds where the section ends: the next h1/h2, or end of file.
func sectionEnd(body string, start int) int {
	for _, line := range splitKeepOffsets(body) {
		if line.off <= start || line.fenced {
			continue
		}
		t := strings.TrimSpace(line.text)
		if strings.HasPrefix(t, "## ") || strings.HasPrefix(t, "# ") {
			return line.off
		}
	}
	return len(body)
}

type offsetLine struct {
	off    int
	text   string
	fenced bool // inside a ``` block, where a leading # is a comment, not a heading
}

// splitKeepOffsets splits body into lines, remembering offsets and fenced state.
func splitKeepOffsets(body string) []offsetLine {
	var out []offsetLine
	off, inFence := 0, false
	for _, l := range strings.SplitAfter(body, "\n") {
		text := strings.TrimSuffix(l, "\n")
		fence := strings.HasPrefix(strings.TrimSpace(text), "```")
		out = append(out, offsetLine{off: off, text: text, fenced: inFence || fence})
		if fence {
			inFence = !inFence
		}
		off += len(l)
	}
	return out
}

// mcpEntry is the fragua server block written into .mcp.json.
func mcpEntry() map[string]any {
	return map[string]any{
		"command": "fragua",
		"args":    []any{"mcp"},
	}
}

// writeMCPJSON adds the fragua server to .mcp.json, preserving other servers.
func writeMCPJSON(f initFlags, note func(string, ...any)) error {
	path := filepath.Join(f.dir, ".mcp.json")
	doc := map[string]any{}
	existed := false
	if raw, err := os.ReadFile(path); err == nil {
		existed = true
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if cur, ok := servers["fragua"]; ok && !f.force {
		if sameJSON(cur, mcpEntry()) {
			note("  unchanged  .mcp.json")
			return nil
		}
		note("  kept       .mcp.json (fragua entry edited locally; --force to refresh)")
		return nil
	}
	servers["fragua"] = mcpEntry()
	doc["mcpServers"] = servers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return err
	}
	if existed {
		note("  merged     .mcp.json (fragua server)")
	} else {
		note("  wrote      .mcp.json")
	}
	return nil
}

// sameJSON compares two values by their JSON encoding.
func sameJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}
