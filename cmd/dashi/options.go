package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/color"
)

// Options are the parsed command-line flags.
type Options struct {
	// Resume is true for --resume or -c.
	Resume bool
	// ResumeID names the session for --resume ID; empty means the last one.
	ResumeID string
	// Print is the prompt of -p; PrintSet tells an empty prompt from none.
	Print           string
	PrintSet        bool
	OutputFormat    string
	OutputFormatSet bool
	Model           string
	Provider        string
	APIEndpoint     string
	Workspace       string
	EnvFiles        []string
	MCPFiles        []string
	AttachFiles     []string
	SystemPrompt    string
	SystemPromptSet bool
	Models          bool
	Available       bool
	Controls        generationPreferences

	// Yes approves every tool call, for -p.
	Yes bool
	// Plain turns off the full screen and uses line mode.
	Plain bool
	// Inline uses Wonton in the terminal's native scrollback.
	Inline bool
	// Help is true when the command line asked only for help or the
	// version, which parseArgs has already written.
	Help bool
}

// settingsNote ends the generated help.
const settingsNote = `Settings: DASHI_PROVIDER, DASHI_MODEL, matching DASHI_* controls,
~/.dashi/settings.json, and workspace .dashi/settings.json.`

// agentFlags are the flags of the agent, the root command. They are global
// flags so the parser knows which ones take a value: "dashi -p help" sends
// the prompt "help" instead of showing help.
func agentFlags() []cli.Flag {
	return []cli.Flag{
		cli.String("print", "p").Help("run one turn without a screen and print the reply (PROMPT)"),
		cli.String("output-format").Enum("text", "json", "jsonl").Help("run one request with response-only output"),
		cli.String("resume", "c").Help("reopen the last session of this directory, or session ID ([ID])"),
		cli.String("provider").Help("choose a configured destination (ID)"),
		cli.String("model").Help("choose an exact registered model ID (NAME)"),
		cli.String("api-endpoint").Help("override a custom destination endpoint (URL)"),
		cli.Strings("mcp-config").Help("add a trusted MCP configuration (FILE, repeatable)"),
		cli.Strings("attach").Help("select a file for -p (PATH, repeatable, before mentions)"),
		cli.String("workspace").Help("use an existing workspace directory (DIR)"),
		cli.Strings("env-file").Help("load an explicit literal environment file (PATH, repeatable)"),
		cli.String("system-prompt").Help("replace built-in coding guidance (TEXT)"),
		cli.String("thinking-effort").Help("set reasoning effort (LEVEL or omit)"),
		cli.Bool("show-thinking").Help("request and display public summaries (--show-thinking=false turns them off)"),
		cli.String("temperature").Help("set or omit sampling temperature (N or omit)"),
		cli.String("max-tokens").Help("set the output limit (N or default)"),
		cli.Bool("yes").Help("approve every tool call (for -p)"),
		cli.Bool("plain").Help("use line mode instead of the full screen"),
		cli.Bool("inline").Help("use interactive controls in native terminal scrollback"),
	}
}

// modelsFlags are the agent flags that the models command also reads.
var modelsFlags = map[string]bool{"workspace": true, "env-file": true}

// parseArgs reads the command line. Help and the version go to out.
func parseArgs(args []string, out io.Writer) (Options, error) {
	var o Options
	ran := false
	app := cli.New("dashi").
		Description("A terminal coding agent.").
		Version(buildVersion()).
		Long(settingsNote).
		SetStdout(out).
		SetStderr(io.Discard).
		SetColorEnabled(colorFor(out)).
		GlobalFlags(agentFlags()...)
	app.Main().
		UsageSummary("Start a session in this directory").
		Run(func(ctx *cli.Context) error {
			ran = true
			var err error
			o, err = agentOptions(ctx)
			return withHelpHint(err, "dashi")
		})
	models := app.Command("models").
		Description("List destinations and their exact model IDs").
		Flags(cli.Bool("available").Help("list only destinations with configured credentials")).
		Run(func(ctx *cli.Context) error {
			ran = true
			var err error
			o, err = modelsOptions(ctx)
			return withHelpHint(err, "dashi models")
		})
	for _, f := range agentFlags() {
		if !modelsFlags[f.GetName()] {
			models.OmitGlobalFlag(f.GetName())
		}
	}
	err := app.ExecuteArgs(wontonArgs(args))
	if cli.IsHelpRequested(err) || (err == nil && !ran) {
		// Help, or the version, was written.
		return Options{Help: true}, nil
	}
	return o, err
}

// wontonArgs rewrites two forms that Wonton does not parse. A Wonton string
// flag always needs a value, so the forms of --resume without a session ID
// become "--resume=": -c never takes an ID, and --resume takes the next
// argument unless it is a flag. Wonton also has no "-p=PROMPT", so that
// becomes "--print=PROMPT".
func wontonArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		if a == "-c" || (a == "--resume" && (i+1 == len(args) || strings.HasPrefix(args[i+1], "-"))) {
			a = "--resume="
		} else if prompt, ok := strings.CutPrefix(a, "-p="); ok {
			a = "--print=" + prompt
		}
		out = append(out, a)
	}
	return out
}

