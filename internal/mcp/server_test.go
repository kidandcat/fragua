package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mentasystems/fragua/internal/core"
)

// connect wires a client to a fragua MCP server over the in-memory transport.
func connect(t *testing.T) (*sdk.ClientSession, *core.Project) {
	t.Helper()
	p := core.NewProject("test")
	srv := NewServer(&Local{Project: p, APIAddr: "127.0.0.1:7878"})
	ct, st := sdk.NewInMemoryTransports()

	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, p
}

// call runs a tool and returns its concatenated text content.
func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestListToolsCoversTheAdvertisedSurface(t *testing.T) {
	cs, _ := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
	}
	for _, want := range []string{
		"fragua_script", "fragua_help", "fragua_status", "fragua_state",
		"fragua_screenshot", "fragua_save", "fragua_drc", "fragua_route",
	} {
		if !got[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

func TestScriptMutatesTheLiveProject(t *testing.T) {
	cs, p := connect(t)
	out, isErr := call(t, cs, "fragua_script", map[string]any{"script": "outline 30 20\nstatus"})
	if isErr {
		t.Fatalf("script errored: %s", out)
	}
	if !strings.Contains(out, "ok outline") {
		t.Errorf("no outline result in %q", out)
	}
	p.RLock()
	defer p.RUnlock()
	if p.Board().Outline == nil {
		t.Error("outline verb did not reach the project")
	}
}

func TestScriptReportsFailureAsToolError(t *testing.T) {
	cs, _ := connect(t)
	out, isErr := call(t, cs, "fragua_script", map[string]any{"script": "no-such-verb 1 2"})
	if isErr {
		t.Fatalf("a bad verb should come back as a normal result, got tool error: %s", out)
	}
	if !strings.Contains(out, "error line 1") {
		t.Errorf("expected a per-line error, got %q", out)
	}
}

func TestHelpFullAndPerVerb(t *testing.T) {
	cs, _ := connect(t)
	full, _ := call(t, cs, "fragua_help", nil)
	if !strings.Contains(full, "Script verbs") || !strings.Contains(full, "First 10 minutes") {
		t.Errorf("full help looks wrong: %.120q", full)
	}
	one, _ := call(t, cs, "fragua_help", map[string]any{"verb": "route"})
	if !strings.Contains(one, "route [max_seconds=N]") || !strings.Contains(one, "Examples:") {
		t.Errorf("per-verb help looks wrong: %.200q", one)
	}
	if strings.Contains(one, "outline-poly") {
		t.Error("per-verb help leaked the full reference")
	}
	bad, isErr := call(t, cs, "fragua_help", map[string]any{"verb": "nope"})
	if !isErr || !strings.Contains(bad, "unknown verb") {
		t.Errorf("unknown verb should error, got %q", bad)
	}
}

func TestStateSectionFilter(t *testing.T) {
	cs, _ := connect(t)
	call(t, cs, "fragua_script", map[string]any{"script": "outline 30 20"})

	full, _ := call(t, cs, "fragua_state", nil)
	var pf map[string]any
	if err := json.Unmarshal([]byte(full), &pf); err != nil {
		t.Fatalf("state is not JSON: %v", err)
	}
	for _, k := range []string{"name", "board", "schematic"} {
		if _, ok := pf[k]; !ok {
			t.Errorf("state missing %q", k)
		}
	}

	board, _ := call(t, cs, "fragua_state", map[string]any{"section": "board"})
	var b map[string]any
	if err := json.Unmarshal([]byte(board), &b); err != nil {
		t.Fatalf("section is not JSON: %v", err)
	}
	if _, ok := b["schematic"]; ok {
		t.Error("board section leaked the schematic")
	}

	out, isErr := call(t, cs, "fragua_state", map[string]any{"section": "nope"})
	if !isErr || !strings.Contains(out, "no section") {
		t.Errorf("unknown section should error, got %q", out)
	}
}

func TestStatusScreenshotAndRoute(t *testing.T) {
	cs, _ := connect(t)
	call(t, cs, "fragua_script", map[string]any{"script": "outline 30 20"})

	status, isErr := call(t, cs, "fragua_status", nil)
	if isErr || !strings.Contains(status, "127.0.0.1:7878") {
		t.Errorf("status looks wrong: %q", status)
	}
	shot, isErr := call(t, cs, "fragua_screenshot", nil)
	if isErr || !strings.Contains(shot, "<svg") {
		t.Errorf("screenshot is not SVG: %.120q", shot)
	}
	// Nothing to route, but the verb must accept the typed options.
	route, isErr := call(t, cs, "fragua_route", map[string]any{"max_seconds": 5, "organic": true})
	if isErr {
		t.Fatalf("route errored: %s", route)
	}
	if !strings.Contains(route, "max_seconds=5") || !strings.Contains(route, "organic=true") {
		t.Errorf("route did not build the expected line: %q", route)
	}
}

func TestDRCRunsBothChecks(t *testing.T) {
	cs, _ := connect(t)
	call(t, cs, "fragua_script", map[string]any{"script": "outline 30 20"})
	out, isErr := call(t, cs, "fragua_drc", nil)
	if isErr {
		t.Fatalf("drc errored: %s", out)
	}
	if !strings.Contains(out, "erc") || !strings.Contains(out, "drc") {
		t.Errorf("expected both checks in %q", out)
	}
}

func TestSaveRoundTrips(t *testing.T) {
	cs, _ := connect(t)
	call(t, cs, "fragua_script", map[string]any{"script": "outline 30 20"})

	if out, isErr := call(t, cs, "fragua_save", nil); !isErr {
		t.Errorf("a memory-only session must refuse to save without a path, got %q", out)
	}
	path := t.TempDir() + "/board.fragua"
	out, isErr := call(t, cs, "fragua_save", map[string]any{"path": path})
	if isErr {
		t.Fatalf("save errored: %s", out)
	}
	if _, err := core.LoadFromPath(path); err != nil {
		t.Fatalf("saved project does not load back: %v", err)
	}
}

func TestResources(t *testing.T) {
	cs, _ := connect(t)
	res, err := cs.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}
	got := map[string]bool{}
	for _, r := range res.Resources {
		got[r.URI] = true
	}
	for _, want := range []string{"fragua://help", "fragua://state"} {
		if !got[want] {
			t.Fatalf("missing resource %s", want)
		}
	}
	help, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "fragua://help"})
	if err != nil {
		t.Fatalf("read help: %v", err)
	}
	if !strings.Contains(help.Contents[0].Text, "Script verbs") {
		t.Error("fragua://help is not the reference")
	}
	state, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "fragua://state"})
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !json.Valid([]byte(state.Contents[0].Text)) {
		t.Error("fragua://state is not JSON")
	}
}
