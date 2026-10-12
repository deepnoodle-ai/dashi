package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/color"
)

// Options are what the command line asked for.
type Options struct {
	// Run is true for the run command, which sends one request without a
	// screen.
	Run bool
	// Prompt is the run command's prompt. Redirected standard input is
	// added after it.
	Prompt       string
	OutputFormat string
	AttachFiles  []string
	// Yes approves every tool call: run --yes, or
	// --dangerously-skip-permissions on either command.
	Yes bool
	// CompactionThreshold is the token count at which history is compacted.
	// Zero means the compaction default, a share of the model's window.
	CompactionThreshold int
	// Decide is the --decide value, or DASHI_DECIDE's; DecideSet reports
	// that either was given.
	Decide    string
	DecideSet bool

	// Resume reopens a saved session: ResumeID, or the last session of this
	// directory when ResumeID is empty.
	Resume   bool
	ResumeID string
	// SessionID is the --session-id value: the ID of the new session, in
	// lowercase. Empty means a generated ID.
	SessionID string
	// TurnEvents is the --turn-events path: the file that gets one JSON
	// line when a turn starts, ends, or waits for the user.
	TurnEvents string

	Model        string
	Provider     string
	APIEndpoint  string
	Workspace    string
	EnvFiles     []string
	MCPFiles     []string
	SystemPrompt string
	Controls     generationPreferences

	// Plain turns off the full screen and uses line mode.
	Plain bool
	// Inline uses Wonton in the terminal's native scrollback.
	Inline bool

	// Models is true for the models command.
	Models    bool
	Available bool
}

// settingsHelp is the long help of the app: where settings come from.
const settingsHelp = `Settings: a flag wins over its DASHI_* variable (DASHI_PROVIDER, DASHI_MODEL,
DASHI_THINKING_EFFORT, DASHI_SHOW_THINKING, DASHI_TEMPERATURE, DASHI_MAX_TOKENS,
DASHI_COMPACTION_THRESHOLD, DASHI_DECIDE).
Then come a reopened session's own choices, the workspace .dashi/settings.json,
~/.dashi/settings.json, and the defaults. --env-file is read after the command
line, so it supplies credentials but not DASHI_* settings.`

// controlFlags maps each generation control flag to its variable.
var controlFlags = []struct{ flag, env string }{
	{"thinking-effort", "DASHI_THINKING_EFFORT"},
	{"show-thinking", "DASHI_SHOW_THINKING"},
	{"temperature", "DASHI_TEMPERATURE"},
	{"max-tokens", "DASHI_MAX_TOKENS"},
}

// settingsVariables are the DASHI_* variables bound to flags.
var settingsVariables = []string{
	"DASHI_PROVIDER", "DASHI_MODEL", "DASHI_THINKING_EFFORT",
	"DASHI_SHOW_THINKING", "DASHI_TEMPERATURE", "DASHI_MAX_TOKENS",
	"DASHI_COMPACTION_THRESHOLD", "DASHI_DECIDE",
}

// clearEmptySettings unsets each empty settings variable, so it sets
// nothing. Wonton reads a set variable as a value: an empty
// DASHI_SHOW_THINKING would turn thinking summaries off and hide a saved
// choice.
func clearEmptySettings() {
	for _, name := range settingsVariables {
		if value, ok := os.LookupEnv(name); ok && value == "" {
			os.Unsetenv(name)
		}
	}
}

