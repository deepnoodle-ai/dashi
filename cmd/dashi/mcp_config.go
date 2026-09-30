package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	dashimcp "github.com/deepnoodle-ai/dashi/mcp"
	"github.com/deepnoodle-ai/dashi/workspace"
)

const mcpDefaultTimeout = 10 * time.Second

type mcpEntry struct {
	server        dashimcp.Server
	state, reason string
	ignored       []string
}
type mcpSelection struct {
	config   dashimcp.Config
	entries  map[string]mcpEntry
	warnings []string
	timeout  time.Duration
}

// selectMCP captures each file once. The parser and raw preflight inspect the
// same immutable bytes. Nothing here expands variables or opens a connection.
func selectMCP(home, dir, invocation string, explicit []string, read func(string) ([]byte, error), lookup func(string) (string, bool)) mcpSelection {
	out := mcpSelection{entries: map[string]mcpEntry{}, timeout: mcpDefaultTimeout}
	if raw, ok := lookup("MCP_TIMEOUT"); ok {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 || n > math.MaxInt64/int64(time.Millisecond) {
			out.warnings = append(out.warnings, "MCP_TIMEOUT must be positive milliseconds; using 10s.")
		} else {
			out.timeout = time.Duration(n) * time.Millisecond
		}
	}
	type source struct {
		path              string
		project, explicit bool
	}
	sources := []source{{path: filepath.Join(home, ".dashi", "settings.json")}, {path: filepath.Join(dir, ".mcp.json"), project: true}}
	for _, path := range explicit {
		if !filepath.IsAbs(path) {
			path = filepath.Join(invocation, path)
		}
		sources = append(sources, source{path: filepath.Clean(path), explicit: true})
	}
	var configs []dashimcp.Config
	all := map[string]map[string]mcpEntry{}
	type captured struct {
		data []byte
		err  error
	}
	capture := map[string]captured{}
	for _, src := range sources {
		item, ok := capture[src.path]
		if !ok {
			item.data, item.err = read(src.path)
			capture[src.path] = item
		}
		data, err := item.data, item.err
		if err != nil {
			if !os.IsNotExist(err) || src.explicit {
				out.warnings = append(out.warnings, fmt.Sprintf("MCP configuration %s is unreadable; ignored.", workspace.PrintableText(src.path)))
			}
			continue
		}
		cfg, err := dashimcp.ParseConfig(data)
		if err != nil {
			out.warnings = append(out.warnings, fmt.Sprintf("MCP configuration %s is malformed; ignored.", workspace.PrintableText(src.path)))
			continue
		}
		var raw struct {
			Servers map[string]map[string]json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(data, &raw) != nil {
			continue
		} // ParseConfig already validated this input.
		metadata := map[string]mcpEntry{}
		for name, s := range cfg.Servers {
			s.Source, s.Untrusted = src.path, src.project
			cfg.Servers[name] = s
			state, reason := preflightMCP(s, raw.Servers[name])
			metadata[name] = mcpEntry{server: s, state: state, reason: reason}
		}
		// Keep metadata by fingerprint: explicitly naming the project file gives
		// the identical raw definition trusted status, so trust is also in the key.
		for name, e := range metadata {
			key := mcpEntryKey(e.server)
			if all[name] == nil {
				all[name] = map[string]mcpEntry{}
			}
			all[name][key] = e
		}
		configs = append(configs, cfg)
	}
	merged := dashimcp.Merge(configs...)
	out.config = dashimcp.Config{Servers: map[string]dashimcp.Server{}}
	for name, s := range merged.Servers {
		e := all[name][mcpEntryKey(s)]
		for _, cfg := range configs {
			if other, ok := cfg.Servers[name]; ok && mcpEntryKey(other) != mcpEntryKey(s) {
				if !slices.Contains(e.ignored, other.Source) {
					e.ignored = append(e.ignored, other.Source)
				}
			}
		}
		e.server = s
		out.entries[name] = e
		if e.state == "" {
			out.config.Servers[name] = s
		}
	}
	return out
}
func mcpEntryKey(s dashimcp.Server) string { return fmt.Sprintf("%s:%t", s.Fingerprint(), s.Untrusted) }
func preflightMCP(s dashimcp.Server, raw map[string]json.RawMessage) (string, string) {
	if s.Disabled {
		return "disabled", "disabled in configuration"
	}
	for _, key := range []string{"oauth", "headersHelper"} {
		if value := strings.TrimSpace(string(raw[key])); value != "" && value != "null" {
			return "unsupported", "OAuth and credential helpers require remote MCP compatibility; use streamable HTTP with caller-supplied headers"
		}
	}
	switch strings.ToLower(s.Type) {
	case "sse":
		return "unsupported", "legacy SSE requires remote MCP compatibility; use streamable HTTP"
	case "stdio":
		if s.Command == "" {
			return "failed", "stdio entry needs a command"
		}
		return "", ""
	case "":
		if s.Command != "" {
			return "", ""
		}
		if s.URL == "" {
			return "failed", "entry needs a command or URL"
		}
	case "http", "streamable-http":
		if s.URL == "" {
			return "failed", "HTTP entry needs a URL"
		}
	default:
		return "failed", "unknown transport type"
	}
	if err := validateMCPURL(s.URL, len(s.Headers) > 0); err != nil {
		return "failed", err.Error()
	}
	return "", ""
}
func validateMCPURL(raw string, credentials bool) error {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || (scheme != "http" && scheme != "https") {
		return fmt.Errorf("HTTP URL needs a literal http or https scheme and authority")
	}
	// Use the first delimiter without expanding or resolving the authority.
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	host := rest[:end]
	if host == "" || strings.ContainsAny(host, "${}@\\") {
		return fmt.Errorf("HTTP URL needs a literal authority without userinfo or variable substitutions")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return fmt.Errorf("HTTP URL is malformed or contains userinfo or a fragment")
	}
	if credentials && u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		loopback := ip != nil && ((ip.To4() != nil && !strings.Contains(u.Hostname(), ":") && ip.To4()[0] == 127) || u.Hostname() == "::1")
		if !loopback {
			return fmt.Errorf("HTTP headers require HTTPS or literal 127/8 or ::1 loopback")
		}
	}
	return nil
}

