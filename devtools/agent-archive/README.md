# agent-archive

`agent-archive` copies Claude Code and Codex session files into a local
archive, so they outlive the apps' own cleanup. Each copy is byte-exact, and
a database describes them. The design is in
[`design/agent-archive.md`](../../design/agent-archive.md).

## Install

The repository's root `go.work` does not list this module, so install it
from its own folder:

```sh
cd devtools/agent-archive && go install .
```

## Commands

| Command | What it does |
| --- | --- |
| `agent-archive copy` | Copy new and changed originals and update the database. |
| `agent-archive status` | Show the last copy run, counts, warnings, and failed files. |
| `agent-archive verify` | Re-hash every copy, current and superseded, against the database. |
| `agent-archive verify --snapshot <dir>` | Also check that every file in a snapshot has a copy that equals it or begins with it. |
| `agent-archive rebuild` | Recreate the database from the copy log and the copies. |
| `agent-archive schedule install` | Install a LaunchAgent that runs `copy` daily at 03:00 and at login (macOS). |
| `agent-archive schedule remove` | Unload the LaunchAgent and delete its plist. |

Run `agent-archive <command> --help` for a command's flags.

| Flag | Default | Environment variable | Taken by |
| --- | --- | --- | --- |
| `--archive` | `~/agent-archive` | `AGENT_ARCHIVE_DIR` | all but `schedule remove` |
| `--claude-dir` | `~/.claude` | `AGENT_ARCHIVE_CLAUDE_DIR` | `copy`, `schedule install` |
| `--codex-dir` | `~/.codex` | `AGENT_ARCHIVE_CODEX_DIR` | `copy`, `schedule install` |

A flag wins over its environment variable. `schedule install` writes the
directories into the LaunchAgent, since launchd does not see your shell's
variables. `verify` exits 1 when it finds a problem, as does any command
that fails. Output has color only on a terminal; `--no-color` or `NO_COLOR`
turns it off. Run `schedule install` from the installed binary: the
LaunchAgent runs the binary at its current path.

## Archive layout

```text
~/agent-archive/
  claude-code/      copies mirroring ~/.claude (projects/, history.jsonl, ...)
  codex/            copies mirroring ~/.codex; session files by date in sessions/YYYY/MM/DD/
  copy-log.jsonl    append-only log of every copy and run
  archive.sqlite    database built from the log and the copies
  logs/             output of the scheduled copy
  snapshots/        the one-time snapshot of 2026-10-03, not copies
```

`.jsonl` copies are gzipped. A copy whose original was rewritten is kept
with `.superseded-<UTC time>` added to its name.

## Privacy

The archive holds client code and probably secrets from tool output.

- It stays on this machine: never in a repository, a hosted database, or a
  synced folder.
- The archive folder is `0700` and its files `0600`; `copy` tightens looser
  permissions and warns.
- Never commit real session content. Test fixtures are hand-written.
