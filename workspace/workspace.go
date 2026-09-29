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
	"slices"

	"github.com/deepnoodle-ai/dashi/tool"
)

// Tool names. The Policy trusts the read-only names in this list, so a host
// must not bind another tool under one of them.
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

// Option changes how Tools builds the tool set. The zero value of every
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

// Tools returns the coding tools for dir. dir must be an existing directory.
// The tools resolve relative paths against dir and refuse paths outside it.
// Register the result as Runner.Tools.
func Tools(dir string, opts ...Option) (tool.List, error) {
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}
	s, err := newScope(dir)
	if err != nil {
		return nil, err
	}
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
			return nil, err
		}
		if !slices.Contains(cfg.without, t.Definition.Name) {
			list = append(list, t)
		}
	}
	return list, nil
}

// readOnlyTools are the names that the Policy runs without a prompt.
var readOnlyTools = []string{
	ToolReadFile,
	ToolListDirectory,
	ToolGlob,
	ToolGrep,
	ToolAskUser,
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
