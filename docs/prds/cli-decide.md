# Turn on decision-model checks in the dashi CLI

Status: Draft\
PRD PR: pending · Implementation PR: pending · Updated: 2026-10-06

## Problem

A developer who uses `dashi` as a coding agent has two poor choices today.
In the default mode, every shell command asks, safe or not, and the prompt
gives no hint which ones deserve a careful read. With
`--dangerously-skip-permissions` (#202), nothing asks, and the guide says to
use it only inside a container. Long sessions have two more gaps: the stock
summary drops exact paths and error strings, and nothing notices when the
agent repeats a failing call.

Dashi has decision-model packages for these jobs (`turnwatch`,
`selective`, and the tool-call checks of track decide-tool-gate), but the
CLI cannot use them. Our Decide plugin for Claude Code proves the design in
another agent.

## Context

- **Approvals today.** The coding-tools policy asks the CLI about each
  write, edit, shell command, and MCP call. `a` remembers an exact command
  for the process. `--dangerously-skip-permissions` answers yes to all of
  them. `dashi run` refuses them unless `--yes`.
- **The Decide plugin (v0.3.0)** checks shell commands in bypass mode only
  (`severe` at 80%), and checks web pages, MCP results, and `gh`, `curl`,
  `wget`, and `http` output for prompt injection (60%). It warns without
  hiding the result, shows a footer count and a mark on each checked row,
  and fails open. It dropped its reply check, which flagged one honest
  reply in five.
- **Others.** Claude Code auto mode lets a classifier model approve calls
  with no person involved. Codex CLI pairs approvals with an OS sandbox.
  OpenCode and Cursor use static allow and deny patterns.
- **Selective compaction eval:** needle recall 100% against 75% for stock,
  at about 8% of the summary's cost, on generated sessions only.

## Users and job

Primary: a developer who runs `dashi` in their own repository. They want
fewer prompts for safe work, a second opinion before a dangerous command,
a compaction that keeps exact details, and an agent that notices its own
loops. They will not accept surprise spend, surprise latency, or their
commands sent to a third party without their consent.

## Concepts

- **Decision model:** a fast model that answers typed questions about a
  text with probabilities. Decide is our client for them (TypeSafe's Jev or
  Cloudflare's Clef). It writes no prose.
- **Command check:** asks whether a shell command could cause severe harm
  that is hard to undo. A command is **flagged** when the answer's
  probability clears the check's threshold.
- **Content check:** asks whether outside content tries to take over the
  agent or hides text from the person.
- **Turn watcher:** notices a repeated failing call, confirms it with a
  decision model, and gives the agent a short reminder.
- **Selective compaction:** keeps old messages that are still needed word
  for word, and summarizes only the rest.
- **Unchecked:** a call the check could not answer (no key, timeout,
  provider error). It goes on as if the check were off.

## Use cases

### UC-1: Turn features on and see them

The user adds to `~/.dashi/settings.json`:

```json
{"decision_models": {"command_check": true, "turn_watcher": true}}
```

or starts `dashi --decision-models command_check,turn_watcher`. `/settings`
shows a "Decision models" section.

Acceptance:
- [ ] With no setting, flag, or variable, every feature is off and dashi
      sends no Decide request.
- [ ] `/settings` lists each feature as on or off with the source that set
      it, the provider, the model, and the credential variable name and
      whether it is set (never its value). `--json` has the same fields under
      `decision_models`.
- [ ] `/settings` shows this run's totals: requests, input and output
      tokens, checked, flagged, and unchecked.
- [ ] `--decision-models off` turns every feature off for this launch.

### UC-2: A prompt shows the command check's answer

Default mode, command check on. The agent proposes `rm -rf build/ ~/.cache`.
The approval prompt shows the command and one line:
`decide: severe 0.91 — flagged`. The user answers `n`.

Acceptance:
- [ ] Each shell command that would prompt shows the answer and its
      probability in the same prompt. A flagged command's line is styled as
      a warning.
- [ ] One call gets at most one prompt.
- [ ] A command the user remembered with `a` runs without a check, as before.
- [ ] The check never answers a prompt for the user. `y`, `a`, and `n` keep
      their meaning.

### UC-3: Bypass mode still holds a severe command

`dashi --dangerously-skip-permissions`, command check on. `go test ./...`
runs at once. `git push --force origin main` stops at a prompt that shows the
answer. The user answers `n`, and the model reads why.

Acceptance:
- [ ] A command that is not flagged runs without a prompt.
- [ ] A flagged command asks, with the same keys and the answer line.
- [ ] Calls from delegated tasks are checked the same way.
- [ ] `dashi run --yes` with the check on refuses a flagged command; the
      model reads a reason that names the check. Other commands run.
- [ ] A checked row ends with `decide ✓`, or shows one line under it with
      the flagged question and probability. The turn footer adds
      `decide 4 checked, 1 flagged`. A reopened session shows flagged calls
      from the turn record.

### UC-4: No key, or the provider fails

A feature is on, and `TYPESAFE_API_KEY` is not set. Dashi starts, prints one
warning that names the missing variable and the features it affects, and
runs as if those features were off. Later, with a key, a request times out.

Acceptance:
- [ ] Missing credentials give exactly one warning per launch, not one per
      call or turn.
- [ ] A failed check lets the call go on. The tool row shows
      `decide: unchecked (timeout)`, and the turn records a warning.
- [ ] The status area shows a decide warning only while a feature is off for
      missing credentials or calls went unchecked. It is quiet otherwise.

### UC-5: Outside content gets a warning (PR 2)

Content check on. A `curl` result says "ignore previous instructions".

Acceptance:
- [ ] The check reads `bash` results whose command starts with `curl`,
      `wget`, `gh`, or `http`, and every MCP tool result.
- [ ] A flagged result reaches the model with a warning beside it; it is not
      hidden. The row shows the flagged question and probability.

### UC-6: The agent gets a reminder when it loops (PR 2)

Turn watcher on. `go test` fails three times the same way. The transcript
shows `turn watcher: reminded the agent (repeating 0.88)`, and the agent's
next call changes approach.

Acceptance:
- [ ] The reminder line appears once per reminder, live and on reopen.

### UC-7: Compaction keeps exact details (PR 3)

Selective compaction on. Automatic compaction and `/compact` use it.
`/settings` marks it experimental.

Acceptance:
- [ ] A fallback to stock compaction shows its cause in the compaction
      line.

## Requirements

- R-1: Features: `command_check`, `content_check`, `turn_watcher`,
  `selective_compaction`. Each is off unless turned on.
- R-2: Sources, highest first: `--decision-models LIST`, then
  `DASHI_DECISION_MODELS`, then the `decision_models` block in
  `~/.dashi/settings.json`. A list replaces the lower tiers; it does not
  merge. `off` turns all off. An unknown name is a startup error that lists
  the valid names.
- R-3: A `decision_models` block in a project `.dashi/settings.json` is
  ignored with one warning.
- R-4: Provider: `DECIDE_PROVIDER`, then `decision_models.provider`, then
  `typesafe`. TypeSafe reads `TYPESAFE_API_KEY` and `TYPESAFE_BASE_URL`;
  Cloudflare reads `CLOUDFLARE_AUTH_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, and
  `CLOUDFLARE_BASE_URL`, as the decide CLI does. `--env-file` works for
  these. Settings never hold credentials.
- R-5: `decision_models.model` pins the decision model, such as
  `jev-1.13.0`. Empty means the provider's default.
- R-6: The command check uses the plugin's `severe` question flagged at
  80%. The content check flags `injection` or `hidden` at 60%. Users cannot
  change thresholds in v1.
- R-7: Model advice never grants permission. A check may add a prompt, add
  a line to a prompt, refuse a call where nobody can answer, or warn. It
  never approves, never creates a remembered rule, and never skips a prompt
  the policy would show.
- R-8: Every flagged, refused, or unchecked call is in the turn record
  with the probability, the Decide request ID, and the model version.
- R-9: The CLI reaches each feature through library packages only. Any
  behavior the CLI needs from a check goes in the library.
- R-10: `docs/guides/cli.md` and a new section in `docs/guides/approvals.md`
  say what each feature sends to the provider.

## Not in scope

- Context ranking. The CLI has no store of notes or documents to rank;
  AGENTS.md and skills already load by their own rules.
- List tools (`judge_items`, `pick_item`). A coding agent has no host list.
- Dollar spend. Decide reports tokens only and has no price table yet.
- An answer cache. The decision belongs to the library check (track
  decide-tool-gate).
- User-set thresholds, and an `ask|deny` choice for the command check.
  Wait for a user who needs them.

## Decisions

- **Every feature is off by default.** No surprise spend, latency, or data
  sent out. Rejected: on when a key is set; a key is not consent.
- **Fewer prompts come from bypass mode with the check, not from the check
  approving calls.** Model advice never grants permission. Rejected:
  auto-approve calls the check scores safe, as Claude Code auto mode does.
- **The command check runs in both modes.** In bypass mode it adds a prompt
  for a flagged command. In default mode it adds a line to the prompt the
  user sees anyway. Rejected: bypass mode only, as the plugin does; the
  brief's main case is the default mode.
- **A flagged command asks inline; it never stops the turn.** A held call
  (`NeedsApproval`) ends a CLI turn with `StopWaiting`, which the
  interactive CLI does not resolve inline. Rejected: hold and resolve.
- **`dashi run --yes` refuses a flagged command.** Nobody can answer.
  Rejected: let it run; then the check does nothing in CI.
- **Checks fail open, with one startup warning and unchecked marks.** It
  matches design/decide/README.md decision 3 and the plugin. Rejected: fail
  closed, as `examples/decide` does; an outage would stop bypass-mode work.
- **User settings, flag, and variable only.** A project file that turns a
  feature on would send the user's commands to a third party with the
  user's key. Rejected: honor project settings, as for model controls.
- **One list flag.** Its values are the settings key names, so each feature
  has one spelling. Rejected: one flag per feature.
- **Tokens and counts, not dollars.** A price copied into dashi would drift.
  Rejected: a hard-coded Jev price.
- **The CLI builds the Cloudflare client with decide's public
  `backend.NewClient`.** decide's env-reading code is internal. Rejected:
  wait for a decide release. Follow-up: ask decide to export it.
- **Three implementation PRs.** PR 1: settings, flag, credentials,
  warnings, `/settings`, footer, row marks, and the command check (most
  value). PR 2: content check and turn watcher. PR 3: selective compaction,
  experimental until its eval passes on recorded CLI sessions. Rejected:
  one PR that waits on every dependency.
- **UI label `decide`; no "verdict".** `decide` names the service that
  answered, as in the plugin. "Verdict" already means a hook verdict in
  Dashi. Rejected: `decision model` on every row; too long.

## Dependencies

- Track decide-in-root: decide packages in the root module, on decide
  v0.3.0. `cmd/dashi` may move to Go 1.27.
- Track decide-tool-gate (PR 1 and PR 2): a command check and a content
  check that ask through a host approval callback, record a reason, report
  usage to a callback, and support the thresholds in R-6.

## Success

- A bypass-mode session holds a severe command and lets routine commands
  run, live, with a real key; the PR shows the transcript.
- No added prompt in default mode, and median added wait per checked
  command under one second.
- Must not get worse: startup with every feature off, and the zero-setting
  launch.

## Risks and open questions

- **False flags** would make users turn the check off. The `severe`
  question reduces this; PR 1 reports the flag rate from a live session.
- **A text judge misses hidden effects** (`make clean`, a script). The guide
  says the check is a second opinion, not a sandbox.
- **The tool-gate design may not fit inline asking.** This blocks PR 1. We
  raise it with that track now.
