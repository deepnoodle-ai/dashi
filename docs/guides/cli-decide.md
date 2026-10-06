# decide checks in the dashi CLI

decide is our client for decision models: fast models, such as TypeSafe's
Jev or Cloudflare's Clef, that answer typed questions about a text with a
probability. The dashi CLI can ask one for a second opinion before it runs a
shell command. The check is advice, not a sandbox. It can add a prompt or a
warning line. It never approves a call and never removes a prompt.

Every decide feature is off until you turn it on. With every feature off,
dashi sends no decide request and shows nothing new.

## Turn a feature on

Name the features in your user settings file, `~/.dashi/settings.json`:

```json
{"decide": ["command_risk"]}
```

Or name them for one launch, with a flag or a variable:

```sh
dashi --decide command_risk
DASHI_DECIDE=command_risk dashi
dashi --decide off      # turn every feature off for this launch
```

`--decide` wins over `DASHI_DECIDE`, which wins over the settings file. The
highest one present replaces the others. `off` turns every feature off. An
unknown name stops dashi at start and lists the valid names.

A `decide` key in a project's `.dashi/settings.json` is ignored with a
warning. A repository must not be able to send your commands to a third
party with your key, or turn off your check.

| Feature | What it does | Status |
| --- | --- | --- |
| `command_risk` | Checks each shell command for severe harm that is hard to undo | Available |
| `prompt_injection` | Warns the model about a tool result that tries to take it over | Later release |
| `turn_watcher` | Reminds the agent when it repeats a failing call | Later release |
| `selective_compaction` | Keeps needed messages word for word when it compacts | Later release |

A feature from a later release stays off, with a warning.

## Credentials and provider

dashi passes no decide settings of its own; the library reads the same
variables as the `decide` command. Set them in your shell, or load them with
`--env-file`:

- TypeSafe (the default): `TYPESAFE_API_KEY`, and `TYPESAFE_BASE_URL` if you
  need another endpoint.
- Cloudflare Workers AI: `DECIDE_PROVIDER=cloudflare`,
  `CLOUDFLARE_AUTH_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, and `CLOUDFLARE_BASE_URL`
  if needed.

dashi checks this setup at start, before the first turn, without sending a
request. With no credentials or an unknown `DECIDE_PROVIDER`, it shows one
warning that names the variables to set, and the status area shows
`decide: off (no credentials)` or `decide: off (unknown DECIDE_PROVIDER
"x")`. Every call then runs as it would without decide: no prompt, row, or
footer shows decide. `dashi run --yes` with `command_risk` on refuses to
start instead, because nobody watches that output.

## The command-risk check

The check asks whether a shell command could cause severe harm that is hard
to undo, beyond the local checkout: deleting a home folder, force-pushing a
shared branch, dropping a database, or sending secrets away. A command is
**flagged** when the probability reaches 80%. It reads the workspace's own
`bash` tool only; edits, writes, and MCP calls are not checked. A command you
allowed with `a` runs again without a check.

**Default mode.** The prompt shows at once with `decide: checking…`, and the
keys work at once. Within 3 seconds the line shows the answer, such as
`decide: severe 90% — flagged`, `decide: severe 52% — ok`, or
`decide: unchecked (timeout)`. A flagged
command offers `y` and `n` only, so it is never remembered. If you answer
before the check, it is canceled and the record says
`not checked (answered first)`.

**Bypass mode** (`--dangerously-skip-permissions`). A command that is not
flagged runs without a prompt; its row shows `decide: checking…` while it
waits, at most 3 seconds. A flagged command asks, with `y` and `n` only, and
its row reads `waiting for you`. Calls from delegated tasks are checked the
same way, at the same time; other calls never wait behind a check. Bypass mode still runs
`write_file`, `edit_file`, and MCP calls without a prompt or a check, so keep
using a container.

**`dashi run --yes`.** Nobody can answer, so a command that would ask in
bypass mode is refused. The model reads a reason that names the check.
`dashi run` without `--yes` refuses every call that needs approval, so it
never checks.

## When decide cannot answer

An unchecked call has one of two kinds of cause:

- **Suspect:** a timeout, a command too large to check, a rate limit, or a
  rejected request or answer. The command text can cause these, so the call
  asks even in bypass mode, and `run --yes` refuses it. They never pause the
  check.
- **Outage:** a failed login, a server error, or a dropped connection.
  These most likely come from the provider, not the command, so the check
  pauses for a minute, and the status area counts down:
  `decide: command_risk paused (server error), resumes in 45s`. During the
  pause, bypass mode runs commands without asking. Line mode and `dashi run`
  print one warning per pause instead.

A known gap: a server error or dropped connection pauses the check for a
minute even when the command's own text caused it, such as a provider bug
on some text, or a gateway failure on a large command. During that minute,
bypass mode runs every shell command unchecked, delegated tasks' too.

The status area is quiet while every check answers.

## What you see

- A checked row that ran ends with `decide ✓`. Any other checked row shows
  one line under it, such as `decide: severe 90% — flagged`,
  `decide: unchecked (timeout)`, or, beside your denial,
  `decide: severe 52% — ok`.
- The turn footer adds `decide 4 checked, 1 flagged` (and `, N unchecked`),
  leaving zero counts out. A narrow screen drops other footer values first.
- A reopened session shows the same marks, from the turn record.
- `/settings` has a `decide` section: each feature with its source, what
  `command_risk` reads, what bypass mode skips, the provider state (`typesafe,
  not used yet`, `ready: typesafe jev-1.13.0`, or `off: no credentials`), and this run's
  requests, tokens, and checked, flagged, and unchecked calls.
  `/settings --json` has the same fields under `decide`.

The turn record keeps each answer in the approval decision: its reason gets
`; decide: severe 90% flagged (jev-1.13.0, request r_12)`, and its rules
version names `decide/` and the model.

## What is sent

`command_risk` sends each checked command's text to TypeSafe or Cloudflare,
with your key. A secret typed into a command goes too. Thresholds are
decide's and cannot be changed. decide reports tokens, not dollars.