// newApp returns the dashi command line. Each command's handler reads its
// flags and arguments into Options and passes them to start, which does
// the work.
func newApp(stdout, stderr io.Writer, start func(Options) error) *cli.App {
	clearEmptySettings()
	app := cli.New("dashi").
		Description("A terminal coding agent").
		Version(buildVersion()).
		Long(settingsHelp).
		Examples(
			cli.NewExample("Start a session in this directory", "dashi"),
			cli.NewExample("Reopen the last session of this directory", "dashi -c"),
			cli.NewExample("Send one request and print the reply", `dashi run "explain main.go"`),
			cli.NewExample("List the models you have credentials for", "dashi models --available"),
		).
		SetStdout(stdout).
		SetStderr(stderr).
		SetColorEnabled(colorFor(stdout)).
		GlobalFlags(
			cli.String("workspace").ValidateWith(needsValue).
				Help("work in this existing directory instead of the current one"),
			cli.Strings("env-file").
				Help("load variables, such as API keys, from this file; repeatable"),
		)

	app.Main().
		UsageSummary("Start an interactive session").
		Flags(append(agentFlags(),
			cli.Bool("plain").Help("use line mode instead of the full screen"),
			cli.Bool("inline").Help("keep the conversation in the terminal's scrollback"),
			cli.String("session-id").ValidateWith(sessionIDSyntax).
				Help("start a new session with this UUID as its ID"),
			cli.String("turn-events").ValidateWith(needsValue).
				Help("append a JSON line to this file when a turn starts, ends, or waits for you"),
		)...).
		Validate(func(ctx *cli.Context) error {
			if ctx.Bool("plain") && ctx.Bool("inline") {
				return errors.New("--plain and --inline cannot be combined")
			}
			if ctx.IsSet("session-id") && (ctx.Bool("continue") || ctx.IsSet("resume")) {
				return errors.New("--session-id starts a new session; to reopen one, use --resume ID alone")
			}
			return validateAgent(ctx)
		}).
		Run(func(ctx *cli.Context) error {
			o := agentOptions(ctx)
			o.Plain = ctx.Bool("plain")
			o.Inline = ctx.Bool("inline")
			o.SessionID = strings.ToLower(ctx.String("session-id"))
			o.TurnEvents = ctx.String("turn-events")
			return start(o)
		})

	app.Command("run").
		Description("Send one request and print the reply").
		Long("The request is the prompt, then any redirected standard input. With\n" +
			"--continue or --resume and no request, it prints the saved reply.").
		AddArg(&cli.Arg{Name: "prompt", Description: "the request; a /command runs that command"}).
		Flags(append(agentFlags(),
			cli.String("output-format", "o").Default("text").Enum("text", "json", "jsonl").
				Help("how to print the reply"),
			cli.Strings("attach").Help("attach this file to the request; repeatable"),
			cli.Bool("yes", "y").Help("approve every tool call"),
		)...).
		Validate(func(ctx *cli.Context) error {
			if err := validateAgent(ctx); err != nil {
				return err
			}
			return needsValues(ctx, "attach")
		}).
		Run(func(ctx *cli.Context) error {
			o := agentOptions(ctx)
			o.Run = true
			o.Prompt = ctx.Arg(0)
			o.OutputFormat = ctx.String("output-format")
			o.AttachFiles = ctx.Strings("attach")
			o.Yes = o.Yes || ctx.Bool("yes")
			return start(o)
		})

	app.Command("models").
		Description("List destinations and their exact model IDs").
		Flags(cli.Bool("available").Help("list only destinations with configured credentials")).
		Validate(func(ctx *cli.Context) error { return needsValues(ctx, "env-file") }).
		Run(func(ctx *cli.Context) error {
			return start(Options{
				Models:    true,
				Available: ctx.Bool("available"),
				Workspace: ctx.String("workspace"),
				EnvFiles:  ctx.Strings("env-file"),
			})
		})

	app.AddCompletionCommand()
	return app
}

// agentFlags are the flags of the commands that run the agent: the
// interactive session and run.
func agentFlags() []cli.Flag {
	return []cli.Flag{
		cli.Bool("continue", "c").Help("reopen the last session of this directory"),
		cli.String("resume", "r").ValidateWith(needsValue).Help("reopen the session with this ID"),
		cli.String("provider").Env("DASHI_PROVIDER").ValidateWith(needsValue).
			Help("use this configured destination"),
		cli.String("model").Env("DASHI_MODEL").ValidateWith(needsValue).
			Help("use this exact registered model ID"),
		cli.String("api-endpoint").ValidateWith(needsValue).
			Help("send requests to this URL; needs --provider naming a custom destination"),
		cli.Strings("mcp-config").Help("add a trusted MCP configuration file; repeatable"),
		cli.String("system-prompt").ValidateWith(needsValue).
			Help("replace the built-in coding guidance with this text"),
		cli.String("thinking-effort").Env("DASHI_THINKING_EFFORT").ValidateWith(controlSyntax("--thinking-effort")).
			Help("reasoning effort, such as high, or omit to send none"),
		cli.Bool("show-thinking").Env("DASHI_SHOW_THINKING").
			Help("request and show thinking summaries; --show-thinking=false turns them off"),
		cli.String("temperature").Env("DASHI_TEMPERATURE").ValidateWith(controlSyntax("--temperature")).
			Help("sampling temperature, or omit to send none"),
		cli.String("max-tokens").Env("DASHI_MAX_TOKENS").ValidateWith(controlSyntax("--max-tokens")).
			Help("output token limit, or default"),
		cli.String("compaction-threshold").Env("DASHI_COMPACTION_THRESHOLD").ValidateWith(compactionThresholdSyntax).
			Help("compact history once a model call uses this many tokens (default: 20% of the model's window)"),
		cli.Bool("dangerously-skip-permissions").
			Help("approve every tool call; use only in an externally sandboxed environment"),
		cli.String("decide").Env("DASHI_DECIDE").ValidateWith(decideSyntax).
			Help("turn on decide checks, such as command_risk; off turns them all off"),
	}
}

// validateAgent checks the rules that span the agent flags, and the
// controls that came from DASHI_* variables, which Wonton does not pass to
// a flag's own validator.
func validateAgent(ctx *cli.Context) error {
	if ctx.Bool("continue") && ctx.IsSet("resume") {
		return errors.New("--continue and --resume cannot be combined")
	}
	if _, err := controls(ctx); err != nil {
		return err
	}
	if _, err := compactionThreshold(ctx); err != nil {
		return err
	}
	if ctx.IsSet("decide") {
		if _, err := parseDecideList(ctx.String("decide")); err != nil {
			return fmt.Errorf("invalid --decide or DASHI_DECIDE: %w", err)
		}
	}
	return needsValues(ctx, "env-file", "mcp-config")
}

