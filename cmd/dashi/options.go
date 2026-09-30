package main

import (
	"fmt"
	"strings"
)

// Options are the parsed command-line flags.
type Options struct {
	// Resume is true for --resume or -c.
	Resume bool
	// ResumeID names the session for --resume ID; empty means the last one.
	ResumeID string
	// Print is the prompt of -p; PrintSet tells an empty prompt from none.
	Print    string
	PrintSet bool
	Model    string
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
		case "--model":
			v, err := value()
			if err != nil {
				return o, err
			}
			o.Model = v
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
	if o.Plain && o.Inline {
		return o, fmt.Errorf("--plain and --inline cannot be combined")
	}
	return o, nil
}

const usage = `Usage: dashi [flags]

A terminal coding agent.

Flags:
  -p, --print PROMPT   run one turn without a screen and print the answer
  -c, --resume [ID]    reopen the last session of this directory, or session ID
      --model NAME     set the model (default gpt-6-luna)
      --yes            approve every tool call (for -p)
      --plain          use line mode instead of the full screen
      --inline         use interactive controls in native terminal scrollback
  -h, --help           show this help

Settings: DASHI_MODEL, ~/.dashi/settings.json, .dashi/settings.json.
`
