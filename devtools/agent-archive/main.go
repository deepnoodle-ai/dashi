// Command agent-archive copies Claude Code and Codex session files into a
// local archive, so they outlive the apps' own cleanup. See
// design/agent-archive.md in the Dashi repository.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/deepnoodle-ai/wonton/cli"
)

func main() {
	syscall.Umask(0o077) // archive files are owner-only
	app := newApp()
	if err := app.Execute(); err != nil {
		app.PrintError(err)
		os.Exit(cli.GetExitCode(err))
	}
}

// options are the directories a command works on. Tests point them at
// temporary directories so they never touch the real ones.
type options struct {
	ArchiveDir string
	ClaudeDir  string
	CodexDir   string
}

// The directory flags' defaults. Paths may start with ~/.
const (
	defaultArchiveDir = "~/agent-archive"
	defaultClaudeDir  = "~/.claude"
	defaultCodexDir   = "~/.codex"
)

// The directory flags. Each also reads an environment variable; a flag on
// the command line wins over the variable.
func archiveFlag() cli.Flag {
	return cli.String("archive", "").Default(defaultArchiveDir).Env("AGENT_ARCHIVE_DIR").
		Help("archive directory; env AGENT_ARCHIVE_DIR")
}

func originalsFlags() []cli.Flag {
	return []cli.Flag{
		archiveFlag(),
		cli.String("claude-dir", "").Default(defaultClaudeDir).Env("AGENT_ARCHIVE_CLAUDE_DIR").
			Help("Claude Code directory; env AGENT_ARCHIVE_CLAUDE_DIR"),
		cli.String("codex-dir", "").Default(defaultCodexDir).Env("AGENT_ARCHIVE_CODEX_DIR").
			Help("Codex directory; env AGENT_ARCHIVE_CODEX_DIR"),
	}
}

// newScheduler returns the scheduler the schedule command uses; tests
// replace it.
var newScheduler = defaultScheduler

