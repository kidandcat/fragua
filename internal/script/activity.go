package script

import (
	"strconv"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// ClientHeader tags who sent a POST /script. The browser UI sends "ui" and
// logs its own lines, so the host publishes activity only for other clients
// (an MCP proxy sends "mcp"; curl and scripts default to "http").
const ClientHeader = "X-Fragua-Client"

// ClientSource normalises a ClientHeader value to a short feed tag.
func ClientSource(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	for _, r := range v {
		if b.Len() >= 24 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "http"
	}
	return b.String()
}

// activityMax caps the script and result text one activity event carries:
// the feed is for a human watching, not a transcript of a 10k-line import.
const activityMax = 4096

// PublishActivity tells every observer (the browser UI over /events) that a
// client other than the UI ran src, with its results. Message is the script,
// Detail the formatted results, Level error when any line failed.
func PublishActivity(p *core.Project, source, src string, rs []Result) {
	if p == nil || strings.TrimSpace(src) == "" {
		return
	}
	if source == "" {
		source = "http"
	}
	level := core.ActivitySuccess
	for _, r := range rs {
		if !r.OK {
			level = core.ActivityError
			break
		}
	}
	p.Events().Publish(core.Event{
		Kind:    core.EventActivity,
		Level:   level,
		Source:  source,
		Message: clip(strings.TrimSpace(src)),
		Detail:  clip(strings.TrimRight(FormatResults(rs), "\n")),
	})
}

func clip(s string) string {
	if len(s) <= activityMax {
		return s
	}
	cut := activityMax
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… (" + strconv.Itoa(strings.Count(s[cut:], "\n")+1) + " more lines)"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
