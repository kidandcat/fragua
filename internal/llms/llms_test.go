package llms

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/script"
)

// docsDir is the checked-in docs/ directory, relative to this package.
const docsDir = "../../docs"

// TestDocsAreCurrent fails when docs/llms*.txt no longer match the binary.
// Fix: scripts/gen-llms.sh
func TestDocsAreCurrent(t *testing.T) {
	for name, want := range map[string]string{
		"llms.txt":      Index(),
		"llms-full.txt": Full(),
	} {
		got, err := os.ReadFile(filepath.Join(docsDir, name))
		if err != nil {
			t.Fatalf("%s: %v — run scripts/gen-llms.sh", name, err)
		}
		if string(got) != want {
			t.Errorf("docs/%s is stale — run scripts/gen-llms.sh", name)
		}
	}
}

func TestIndexFollowsLLMsTxtConventions(t *testing.T) {
	got := Index()
	if !strings.HasPrefix(got, "# fragua\n") {
		t.Error("llms.txt must open with a single H1")
	}
	if !strings.Contains(got, "\n> ") {
		t.Error("llms.txt needs a blockquote summary")
	}
	if !strings.Contains(got, "## Docs") || !strings.Contains(got, "llms-full.txt") {
		t.Error("llms.txt should link to llms-full.txt from a section")
	}
}

func TestFullCarriesTheGuideAndEveryVerb(t *testing.T) {
	got := Full()
	// The agent guide must be present verbatim, not summarised.
	if !strings.Contains(got, "### End-to-end recipe") {
		t.Error("llms-full.txt is missing the agent guide")
	}
	if !strings.Contains(got, "Script verbs (line-oriented, agent-first):") {
		t.Error("llms-full.txt is missing the full script reference")
	}
	for i := range script.Verbs {
		name := script.Verbs[i].Name
		if !strings.Contains(got, "\n## "+name+"\n") {
			t.Errorf("llms-full.txt has no per-verb section for %q", name)
		}
	}
}
