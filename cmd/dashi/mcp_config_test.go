package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMCPSourceSelectionCapturesOnceAndProtectsTrustedEntries(t *testing.T) {
	home, dir, invocation := t.TempDir(), t.TempDir(), t.TempDir()
	paths := map[string]string{
		filepath.Join(home, ".dashi", "settings.json"): `{"mcpServers":{"docs":{"command":"user"},"unavailable":{"type":"sse","url":"https://example.com"}}}`,
		filepath.Join(dir, ".mcp.json"):                `{"mcpServers":{"docs":{"command":"project"},"project":{"command":"project"}}}`,
		filepath.Join(invocation, "extra.json"):        `{"mcpServers":{"unavailable":{"command":"explicit"}}}`,
	}
	counts := map[string]int{}
	read := func(path string) ([]byte, error) {
		counts[path]++
		if value, ok := paths[path]; ok {
			return []byte(value), nil
		}
		return nil, os.ErrNotExist
	}
	cfg := selectMCP(home, dir, invocation, []string{"extra.json"}, read, func(string) (string, bool) { return "", false })
	if cfg.config.Servers["docs"].Command != "user" || !cfg.config.Servers["project"].Untrusted || cfg.config.Servers["unavailable"].Command != "explicit" {
		t.Fatalf("selection=%+v", cfg)
	}
	if len(cfg.entries["docs"].ignored) != 1 {
		t.Fatal("missing ignored source")
	}
	for _, count := range counts {
		if count != 1 {
			t.Fatalf("reads=%v", counts)
		}
	}
	clear(counts)
	cfg = selectMCP(home, dir, invocation, []string{filepath.Join(dir, ".mcp.json")}, read, func(string) (string, bool) { return "", false })
	if cfg.config.Servers["docs"].Untrusted || cfg.config.Servers["docs"].Command != "project" || counts[filepath.Join(dir, ".mcp.json")] != 1 {
		t.Fatalf("selection=%+v reads=%v", cfg, counts)
	}
}
func TestMCPMalformedSourceAndUnsupportedEntries(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	raw := `{"mcpServers":{"sse":{"type":"sse","url":"https://example.com"},"oauth":{"url":"https://example.com","oauth":{}},"helper":{"url":"https://example.com","headersHelper":"SECRET"},"disabled":{"type":"sse","disabled":true},"unknown":{"type":"secret-new"},"valid":{"command":"true"}}}`
	cfg := selectMCP(home, dir, dir, []string{"missing.json"}, func(path string) ([]byte, error) {
		if strings.HasSuffix(path, "settings.json") {
			return []byte(`{"mcpServers":`), nil
		}
		if strings.HasSuffix(path, ".mcp.json") {
			return []byte(raw), nil
		}
		return nil, os.ErrNotExist
	}, func(string) (string, bool) { return "", false })
	if len(cfg.warnings) != 2 || len(cfg.config.Servers) != 1 {
		t.Fatalf("selection=%+v", cfg)
	}
	for _, name := range []string{"sse", "oauth", "helper"} {
		if cfg.entries[name].state != "unsupported" {
			t.Fatalf("entry=%+v", cfg.entries[name])
		}
	}
	if cfg.entries["disabled"].state != "disabled" || cfg.entries["unknown"].state != "failed" {
		t.Fatal("wrong states")
	}
	if strings.Contains((&mcpRuntime{selection: cfg}).Inventory(), "SECRET") {
		t.Fatal("secret diagnostic")
	}
}
func TestMCPURLPreflight(t *testing.T) {
	for _, tt := range []struct {
		raw              string
		headers, allowed bool
	}{
		{"https://example.com/mcp", true, true}, {"http://example.com/mcp", false, true},
		{"http://127.2.3.4:8090/mcp", true, true}, {"http://[::1]:8090/mcp", true, true},
		{"http://localhost/mcp", true, false}, {"http://127.1/mcp", true, false}, {"http://[::ffff:127.0.0.1]/mcp", true, false},
		{"https://user:SECRET@example.com/mcp", false, false}, {"https://${MCP_AUTHORITY}/mcp", false, false}, {"https://${MCP_AUTHORITY}/mcp", true, false}, {"${SCHEME}://example.com/mcp", false, false},
		{"https://example.com/${PATH}", true, true}, {"https:///mcp", false, false}, {"http://example.com:bad/mcp", false, false},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			err := validateMCPURL(tt.raw, tt.headers)
			if (err == nil) != tt.allowed {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
func TestMCPTimeoutAndConfigFlag(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "no", "9223372036854775807"} {
		cfg := selectMCP("/missing", "/missing", "/missing", nil, func(string) ([]byte, error) { return nil, os.ErrNotExist }, func(string) (string, bool) { return value, true })
		if cfg.timeout != 10*time.Second || len(cfg.warnings) != 1 {
			t.Fatalf("value=%s cfg=%+v", value, cfg)
		}
	}
	cfg := selectMCP("/missing", "/missing", "/missing", nil, func(string) ([]byte, error) { return nil, os.ErrNotExist }, func(string) (string, bool) { return "250", true })
	if cfg.timeout != 250*time.Millisecond {
		t.Fatal(cfg.timeout)
	}
	o, err := parseArgs([]string{"--mcp-config", "one.json", "--mcp-config=two.json"})
	if err != nil || len(o.MCPFiles) != 2 {
		t.Fatalf("o=%+v err=%v", o, err)
	}
	if _, err := parseArgs([]string{"--mcp-config="}); err == nil {
		t.Fatal("empty config accepted")
	}
}
func TestMCPStartupReviewRedactsValuesDefaultsAndArgDuplicates(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	raw := `{"mcpServers":{"docs":{"command":"server","args":["--token=SECRET","${ARG:-DEFAULT}"],"env":{"TOKEN":"SECRET","REF":"prefix-${KEY:-DEFAULT}-suffix"},"headers":{"Authorization":"Bearer ${AUTH}"}}}}`
	selected := selectMCP(home, dir, dir, nil, func(path string) ([]byte, error) {
		if strings.HasSuffix(path, ".mcp.json") {
			return []byte(raw), nil
		}
		return nil, os.ErrNotExist
	}, func(string) (string, bool) { return "", false })
	m := newMCPRuntime(dir, selected)
	review := m.startupReview("docs", selected.entries["docs"].server)
	if strings.Contains(review, "SECRET") || strings.Contains(review, "DEFAULT") || strings.Contains(review, "prefix-") || !strings.Contains(review, "${KEY:-<redacted>}") || !strings.Contains(review, "${AUTH}") {
		t.Fatal(review)
	}
}

func TestMCPReviewUsesEffectiveTransportInsteadOfUnusedFields(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	raw := `{"mcpServers":{"docs":{"type":"http","url":"https://docs.example.com/mcp","command":"unused-command","args":["unused-argument"]}}}`
	selected := selectMCP(home, dir, dir, nil, func(path string) ([]byte, error) {
		if strings.HasSuffix(path, ".mcp.json") {
			return []byte(raw), nil
		}
		return nil, os.ErrNotExist
	}, func(string) (string, bool) { return "", false })
	m := newMCPRuntime(dir, selected)
	review := m.startupReview("docs", selected.entries["docs"].server)
	if !strings.Contains(review, "Destination: https://docs.example.com") || strings.Contains(review, "Command: unused-command") || strings.Contains(review, "Transport: stdio") {
		t.Fatal(review)
	}
	if strings.Contains(m.Inventory(), "Directory:") {
		t.Fatal("HTTP inventory claimed a stdio directory")
	}
}
