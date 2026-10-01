// Package mcp connects a Dashi application to MCP servers and offers their
// tools to an agent as a tool.Toolset.
//
// It reads .mcp.json style files, starts stdio and streamable HTTP servers,
// and gives each remote tool a stable model-facing name. A server from a
// project file never starts until the host approves it. A server that fails
// to start becomes a status, not an error, so the agent still runs.
//
// The package is its own Go module because the MCP SDK brings an HTTP and
// OAuth stack that the core module does not need. It never sets a read-only
// hint on a tool: an MCP server is not trusted to say that a call is safe.
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Transport types for Server.Type.
const (
	// TypeStdio runs a program and talks to it on stdin and stdout.
	TypeStdio = "stdio"
	// TypeHTTP connects to a streamable HTTP endpoint.
	TypeHTTP = "http"
)

// Config is the merged set of servers. Keys are server names.
type Config struct {
	Servers map[string]Server
}

// Server is one configured MCP server, as written in the file.
//
// Text fields hold the raw file values. Open expands ${VAR} and
// ${VAR:-default} when it starts the server, so an approval prompt shows the
// variable names, and a changed variable value does not change the hash.
type Server struct {
	// Type is "stdio" or "http". Empty means stdio when Command is set and
	// http when URL is set.
	Type string

	// Command is the program a stdio server runs.
	Command string
	Args    []string

	// Env is added to the parent environment of a stdio server.
	Env map[string]string

	// URL is the endpoint of an http server. Headers are sent on each request.
	URL     string
	Headers map[string]string

	// Disabled keeps a listed server from starting or prompting.
	Disabled bool

	// Source is the file that defined the server. The loaders set it.
	Source string

	// Untrusted marks a server that Open starts only when Options.Approve
	// says yes. LoadProjectFile sets it.
	Untrusted bool

	// Warnings lists problems in the entry, such as a merge that dropped a
	// project entry. Open copies them to the server's status.
	Warnings []string
}

// Hash returns a stable hash of the raw entry: type, command, arguments,
// environment, URL, headers, and the file path. A host keys a remembered
// approval on it, so a repository cannot change a command after approval. It
// ignores Disabled and Warnings.
func (s Server) Hash() string {
	raw, _ := json.Marshal(struct {
		Type    string            `json:"type"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Source  string            `json:"source"`
	}{s.Type, s.Command, s.Args, s.Env, s.URL, s.Headers, s.Source})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

// transport returns the effective transport type, or an error when the entry
// cannot start.
func (s Server) transport() (string, error) {
	switch typ := strings.ToLower(s.Type); {
	case typ == TypeStdio || typ == "" && s.Command != "":
		if s.Command == "" {
			return "", fmt.Errorf("stdio server needs a command")
		}
		return TypeStdio, nil
	case typ == TypeHTTP || typ == "streamable-http" || typ == "" && s.URL != "":
		if s.URL == "" {
			return "", fmt.Errorf("http server needs a url")
		}
		return TypeHTTP, nil
	case typ == "":
		return "", fmt.Errorf("server needs a command or a url")
	case typ == "sse":
		return "", fmt.Errorf("type %q is not supported; use http", s.Type)
	default:
		return "", fmt.Errorf("unknown server type %q", s.Type)
	}
}

// ParseConfig reads the "mcpServers" key of JSON data and ignores every other
// key, so it also reads a settings file that holds more. The servers are
// trusted; see LoadProjectFile. An entry that cannot start still parses: Open
// reports it as a failed status.
func ParseConfig(data []byte) (Config, error) {
	var file struct {
		MCPServers map[string]struct {
			Type     string            `json:"type"`
			Command  string            `json:"command"`
			Args     []string          `json:"args"`
			Env      map[string]string `json:"env"`
			URL      string            `json:"url"`
			Headers  map[string]string `json:"headers"`
			Disabled bool              `json:"disabled"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return Config{}, fmt.Errorf("mcp: parse config: %w", err)
	}
	cfg := Config{Servers: map[string]Server{}}
	for name, e := range file.MCPServers {
		cfg.Servers[name] = Server{
			Type:     e.Type,
			Command:  e.Command,
			Args:     e.Args,
			Env:      e.Env,
			URL:      e.URL,
			Headers:  e.Headers,
			Disabled: e.Disabled,
		}
	}
	return cfg, nil
}

// LoadFile reads a trusted file, such as a user settings file or a file the
// user named on the command line. A missing file returns an error that
// matches fs.ErrNotExist. Invalid JSON returns an error; a host should warn
// and continue without that file.
func LoadFile(path string) (Config, error) {
	return load(path, false)
}

// LoadProjectFile reads a file that arrives with a repository, such as
// .mcp.json. Every server is Untrusted: Open starts it only after
// Options.Approve says yes. Errors are as for LoadFile.
func LoadProjectFile(path string) (Config, error) {
	return load(path, true)
}

func load(path string, untrusted bool) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("mcp: %w", err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	source, err := filepath.Abs(path)
	if err != nil {
		source = path
	}
	for name, s := range cfg.Servers {
		s.Source = source
		s.Untrusted = untrusted
		cfg.Servers[name] = s
	}
	return cfg, nil
}

// Merge combines configs. A later config wins for a name, with one exception:
// an untrusted entry never replaces a trusted one. The trusted entry stays and
// gets a warning, so a repository cannot take over a server name that the
// user set up.
func Merge(cfgs ...Config) Config {
	out := Config{Servers: map[string]Server{}}
	for _, cfg := range cfgs {
		for name, s := range cfg.Servers {
			old, ok := out.Servers[name]
			if ok && s.Untrusted && !old.Untrusted {
				old.Warnings = append(slices.Clone(old.Warnings), fmt.Sprintf(
					"ignored server %q from %s: a trusted entry has the same name",
					name,
					s.Source,
				))
				out.Servers[name] = old
				continue
			}
			out.Servers[name] = s
		}
	}
	return out
}

var variable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expand replaces ${VAR} and ${VAR:-default}. An unset or empty variable
// with a default gives the default; one without gives empty text and its name
// goes to missing.
func expand(s string, missing map[string]bool) string {
	return variable.ReplaceAllStringFunc(s, func(m string) string {
		sub := variable.FindStringSubmatch(m)
		value, ok := os.LookupEnv(sub[1])
		if strings.Contains(m, ":-") && value == "" {
			return sub[2]
		}
		if !ok {
			missing[sub[1]] = true
		}
		return value
	})
}

// expanded returns s with variables expanded, and warnings for unset ones.
func (s Server) expanded() (Server, []string) {
	missing := map[string]bool{}
	out := s
	out.Command = expand(s.Command, missing)
	out.URL = expand(s.URL, missing)
	out.Args = nil
	for _, a := range s.Args {
		out.Args = append(out.Args, expand(a, missing))
	}
	out.Env = expandMap(s.Env, missing)
	out.Headers = expandMap(s.Headers, missing)
	var warnings []string
	for _, name := range slices.Sorted(maps.Keys(missing)) {
		warnings = append(warnings, fmt.Sprintf("variable %s is not set", name))
	}
	return out, warnings
}

func expandMap(m map[string]string, missing map[string]bool) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = expand(v, missing)
	}
	return out
}