// agentOptions reads the agent's flags and checks the rules that span
// flags.
func agentOptions(ctx *cli.Context) (Options, error) {
	o := Options{
		Resume:          ctx.IsSet("resume"),
		ResumeID:        ctx.String("resume"),
		Print:           ctx.String("print"),
		PrintSet:        ctx.IsSet("print"),
		OutputFormat:    ctx.String("output-format"),
		OutputFormatSet: ctx.IsSet("output-format"),
		Model:           ctx.String("model"),
		Provider:        ctx.String("provider"),
		APIEndpoint:     ctx.String("api-endpoint"),
		Workspace:       ctx.String("workspace"),
		EnvFiles:        ctx.Strings("env-file"),
		MCPFiles:        ctx.Strings("mcp-config"),
		AttachFiles:     ctx.Strings("attach"),
		SystemPrompt:    ctx.String("system-prompt"),
		SystemPromptSet: ctx.IsSet("system-prompt"),
		Yes:             ctx.Bool("yes"),
		Plain:           ctx.Bool("plain"),
		Inline:          ctx.Bool("inline"),
	}
	if ctx.IsSet("model") && strings.TrimSpace(o.Model) == "" {
		return o, errors.New("--model needs a nonempty exact model ID")
	}
	for _, path := range o.AttachFiles {
		if strings.TrimSpace(path) == "" {
			return o, errors.New("--attach needs a path")
		}
	}
	if err := requireValues(ctx, "provider", "api-endpoint", "workspace", "env-file", "mcp-config"); err != nil {
		return o, err
	}
	if o.SystemPromptSet && strings.TrimSpace(o.SystemPrompt) == "" {
		return o, errors.New("--system-prompt needs nonempty text")
	}
	for _, name := range []string{"thinking-effort", "temperature", "max-tokens"} {
		if !ctx.IsSet(name) {
			continue
		}
		if err := o.Controls.setControl("--"+name, ctx.String(name)); err != nil {
			return o, err
		}
	}
	if ctx.IsSet("show-thinking") {
		o.Controls.ShowThinking = preferenceField[bool]{Set: true, Value: ctx.Bool("show-thinking")}
	}
	if o.OutputFormatSet && (o.Plain || o.Inline) {
		return o, errors.New("--output-format cannot be combined with --plain, --inline, or models")
	}
	if len(o.AttachFiles) > 0 && !o.PrintSet && !o.OutputFormatSet {
		return o, errors.New("--attach requires -p; use /attach or @file interactively")
	}
	if o.Plain && o.Inline {
		return o, errors.New("--plain and --inline cannot be combined")
	}
	return o, nil
}

// modelsOptions reads the models command's flags.
func modelsOptions(ctx *cli.Context) (Options, error) {
	o := Options{
		Models:    true,
		Available: ctx.Bool("available"),
		Workspace: ctx.String("workspace"),
		EnvFiles:  ctx.Strings("env-file"),
	}
	return o, requireValues(ctx, "workspace", "env-file")
}

// requireValues rejects a blank value for each named flag that was set.
func requireValues(ctx *cli.Context, names ...string) error {
	for _, name := range names {
		if !ctx.IsSet(name) {
			continue
		}
		values := ctx.Strings(name)
		if len(values) == 0 {
			values = []string{""}
		}
		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("--%s needs a nonempty value", name)
			}
		}
	}
	return nil
}

// setControl parses one generation control in its flag syntax, which the
// matching DASHI_* variable and slash command share. flag is the flag with
// its dashes, such as "--temperature".
func (p *generationPreferences) setControl(flag, value string) error {
	switch flag {
	case "--thinking-effort":
		if value == "omit" {
			value = ""
		}
		p.Effort = preferenceField[string]{Set: true, Value: value}
	case "--show-thinking":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("invalid --show-thinking")
		}
		p.ShowThinking = preferenceField[bool]{Set: true, Value: v}
	case "--temperature":
		field := preferenceField[float64]{Set: true, Null: value == "omit"}
		if !field.Null {
			var err error
			field.Value, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return errors.New("invalid --temperature")
			}
		}
		p.Temperature = field
	case "--max-tokens":
		n := 0
		if value != "default" {
			var err error
			n, err = strconv.Atoi(value)
			if err != nil {
				return errors.New("invalid --max-tokens")
			}
		}
		p.MaxTokens = preferenceField[int]{Set: true, Value: n}
	default:
		return fmt.Errorf("unknown flag %s", flag)
	}
	return p.validate()
}

// withHelpHint points a command-line error at the command's help, as
// Wonton's own flag errors do.
func withHelpHint(err error, command string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w\n\nRun '%s --help' to see available flags.", err, command)
}

// colorFor reports whether help written to w should use color.
func colorFor(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && color.ShouldColorize(f)
}

// buildVersion is the module version Go recorded in the binary, without
// its leading "v", or empty for a build without one.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return ""
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}
