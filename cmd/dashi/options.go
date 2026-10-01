package main

import (
	"fmt"
	"strconv"
	"strings"
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
	Help   bool
}

// parseArgs reads the flags. --resume takes an optional session ID, which the
// standard flag package cannot express.
func parseArgs(args []string) (Options, error) {
	var o Options
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, inline, hasInline := strings.Cut(a, "=")
		value := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch name {
		case "-c":
			o.Resume = true
		case "--resume":
			o.Resume = true
			if hasInline {
				o.ResumeID = inline
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				o.ResumeID = args[i]
			}
		case "-p", "--print":
			v, err := value()
			if err != nil {
				return o, err
			}
			o.Print, o.PrintSet = v, true
		case "--output-format":
			v, err := value()
			if err != nil {
				return o, err
			}
			if v != "text" && v != "json" && v != "jsonl" {
				return o, fmt.Errorf("--output-format needs text, json, or jsonl")
			}
			o.OutputFormat, o.OutputFormatSet = v, true
		case "--attach":
			v, err := value()
			if err != nil {
				return o, err
			}
			if strings.TrimSpace(v) == "" {
				return o, fmt.Errorf("--attach needs a path")
			}
			o.AttachFiles = append(o.AttachFiles, v)
		case "--model":
			v, err := value()
			if err != nil {
				return o, err
			}
			if strings.TrimSpace(v) == "" {
				return o, fmt.Errorf("--model needs a nonempty exact model ID")
			}
			o.Model = v
		case "models":
			if i != 0 {
				return o, fmt.Errorf("models must be the first argument")
			}
			o.Models = true
		case "--available":
			o.Available = true
		case "--provider", "--api-endpoint", "--workspace", "--env-file", "--mcp-config", "--system-prompt", "--thinking-effort", "--temperature", "--max-tokens":
			v, err := value()
			if err != nil {
				return o, err
			}
			if (name == "--provider" || name == "--api-endpoint" || name == "--workspace" || name == "--env-file" || name == "--mcp-config") && strings.TrimSpace(v) == "" {
				return o, fmt.Errorf("%s needs a nonempty value", name)
			}
			switch name {
			case "--provider":
				o.Provider = v
			case "--api-endpoint":
				o.APIEndpoint = v
			case "--workspace":
				o.Workspace = v
			case "--env-file":
				o.EnvFiles = append(o.EnvFiles, v)
			case "--mcp-config":
				o.MCPFiles = append(o.MCPFiles, v)
			case "--system-prompt":
				if strings.TrimSpace(v) == "" {
					return o, fmt.Errorf("--system-prompt needs nonempty text")
				}
				o.SystemPrompt, o.SystemPromptSet = v, true
			case "--thinking-effort":
				if v == "omit" {
					v = ""
				}
				o.Controls.Effort = preferenceField[string]{Set: true, Value: v}
			case "--temperature":
				field := preferenceField[float64]{Set: true, Null: v == "omit"}
				if !field.Null {
					field.Value, err = strconv.ParseFloat(v, 64)
					if err != nil {
						return o, fmt.Errorf("invalid --temperature")
					}
				}
				o.Controls.Temperature = field
			case "--max-tokens":
				n := 0
				if v != "default" {
					n, err = strconv.Atoi(v)
					if err != nil {
						return o, fmt.Errorf("invalid --max-tokens")
					}
				}
				o.Controls.MaxTokens = preferenceField[int]{Set: true, Value: n}
			}
		case "--show-thinking":
			v := true
			if hasInline {
				var err error
				v, err = strconv.ParseBool(inline)
				if err != nil {
					return o, fmt.Errorf("invalid --show-thinking")
				}
			}
			o.Controls.ShowThinking = preferenceField[bool]{Set: true, Value: v}
		case "--yes":
			o.Yes = true
		case "--plain":
			o.Plain = true
		case "--inline":
			o.Inline = true
		case "-h", "--help":
			o.Help = true
		default:
			return o, fmt.Errorf("unknown flag %s", a)
		}
	}
	if o.OutputFormatSet && (o.Plain || o.Inline || o.Models) {
		return o, fmt.Errorf("--output-format cannot be combined with --plain, --inline, or models")
	}
	if len(o.AttachFiles) > 0 && !o.PrintSet && !o.OutputFormatSet {
		return o, fmt.Errorf("--attach requires -p; use /attach or @file interactively")
	}
	if o.Plain && o.Inline {
		return o, fmt.Errorf("--plain and --inline cannot be combined")
	}
	if o.Available && !o.Models {
		return o, fmt.Errorf("--available requires models")
	}
	if err := o.Controls.validate(); err != nil {
		return o, err
	}
	return o, nil
}

const usage = `Usage: dashi [flags]
       dashi models [--available]

A terminal coding agent.

Flags:
  -p, --print PROMPT   run one turn without a screen and print the answer
      --output-format text|json|jsonl  run one request with response-only output
  -c, --resume [ID]    reopen the last session of this directory, or session ID
      --provider ID    choose a configured destination
      --model NAME     choose an exact registered model ID
      --api-endpoint URL  override a custom destination endpoint
      --mcp-config FILE  add a trusted MCP configuration (repeatable)
      --attach PATH     select a file for -p (repeatable, before mentions)
      --workspace DIR  use an existing workspace directory
      --env-file PATH  load an explicit literal environment file
      --system-prompt TEXT  replace built-in coding guidance
      --thinking-effort LEVEL|omit  set reasoning effort
      --show-thinking[=false]  request and display public summaries
      --temperature N|omit  set or omit sampling temperature
      --max-tokens N|default  set the output limit
      --yes            approve every tool call (for -p)
      --plain          use line mode instead of the full screen
      --inline         use interactive controls in native terminal scrollback
  -h, --help           show this help

Settings: DASHI_PROVIDER, DASHI_MODEL, matching DASHI_* controls,
~/.dashi/settings.json, and workspace .dashi/settings.json.
`
