package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi/tool"
)

const (
	defaultShellTimeout = 2 * time.Minute
	defaultShellMax     = 10 * time.Minute
	progressInterval    = 250 * time.Millisecond
	progressLines       = 20
)

// ShellConfig sets the limits of the bash tool. The zero value of each field
// selects a default. A negative timeout means no limit.
type ShellConfig struct {
	// DefaultTimeout applies when a call sets no timeout. Default 2 minutes.
	DefaultTimeout time.Duration

	// MaxTimeout caps the timeout that a call may ask for. Default 10
	// minutes.
	MaxTimeout time.Duration

	// Path is the shell program, run as Path -c command. The default is bash
	// when found, and sh otherwise.
	Path string
}

// ShellProgress is the Data of each progress report from the bash tool. The
// host decodes it to show live output.
type ShellProgress struct {
	// Lines holds the last 20 lines of output.
	Lines []string `json:"lines"`
}

type bashArgs struct {
	Command string `json:"command" description:"The shell command to run."`
	Timeout int    `json:"timeout,omitempty" description:"Timeout in milliseconds. Default 120000. Maximum 600000."`
}

func newBash(s *scope, cfg ShellConfig) (tool.Tool, error) {
	if cfg.DefaultTimeout == 0 {
		cfg.DefaultTimeout = defaultShellTimeout
	}
	if cfg.MaxTimeout == 0 {
		cfg.MaxTimeout = defaultShellMax
	}
	if cfg.Path == "" {
		cfg.Path = "sh"
		if p, err := exec.LookPath("bash"); err == nil {
			cfg.Path = p
		}
	}
	t, err := tool.Func(
		ToolBash,
		"Run a shell command in the workspace directory. The command gets "+
			"no input. The result holds the output and the error output, in "+
			"order. A command that exits with a non-zero code fails. The tool "+
			"stops a command that runs past the timeout.",
		func(ctx context.Context, _ tool.Call, in bashArgs) (tool.Outcome, error) {
			return runShell(ctx, s, cfg, in)
		},
	)
	return annotate(t, false), err
}

func runShell(
	ctx context.Context,
	s *scope,
	cfg ShellConfig,
	in bashArgs,
) (tool.Outcome, error) {
	if strings.TrimSpace(in.Command) == "" {
		return fail("command is empty. Give a shell command.")
	}
	timeout, note := cfg.DefaultTimeout, ""
	if in.Timeout > 0 {
		timeout = time.Duration(in.Timeout) * time.Millisecond
		if cfg.MaxTimeout > 0 && timeout > cfg.MaxTimeout {
			timeout = cfg.MaxTimeout
			note = fmt.Sprintf("\n[The timeout was reduced to the maximum, %s.]", timeout)
		}
	}
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	out := &shellOutput{ctx: ctx}
	cmd := exec.CommandContext(runCtx, cfg.Path, "-c", in.Command)
	cmd.Dir = s.root
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	err := cmd.Run()
	text := out.String()
	if text == "" {
		text = "(no output)"
	}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return fail(text + "\n[The command was canceled.]" + note)
	case runCtx.Err() != nil:
		return fail(fmt.Sprintf(
			"%s\n[The command ran past the timeout of %s and was stopped. "+
				"The output above is partial. Set a longer timeout, or run "+
				"a smaller command.]%s",
			text, timeout, note,
		))
	case errors.As(err, &exit):
		return fail(fmt.Sprintf("%s\n[Exit code %d]%s", text, exit.ExitCode(), note))
	case err != nil:
		return fail(fmt.Sprintf("cannot run the shell %s: %v", cfg.Path, err))
	}
	return tool.Succeeded(text + note), nil
}

// shellOutput collects the merged output of a command and reports the last
// lines as progress, at most four times a second.
type shellOutput struct {
	ctx  context.Context
	mu   sync.Mutex
	buf  bytes.Buffer
	last time.Time
}

func (o *shellOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf.Write(p)
	if now := time.Now(); now.Sub(o.last) >= progressInterval {
		o.last = now
		o.report()
	}
	return len(p), nil
}

func (o *shellOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.TrimRight(o.buf.String(), "\n")
}

// report must run with mu held.
func (o *shellOutput) report() {
	tail := o.buf.Bytes()
	tail = tail[max(len(tail)-8192, 0):]
	lines := strings.Split(strings.TrimRight(string(tail), "\n"), "\n")
	lines = lines[max(len(lines)-progressLines, 0):]
	data, err := json.Marshal(ShellProgress{Lines: lines})
	if err != nil {
		return
	}
	tool.Report(o.ctx, tool.Progress{
		Message: lines[len(lines)-1],
		Data:    data,
	})
}
