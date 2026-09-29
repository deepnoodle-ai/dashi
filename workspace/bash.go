package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/tool"
)

const (
	defaultShellTimeout = 2 * time.Minute
	defaultShellMax     = 10 * time.Minute
	progressInterval    = 250 * time.Millisecond
	progressLines       = 20

	// The bash tool keeps the first and the last part of the output, so a
	// command that prints without end cannot fill memory.
	shellHeadMax = 512 << 10
	shellTailMax = 512 << 10

	// drainWait bounds how long the tool waits for output after the shell
	// exits. A background process can hold the output pipes open.
	drainWait = 500 * time.Millisecond
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
		// Compare in milliseconds first. The multiplication can overflow.
		const maxMS = int(math.MaxInt64 / int64(time.Millisecond))
		switch {
		case cfg.MaxTimeout > 0 && in.Timeout > int(cfg.MaxTimeout/time.Millisecond):
			timeout = cfg.MaxTimeout
			note = fmt.Sprintf("\n[The timeout was reduced to the maximum, %s.]", timeout)
		case in.Timeout > maxMS:
			timeout = time.Duration(math.MaxInt64)
		default:
			timeout = time.Duration(in.Timeout) * time.Millisecond
		}
	}
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	out := newShellOutput(ctx, shellHeadMax, shellTailMax)
	cmd := exec.CommandContext(runCtx, cfg.Path, "-c", in.Command)
	cmd.Dir = s.root
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = drainWait
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
	case errors.Is(err, exec.ErrWaitDelay):
		// The shell exited with success, but a background process still
		// holds the output pipes. The command itself worked.
		return tool.Succeeded(text + "\n[The command succeeded. A background " +
			"process it started is still running and its output is not " +
			"captured. Redirect its output to a file, such as " +
			"\"cmd > log.txt 2>&1 &\", to keep it.]" + note), nil
	case err != nil:
		return fail(fmt.Sprintf("cannot run the shell %s: %v", cfg.Path, err))
	}
	return tool.Succeeded(text + note), nil
}

// shellOutput collects the merged output of a command and reports the last
// lines as progress, at most four times a second. It keeps the first headMax
// bytes and the last tailMax bytes, and counts what it drops.
type shellOutput struct {
	ctx     context.Context
	headMax int
	tailMax int

	mu      sync.Mutex
	head    []byte
	tail    []byte // may grow to twice tailMax before it is cut
	dropped int
	last    time.Time
}

func newShellOutput(ctx context.Context, headMax, tailMax int) *shellOutput {
	return &shellOutput{ctx: ctx, headMax: headMax, tailMax: tailMax}
}

func (o *shellOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	if room := o.headMax - len(o.head); room > 0 {
		take := min(room, len(p))
		o.head = append(o.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		o.tail = append(o.tail, p...)
		if len(o.tail) > 2*o.tailMax {
			cut := len(o.tail) - o.tailMax
			o.dropped += cut
			o.tail = append(o.tail[:0], o.tail[cut:]...)
		}
	}
	if now := time.Now(); now.Sub(o.last) >= progressInterval {
		o.last = now
		o.report()
	}
	return n, nil
}

// String returns the output. When bytes were dropped, a marker between the
// head and the tail says how many.
func (o *shellOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	head, tail, dropped := o.head, o.tail, o.dropped
	if len(tail) > o.tailMax {
		cut := len(tail) - o.tailMax
		dropped += cut
		tail = tail[cut:]
	}
	if dropped == 0 {
		return strings.TrimRight(string(head)+string(tail), "\n")
	}
	// Do not start or end a part in the middle of a character.
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
		dropped++
	}
	for i := len(head) - 1; i >= 0 && i >= len(head)-utf8.UTFMax; i-- {
		if utf8.RuneStart(head[i]) {
			if !utf8.FullRune(head[i:]) {
				dropped += len(head) - i
				head = head[:i]
			}
			break
		}
	}
	return strings.TrimRight(fmt.Sprintf(
		"%s\n[... %d bytes of output dropped from the middle. The first %d "+
			"bytes and the last %d bytes are shown. Write the output to a "+
			"file, or filter it with grep, head or tail, to see the part "+
			"that you need ...]\n%s",
		head, dropped, len(head), len(tail), tail,
	), "\n")
}

// report must run with mu held.
func (o *shellOutput) report() {
	recent := o.tail[max(len(o.tail)-8192, 0):]
	if len(recent) < 8192 {
		// The tail is short, so the last lines may reach into the head.
		recent = append(append([]byte{}, o.head[max(len(o.head)-(8192-len(recent)), 0):]...), recent...)
	}
	lines := strings.Split(strings.TrimRight(string(recent), "\n"), "\n")
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
