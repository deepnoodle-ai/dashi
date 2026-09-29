// Package workspace provides coding tools for one directory: read, list,
// glob, grep, write, edit, run a shell command, and ask the user. It also
// provides an approval Policy that plugs into Runner.Hooks.
//
// The file tools refuse any path outside the directory, including a path that
// leaves it through a symlink. The shell tool has no such limit, because a
// command can reach any path the user can reach. The approval Policy is the
// guard for the shell.
//
// Every failure that the model can fix, such as a wrong path or a repeated
// edit string, returns as a failed tool outcome. The text names the problem
// and the next step. The package does not bound output size except where a
// tool needs a limit to work. Generic bounding belongs to the host.
package workspace

import (
	"crypto/sha256"
	"encoding/json"
	"slices"
	"sync"

	"github.com/deepnoodle-ai/dashi/tool"
)

// Tool names.
const (
	ToolReadFile      = "read_file"
	ToolListDirectory = "list_directory"
	ToolGlob          = "glob"
	ToolGrep          = "grep"
	ToolWriteFile     = "write_file"
	ToolEditFile      = "edit_file"
	ToolBash          = "bash"
	ToolAskUser       = "ask_user"
)

// Option changes how Workspace.Tools builds the tool set. The zero value of every
// option works.
type Option func(*config)

type config struct {
	ask     AskFunc
	shell   ShellConfig
	without []string
}

// WithAsker adds the ask_user tool. The host renders the question. Without
// this option the tool is absent, because a tool that cannot ask would only
// confuse the model.
func WithAsker(ask AskFunc) Option {
	return func(c *config) {
		c.ask = ask
	}
}

// WithShell sets the shell timeouts and the shell program.
func WithShell(cfg ShellConfig) Option {
	return func(c *config) {
		c.shell = cfg
	}
}

// WithoutTools leaves out the named tools. A name that matches no tool is
// ignored.
func WithoutTools(names ...string) Option {
	return func(c *config) {
		c.without = append(c.without, names...)
	}
}

// Workspace is one directory that the coding tools work in. It owns the
// directory for the tools and for the approval Policy, so the two cannot
// disagree. It also remembers the exact tools that it built, so the Policy
// can tell them from a tool of another package that has the same name.
type Workspace struct {
	scope *scope

	mu    sync.Mutex
	owned map[string]bool // definition fingerprint -> read-only
}

// New returns a Workspace for dir. dir must be an existing directory.
func New(dir string) (*Workspace, error) {
	s, err := newScope(dir)
	if err != nil {
		return nil, err
	}
	return &Workspace{scope: s, owned: map[string]bool{}}, nil
}

// Dir returns the absolute directory of the workspace.
func (w *Workspace) Dir() string {
	return w.scope.root
}

// Tools returns the coding tools. They resolve relative paths against the
// directory and refuse paths outside it. Register the result as
// Runner.Tools. Every call returns new tools; the Policy of this Workspace
// recognizes the tools of all of them.
func (w *Workspace) Tools(opts ...Option) tool.List {
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}
	s := w.scope
	builders := []func() (tool.Tool, error){
		func() (tool.Tool, error) { return newReadFile(s) },
		func() (tool.Tool, error) { return newListDirectory(s) },
		func() (tool.Tool, error) { return newGlob(s) },
		func() (tool.Tool, error) { return newGrep(s) },
		func() (tool.Tool, error) { return newWriteFile(s) },
		func() (tool.Tool, error) { return newEditFile(s) },
		func() (tool.Tool, error) { return newBash(s, cfg.shell) },
	}
	if cfg.ask != nil {
		builders = append(builders, func() (tool.Tool, error) {
			return newAskUser(cfg.ask)
		})
	}
	var list tool.List
	for _, build := range builders {
		t, err := build()
		if err != nil {
			// The definitions are fixed in this package, so a failure is a
			// bug that tests catch.
			panic("workspace: build tool: " + err.Error())
		}
		if slices.Contains(cfg.without, t.Definition.Name) {
			continue
		}
		w.register(t)
		list = append(list, t)
	}
	return list
}

func (w *Workspace) register(t tool.Tool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.owned[fingerprint(t.Definition)] = ptrTrue(t.Definition.Annotations.ReadOnly)
}

// lookup reports whether def is exactly a tool that this Workspace built,
// and whether that tool is read-only.
func (w *Workspace) lookup(def tool.Definition) (owned, readOnly bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	readOnly, owned = w.owned[fingerprint(def)]
	return owned, readOnly
}

// fingerprint identifies a definition by all of its content. A tool from
// another package that copies only the name does not match.
func fingerprint(def tool.Definition) string {
	data, err := json.Marshal(def)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return string(sum[:])
}

// annotate marks t read-only or mutating.
func annotate(t tool.Tool, readOnly bool) tool.Tool {
	t.Definition.Annotations.ReadOnly = &readOnly
	return t
}

// fail returns a failed outcome that the model reads and can act on.
func fail(text string) (tool.Outcome, error) {
	return tool.Failed(text), nil
}
