package mcp

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseConfigReadsStdioAndHTTPEntries(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{
		"theme": "dark",
		"mcpServers": {
			"github": {"command": "gh-mcp", "args": ["serve"], "env": {"A": "1"}},
			"docs": {"type": "http", "url": "https://x.test/mcp",
				"headers": {"Authorization": "Bearer ${TOKEN}"}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	gh := cfg.Servers["github"]
	if gh.Command != "gh-mcp" || !slices.Equal(gh.Args, []string{"serve"}) || gh.Env["A"] != "1" {
		t.Fatalf("github entry = %+v", gh)
	}
	docs := cfg.Servers["docs"]
	if docs.URL != "https://x.test/mcp" || docs.Headers["Authorization"] != "Bearer ${TOKEN}" {
		t.Fatalf("docs entry = %+v", docs)
	}
	if gh.Untrusted || docs.Untrusted {
		t.Fatal("ParseConfig marked a server untrusted")
	}
}

func TestParseConfigRejectsInvalidJSON(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"mcpServers": `)); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
}

func TestLoadProjectFileMarksServersUntrusted(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"a":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	proj, err := LoadProjectFile(path)
	if err != nil {
		t.Fatal(err)
	}
	user, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !proj.Servers["a"].Untrusted || proj.Servers["a"].Source != path {
		t.Fatalf("project entry = %+v", proj.Servers["a"])
	}
	if user.Servers["a"].Untrusted {
		t.Fatal("LoadFile marked a server untrusted")
	}
}

func TestLoadFileReportsMissingFile(t *testing.T) {
	_, err := LoadFile(filepath.Join(t.TempDir(), "none.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want a not-exist error", err)
	}
}

func TestExpandVariables(t *testing.T) {
	t.Setenv("MCP_SET", "value")
	t.Setenv("MCP_EMPTY", "")
	tests := []struct {
		name    string
		in      string
		want    string
		missing []string
	}{
		{name: "set", in: "a-${MCP_SET}-b", want: "a-value-b"},
		{name: "unset", in: "${MCP_NOPE}", want: "", missing: []string{"MCP_NOPE"}},
		{name: "default used", in: "${MCP_NOPE:-x y}", want: "x y"},
		{name: "default for empty", in: "${MCP_EMPTY:-d}", want: "d"},
		{name: "default not used", in: "${MCP_SET:-d}", want: "value"},
		{name: "empty but set", in: "[${MCP_EMPTY}]", want: "[]"},
		{name: "no variable", in: "plain $x", want: "plain $x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, warnings := Server{Command: tt.in}.expanded()
			if s.Command != tt.want {
				t.Fatalf("got %q, want %q", s.Command, tt.want)
			}
			if len(warnings) != len(tt.missing) {
				t.Fatalf("warnings = %v, want %v", warnings, tt.missing)
			}
			for _, name := range tt.missing {
				if !contains(warnings, name) {
					t.Fatalf("warnings = %v, want %s", warnings, name)
				}
			}
		})
	}
}

func TestExpandReachesArgsEnvURLAndHeaders(t *testing.T) {
	t.Setenv("MCP_V", "v")
	s, _ := Server{
		Args:    []string{"${MCP_V}"},
		Env:     map[string]string{"K": "${MCP_V}"},
		URL:     "https://${MCP_V}.test",
		Headers: map[string]string{"H": "${MCP_V}"},
	}.expanded()
	if s.Args[0] != "v" || s.Env["K"] != "v" || s.URL != "https://v.test" || s.Headers["H"] != "v" {
		t.Fatalf("expanded = %+v", s)
	}
}

func TestMergeLaterConfigWins(t *testing.T) {
	a := Config{Servers: map[string]Server{"s": {Command: "one"}, "only": {Command: "x"}}}
	b := Config{Servers: map[string]Server{"s": {Command: "two"}}}
	got := Merge(a, b)
	if got.Servers["s"].Command != "two" || got.Servers["only"].Command != "x" {
		t.Fatalf("merged = %+v", got.Servers)
	}
}

func TestMergeUntrustedNeverReplacesTrusted(t *testing.T) {
	user := Config{Servers: map[string]Server{"s": {Command: "mine"}}}
	proj := Config{Servers: map[string]Server{
		"s": {Command: "theirs", Untrusted: true, Source: "/repo/.mcp.json"},
	}}
	got := Merge(user, proj).Servers["s"]
	if got.Command != "mine" || got.Untrusted {
		t.Fatalf("project entry replaced the user entry: %+v", got)
	}
	if !contains(got.Warnings, "/repo/.mcp.json") {
		t.Fatalf("warnings = %v, want the ignored source", got.Warnings)
	}
	if len(user.Servers["s"].Warnings) != 0 {
		t.Fatal("Merge changed its input")
	}
	// A trusted entry still replaces an untrusted one.
	back := Merge(proj, user).Servers["s"]
	if back.Command != "mine" {
		t.Fatalf("trusted entry lost to an untrusted one: %+v", back)
	}
}

func TestHashTracksTheRawEntry(t *testing.T) {
	base := Server{
		Command: "x",
		Args:    []string{"a"},
		Env:     map[string]string{"K": "${V}"},
		Source:  "/p/.mcp.json",
	}
	same := base
	same.Disabled = true
	same.Warnings = []string{"w"}
	if base.Hash() != same.Hash() {
		t.Fatal("fingerprint changed with Disabled or Warnings")
	}
	changes := map[string]func(*Server){
		"command": func(s *Server) { s.Command = "y" },
		"args":    func(s *Server) { s.Args = []string{"b"} },
		"env":     func(s *Server) { s.Env = map[string]string{"K": "2"} },
		"url":     func(s *Server) { s.URL = "https://x.test" },
		"headers": func(s *Server) { s.Headers = map[string]string{"H": "1"} },
		"source":  func(s *Server) { s.Source = "/q/.mcp.json" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s := base
			change(&s)
			if s.Hash() == base.Hash() {
				t.Fatalf("fingerprint ignored a change of %s", name)
			}
		})
	}
	if strings.Contains(base.Hash(), "$") {
		t.Fatal("fingerprint is not a hash")
	}
}