// agentOptions reads the agent flags. validateAgent has checked them.
func agentOptions(ctx *cli.Context) Options {
	o := Options{
		Resume:       ctx.Bool("continue") || ctx.IsSet("resume"),
		ResumeID:     ctx.String("resume"),
		Model:        ctx.String("model"),
		Provider:     ctx.String("provider"),
		APIEndpoint:  ctx.String("api-endpoint"),
		SystemPrompt: ctx.String("system-prompt"),
		Workspace:    ctx.String("workspace"),
		EnvFiles:     ctx.Strings("env-file"),
		MCPFiles:     ctx.Strings("mcp-config"),
		Yes:          ctx.Bool("dangerously-skip-permissions"),
		Decide:       ctx.String("decide"),
		DecideSet:    ctx.IsSet("decide"),
	}
	o.Controls, _ = controls(ctx)
	o.CompactionThreshold, _ = compactionThreshold(ctx)
	return o
}

// compactionThreshold reads --compaction-threshold. A flag's value has
// passed its validator, so an invalid value here came from
// DASHI_COMPACTION_THRESHOLD. An empty variable sets nothing.
func compactionThreshold(ctx *cli.Context) (int, error) {
	value := ctx.String("compaction-threshold")
	if !ctx.IsSet("compaction-threshold") || value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, errors.New("invalid DASHI_COMPACTION_THRESHOLD: use a whole number of tokens above 0")
	}
	return n, nil
}

// compactionThresholdSyntax validates --compaction-threshold.
func compactionThresholdSyntax(value string) error {
	if n, err := strconv.Atoi(value); err != nil || n <= 0 {
		return errors.New("use a whole number of tokens above 0")
	}
	return nil
}

// controls reads the generation control flags. A flag's value has passed
// its validator, so an invalid value here came from its variable. An empty
// variable sets nothing.
func controls(ctx *cli.Context) (generationPreferences, error) {
	var p generationPreferences
	for _, c := range controlFlags {
		value := ctx.String(c.flag)
		if !ctx.IsSet(c.flag) || value == "" {
			continue
		}
		if err := p.setControl("--"+c.flag, value); err != nil {
			return p, fmt.Errorf("invalid %s: %v", c.env, err)
		}
	}
	return p, nil
}

// decideSyntax validates --decide.
func decideSyntax(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("needs a value: feature names, such as command_risk, or off")
	}
	_, err := parseDecideList(value)
	return err
}

// sessionIDSyntax validates --session-id: a UUID, such as
// 0f8fad5b-d9cb-469f-a165-70867728950e. Letters may be either case; the
// session uses the lowercase form.
func sessionIDSyntax(value string) error {
	if !sessionstore.ValidChosenID(strings.ToLower(value)) {
		return errors.New("use a UUID, such as 0f8fad5b-d9cb-469f-a165-70867728950e")
	}
	return nil
}

// needsValue rejects a blank flag value.
func needsValue(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("needs a value")
	}
	return nil
}

// needsValues rejects a blank value of each named repeatable flag.
func needsValues(ctx *cli.Context, names ...string) error {
	for _, name := range names {
		for _, v := range ctx.Strings(name) {
			if err := needsValue(v); err != nil {
				return fmt.Errorf("invalid value for --%s: %v", name, err)
			}
		}
	}
	return nil
}

// controlSyntax returns a validator for one generation control flag.
func controlSyntax(flag string) func(string) error {
	return func(value string) error {
		var p generationPreferences
		return p.setControl(flag, value)
	}
}

// setControl parses one generation control in its flag syntax, which the
// matching DASHI_* variable and slash command share. flag is the flag with
// its dashes, such as "--temperature". An error says what the value should
// be.
func (p *generationPreferences) setControl(flag, value string) error {
	var syntax error
	switch flag {
	case "--thinking-effort":
		syntax = errors.New("use a level, such as high, or omit")
		if value == "omit" {
			value = ""
		}
		p.Effort = preferenceField[string]{Set: true, Value: value}
	case "--show-thinking":
		syntax = errors.New("use true or false")
		v, err := strconv.ParseBool(value)
		if err != nil {
			return syntax
		}
		p.ShowThinking = preferenceField[bool]{Set: true, Value: v}
	case "--temperature":
		syntax = errors.New("use a number of 0 or more, or omit")
		field := preferenceField[float64]{Set: true, Null: value == "omit"}
		if !field.Null {
			var err error
			field.Value, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return syntax
			}
		}
		p.Temperature = field
	case "--max-tokens":
		syntax = errors.New("use a whole number of 0 or more, or default")
		n := 0
		if value != "default" {
			var err error
			n, err = strconv.Atoi(value)
			if err != nil {
				return syntax
			}
		}
		p.MaxTokens = preferenceField[int]{Set: true, Value: n}
	default:
		return fmt.Errorf("unknown flag %s", flag)
	}
	if p.validate() != nil {
		return syntax
	}
	return nil
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