func newApp() *cli.App {
	app := cli.New("agent-archive").
		Description("Copy Claude Code and Codex session files into a local archive")

	app.Command("copy").
		Description("Copy new and changed originals and update the database").
		Flags(originalsFlags()...).
		Run(func(ctx *cli.Context) error {
			o, err := directories(ctx)
			if err != nil {
				return err
			}
			sum, err := runCopy(o)
			if err != nil {
				return err
			}
			printCopySummary(ctx.Stdout(), sum)
			return nil
		})

	app.Command("rebuild").
		Description("Recreate the database from the copies and the copy log").
		Flags(archiveFlag()).
		Run(func(ctx *cli.Context) error {
			o, err := directories(ctx)
			if err != nil {
				return err
			}
			sum, err := runRebuild(o.ArchiveDir)
			if err != nil {
				return err
			}
			printRebuildSummary(ctx.Stdout(), o.ArchiveDir, sum)
			return nil
		})

	app.Command("parse").
		Description("Parse session files that need it: the sessions and usage tables").
		Long("Parse every copy not yet parsed by this version of agent-archive. copy and "+
			"rebuild also do this; --all parses every copy again.").
		Flags(
			archiveFlag(),
			cli.Bool("all").Help("parse every copy again"),
		).
		Run(func(ctx *cli.Context) error {
			o, err := directories(ctx)
			if err != nil {
				return err
			}
			sum, err := runParse(o.ArchiveDir, ctx.Bool("all"))
			if err != nil {
				return err
			}
			printParseSummary(ctx.Stdout(), sum)
			return nil
		})

	app.Command("convert").
		Description("Print one agent of a session as Dashi messages, one JSON line each").
		Long("Each line holds a message and its ref: the copy and lines it came from, whether it "+
			"was inherited, and the model and usage of an assistant message. Counts of skipped "+
			"lines and changed blocks go to stderr.").
		Flags(
			archiveFlag(),
			cli.String("session", "").Required().Help("session_id of the session"),
			cli.String("agent", "").Help("agent_id of a subagent; empty for the root"),
		).
		Run(func(ctx *cli.Context) error {
			o, err := directories(ctx)
			if err != nil {
				return err
			}
			return runConvert(o.ArchiveDir, ctx.String("session"), ctx.String("agent"), ctx.Stdout(), ctx.Stderr())
		})

	app.Command("report").
		Description("Show list-price spend by model, app, project, or session").
		Long("Price every model call with Dashi's catalog and show spend per day, week, or month. "+
			"A trailing + marks a lower bound: some calls could not be priced. Codex cache writes, and "+
			"Claude Code cache writes not in session files, are high estimates; --json gives the low "+
			"end too. The last line compares Claude Code's own session totals with Dashi's.").
		Flags(
			archiveFlag(),
			cli.String("by", "").Default(byModel).Enum(byModel, byApp, byProject, bySession).
				Help("what each row is: model, app, project, or session"),
			cli.String("per", "").Default(perWeek).Enum(perDay, perWeek, perMonth).
				Help("what each column is: day, week (from Monday), or month"),
			cli.String("since", "").Help("first day included (YYYY-MM-DD); default: eight periods back, counting the current one"),
			cli.String("until", "").Help("last day included (YYYY-MM-DD); default: today"),
			cli.Bool("json").Help("print one JSON object per row and period instead of a table"),
		).
		Run(func(ctx *cli.Context) error {
			o, err := directories(ctx)
			if err != nil {
				return err
			}
			ro, err := newReportOptions(ctx.String("by"), ctx.String("per"), ctx.String("since"),
				ctx.String("until"), time.Now(), time.Local)
			if err != nil {
				return err
			}
			ro.JSON = ctx.Bool("json")
			ro.Home, _ = os.UserHomeDir()
			return runReport(o.ArchiveDir, ro, ctx.Stdout(), ctx.Stderr())
		})

	app.Command("status").
		Description("Show the last copy run, counts, warnings, and failed files").
		Flags(archiveFlag()).
		Run(func(ctx *cli.Context) error {
			o, err := directories(ctx)
			if err != nil {
				return err
			}
			return runStatus(ctx, o.ArchiveDir)
		})

	app.Command("verify").
		Description("Re-hash every copy against the database").
		Long("Re-hash every copy, current and superseded, against the database. "+
			"With --snapshot, also check that every file in a snapshot has a copy "+
			"that equals it or begins with it. Exits 1 when it finds a problem.").
		Flags(
			archiveFlag(),
			cli.String("snapshot", "").Help("snapshot directory to check, holding claude/ and codex/"),
		).
		Run(func(ctx *cli.Context) error {
			o, err := directories(ctx)
			if err != nil {
				return err
			}
			snapshot := ctx.String("snapshot")
			if snapshot != "" {
				if snapshot, err = expandHome(snapshot); err != nil {
					return err
				}
			}
			res, err := runVerify(o.ArchiveDir, snapshot)
			if err != nil {
				return err
			}
			printVerifyResult(ctx.Stdout(), o.ArchiveDir, res)
			if n := res.problems(); n > 0 {
				return fmt.Errorf("verify failed: %d problems", n)
			}
			ctx.Success("verify passed")
			return nil
		})

	schedule := app.Group("schedule").
		Description("Install or remove the daily LaunchAgent that runs copy (macOS)")

	schedule.Command("install").
		Description("Install a LaunchAgent that runs copy daily at 03:00 and at login").
		Long("Run this from the installed binary: the LaunchAgent runs the binary at its current path. " +
			"Directories that differ from the defaults are written into the LaunchAgent.").
		Flags(originalsFlags()...).
		Run(func(ctx *cli.Context) error {
			o, err := directories(ctx)
			if err != nil {
				return err
			}
			s, err := newScheduler()
			if err != nil {
				return err
			}
			return s.install(o, ctx.Stdout())
		})

	schedule.Command("remove").
		Description("Unload the LaunchAgent and delete its plist").
		Run(func(ctx *cli.Context) error {
			s, err := newScheduler()
			if err != nil {
				return err
			}
			return s.remove(ctx.Stdout())
		})

	return app
}

// directories reads the directory flags and makes them absolute, expanding
// ~. An empty value, from a flag the command lacks or an empty environment
// variable, means the default.
func directories(ctx *cli.Context) (options, error) {
	var o options
	for _, d := range []struct {
		dst        *string
		flag, dflt string
	}{
		{&o.ArchiveDir, "archive", defaultArchiveDir},
		{&o.ClaudeDir, "claude-dir", defaultClaudeDir},
		{&o.CodexDir, "codex-dir", defaultCodexDir},
	} {
		v := ctx.String(d.flag)
		if v == "" {
			v = d.dflt
		}
		abs, err := expandHome(v)
		if err != nil {
			return o, err
		}
		*d.dst = abs
	}
	return o, nil
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
