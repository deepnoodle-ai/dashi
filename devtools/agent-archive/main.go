// Command agent-archive copies Claude Code and Codex session files into a
// local archive, so they outlive the apps' own cleanup. See
// design/agent-archive.md in the Dashi repository.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

func main() {
	syscall.Umask(0o077) // archive files are owner-only
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options are the directories a command works on. Tests point them at
// temporary directories so they never touch the real ones.
type options struct {
	ArchiveDir string
	ClaudeDir  string
	CodexDir   string
}

// command is one subcommand.
type command struct {
	Summary string
	Run     func(args []string, stdout, stderr io.Writer) error
}

var commands = map[string]command{
	"copy": {
		Summary: "copy new and changed originals and update the database",
		Run: func(args []string, stdout, stderr io.Writer) error {
			fs := newFlagSet("copy", stderr)
			o := addFlags(fs, true)
			if err := parseFlags(fs, args, o); err != nil {
				return err
			}
			sum, err := runCopy(*o)
			if err != nil {
				return err
			}
			printCopySummary(stdout, sum)
			return nil
		},
	},
	"rebuild": {
		Summary: "recreate the database from the copies and the copy log",
		Run: func(args []string, stdout, stderr io.Writer) error {
			fs := newFlagSet("rebuild", stderr)
			o := addFlags(fs, false)
			if err := parseFlags(fs, args, o); err != nil {
				return err
			}
			sum, err := runRebuild(o.ArchiveDir)
			if err != nil {
				return err
			}
			printRebuildSummary(stdout, o.ArchiveDir, sum)
			return nil
		},
	},
	"status": {
		Summary: "show the last copy run, counts, warnings, and failed files",
		Run: func(args []string, stdout, stderr io.Writer) error {
			fs := newFlagSet("status", stderr)
			o := addFlags(fs, false)
			if err := parseFlags(fs, args, o); err != nil {
				return err
			}
			return runStatus(o.ArchiveDir, stdout)
		},
	},
	"verify": {
		Summary: "re-hash every copy against the database; -snapshot also checks a snapshot",
		Run: func(args []string, stdout, stderr io.Writer) error {
			fs := newFlagSet("verify", stderr)
			o := addFlags(fs, false)
			snapshot := fs.String("snapshot", "", "snapshot `directory` to check, holding claude/ and codex/")
			if err := parseFlags(fs, args, o); err != nil {
				return err
			}
			if *snapshot != "" {
				p, err := expandHome(*snapshot)
				if err != nil {
					return err
				}
				*snapshot = p
			}
			res, err := runVerify(o.ArchiveDir, *snapshot)
			if err != nil {
				return err
			}
			printVerifyResult(stdout, o.ArchiveDir, res)
			if n := res.problems(); n > 0 {
				return fmt.Errorf("%d problems found", n)
			}
			return nil
		},
	},
	"schedule": {
		Summary: "install or remove the daily LaunchAgent that runs copy (macOS)",
		Run: func(args []string, stdout, stderr io.Writer) error {
			if len(args) == 0 || (args[0] != "install" && args[0] != "remove") {
				fmt.Fprintln(stderr, "usage: agent-archive schedule install [flags] | agent-archive schedule remove")
				return errUsage
			}
			fs := newFlagSet("schedule "+args[0], stderr)
			o := &options{}
			if args[0] == "install" {
				o = addFlags(fs, true)
			}
			if err := parseFlags(fs, args[1:], o); err != nil {
				return err
			}
			s, err := newScheduler()
			if err != nil {
				return err
			}
			if args[0] == "install" {
				return s.install(*o, stdout)
			}
			return s.remove(stdout)
		},
	},
}

// newScheduler returns the scheduler the schedule command uses; tests
// replace it.
var newScheduler = defaultScheduler

// errUsage reports bad arguments; its message has already been printed.
var errUsage = errors.New("usage")

// run is main without the process: it returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" || args[0] == "help" {
		printUsage(stdout)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "agent-archive: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
	if err := cmd.Run(args[1:], stdout, stderr); err != nil {
		if errors.Is(err, errUsage) {
			return 2
		}
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "agent-archive %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: agent-archive <command> [flags]")
	fmt.Fprintln(w, "\ncommands:")
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-9s %s\n", name, commands[name].Summary)
	}
	fmt.Fprintln(w, "\nRun agent-archive <command> -h for a command's flags.")
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("agent-archive "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// addFlags adds -archive, and with originals also -claude-dir and
// -codex-dir. Paths may start with ~/.
func addFlags(fs *flag.FlagSet, originals bool) *options {
	o := &options{}
	fs.StringVar(&o.ArchiveDir, "archive", "~/agent-archive", "archive directory")
	if originals {
		fs.StringVar(&o.ClaudeDir, "claude-dir", "~/.claude", "Claude Code directory")
		fs.StringVar(&o.CodexDir, "codex-dir", "~/.codex", "Codex directory")
	}
	return o
}

// parseFlags parses args and makes the paths in o absolute, expanding ~.
func parseFlags(fs *flag.FlagSet, args []string, o *options) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		return errUsage
	}
	for _, p := range []*string{&o.ArchiveDir, &o.ClaudeDir, &o.CodexDir} {
		if *p == "" {
			continue
		}
		abs, err := expandHome(*p)
		if err != nil {
			return err
		}
		*p = abs
	}
	return nil
}

// expandHome makes p absolute, expanding a leading ~ to the home directory.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return filepath.Abs(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
