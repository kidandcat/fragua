package main

import (
	"strings"
	"testing"
)

func TestParseHostFlags(t *testing.T) {
	cases := []struct {
		cmd     string
		args    []string
		connect bool
		want    hostFlags
	}{
		{"run", nil, false, hostFlags{}},
		{"run", []string{"b.fragua"}, false, hostFlags{file: "b.fragua"}},
		{"run", []string{"b.fragua", "--control", "7900"}, false, hostFlags{file: "b.fragua", addr: "127.0.0.1:7900"}},
		{"run", []string{"--addr=0.0.0.0:7000", "b.fragua"}, false, hostFlags{file: "b.fragua", addr: "0.0.0.0:7000"}},
		{"mcp", []string{"--connect", "7878"}, true, hostFlags{connect: "127.0.0.1:7878"}},
		{"mcp", []string{"-connect=127.0.0.1:7900"}, true, hostFlags{connect: "127.0.0.1:7900"}},
		{"mcp", []string{"x.fragua", "--control", "7901"}, true, hostFlags{file: "x.fragua", addr: "127.0.0.1:7901"}},
	}
	for _, c := range cases {
		got, err := parseHostFlags(c.cmd, c.args, c.connect)
		if err != nil || got != c.want {
			t.Fatalf("%s %v = %+v, %v; want %+v", c.cmd, c.args, got, err, c.want)
		}
	}
}

func TestParseHostFlagsErrors(t *testing.T) {
	cases := []struct {
		cmd     string
		args    []string
		connect bool
		want    string
	}{
		{"run", []string{"--connect", "7878"}, false, "always hosts"},
		{"run", []string{"--control"}, false, "needs a value"},
		{"run", []string{"--control", "nope"}, false, "bad port"},
		{"run", []string{"--port", "7878"}, false, "unknown flag"},
		{"run", []string{"a.fragua", "b.fragua"}, false, "unexpected argument"},
		{"mcp", []string{"--connect", "7878", "--control", "7900"}, true, "cannot be combined"},
		{"mcp", []string{"--connect", "7878", "a.fragua"}, true, "host's own project"},
	}
	for _, c := range cases {
		_, err := parseHostFlags(c.cmd, c.args, c.connect)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s %v: err = %v, want %q", c.cmd, c.args, err, c.want)
		}
	}
}

// --connect must fail fast when nothing listens, not host its own project.
func TestMCPConnectNeedsRunningHost(t *testing.T) {
	_, _, err := mcpBackend(hostFlags{connect: "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "no fragua host answers") {
		t.Fatalf("err = %v", err)
	}
}
