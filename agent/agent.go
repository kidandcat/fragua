// Package agent holds the onboarding documents `fragua init` drops into a
// project: the shared AGENTS.md section, the Claude Code skill and the Cursor
// rule. These files are the product's front door for AI agents — edit the
// markdown here, not the copies in user projects.
package agent

import _ "embed"

// AGENTSMD is the "## Fragua" section for a project's AGENTS.md.
//
//go:embed AGENTS.md
var AGENTSMD string

// SkillMD is the Claude Code skill (.claude/skills/fragua/SKILL.md).
//
//go:embed SKILL.md
var SkillMD string

// CursorMDC is the Cursor rule (.cursor/rules/fragua.mdc).
//
//go:embed fragua.mdc
var CursorMDC string