var mcpVariable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

func mcpBindingReview(raw string) string {
	tokens := mcpVariable.FindAllStringSubmatch(raw, -1)
	if len(tokens) == 0 {
		return "configured literal value (redacted)"
	}
	var refs []string
	for _, token := range tokens {
		ref := "${" + token[1] + "}"
		if strings.Contains(token[0], ":-") {
			ref = "${" + token[1] + ":-<redacted>}"
		}
		refs = append(refs, ref)
	}
	return strings.Join(refs, " ") + "; literal fragments redacted"
}
func mcpSafeCommand(s dashimcp.Server, raw string) string {
	// A configured secret can also appear in argv. Hide all literal binding
	// fragments and defaults before displaying those arguments.
	for _, bindings := range []map[string]string{s.Env, s.Headers} {
		for _, value := range bindings {
			for _, token := range mcpVariable.FindAllStringSubmatch(value, -1) {
				if token[2] != "" {
					raw = strings.ReplaceAll(raw, token[2], "<redacted>")
				}
			}
			for _, literal := range strings.FieldsFunc(mcpVariable.ReplaceAllString(value, "\x00"), func(r rune) bool { return r == 0 }) {
				if literal != "" {
					raw = strings.ReplaceAll(raw, literal, "<redacted>")
				}
			}
		}
	}
	return workspace.PrintableText(mcpVariable.ReplaceAllStringFunc(raw, func(token string) string {
		sub := mcpVariable.FindStringSubmatch(token)
		if strings.Contains(token, ":-") {
			return "${" + sub[1] + ":-<redacted>}"
		}
		return token
	}))
}
