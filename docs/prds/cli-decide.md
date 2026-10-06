# Turn on decide checks in the dashi CLI

Status: Draft\
PRD PR: [#206](https://github.com/deepnoodle-ai/dashi/pull/206) · Implementation PR: pending · Updated: 2026-10-06\
Tech spec: [docs/design/cli-decide.md](../design/cli-decide.md)

## Problem

A developer who uses `dashi` as a coding agent has two poor choices today.
In the default mode, every shell command asks, safe or not, and the prompt
gives no hint which ones deserve a careful read. With
`--dangerously-skip-permissions` (#202), nothing asks, and the guide says to
use it only inside a container. Long sessions have two more gaps: the stock
summary drops exact paths and error strings, and nothing notices when the
agent repeats a failing call.

Dashi has decision-model packages for these jobs (`turnwatch`, `selective`,
and the `toolcheck` package specified in
[docs/prds/decide-tool-gate.md](decide-tool-gate.md)), but the CLI cannot
use them.
Our decide plugin for Claude Code proves the design in another agent.

## Context

- **Approvals today.** The coding-tools policy asks the CLI about each
  write, edit, shell command, and MCP call. `a` remembers an exact command
  for the process. `--dangerously-skip-permissions` answers yes to all of
  them. `dashi run` refuses them unless `--yes`.
- **The decide plugin (v0.3.0)** checks shell commands in bypass mode only
  (`severe` at 80%), and checks web pages, MCP results, and `gh`, `curl`,
  `wget`, and `http` output for prompt injection (60%). It warns without
  hiding the result, shows a footer count and a mark on each checked row,
  fails open, and pauses a check for a minute after a failure. It dropped
  its reply check, which flagged one honest reply in five.
- **Others.** Claude Code auto mode lets a classifier model approve calls.
  Codex CLI pairs approvals with an OS sandbox.
- **Selective compaction eval:** needle recall 100% against 75% for stock,
  at about 8% of the summary's cost, on generated sessions only.

## Users and job

Primary: a developer who runs `dashi` in their own repository. They want
fewer prompts for safe work, a second opinion before a dangerous command,
a compaction that keeps exact details, and an agent that notices its own
loops. They will not accept surprise spend, surprise latency, or their
commands sent to a third party without their consent.

## Concepts

- **decide**: the one word the user sees, in the setting, the flag, and on
  screen. It names our client for decision models: fast models (TypeSafe's
  Jev or Cloudflare's Clef) that answer typed questions about a text with
  probabilities and write no prose.
- **Command-risk check**: asks whether a shell command could cause severe
  harm that is hard to undo. A command is **flagged** when the probability
  clears 80%. It reads shell commands only.
- **Prompt-injection check**: asks whether a tool result tries to take
  over the agent or hides text from the person.
- **Turn watcher**: notices a repeated failing call, confirms it with
  decide, and gives the agent a short reminder.
- **Selective compaction**: keeps old messages that are still needed word
  for word, and summarizes only the rest.
- **Unchecked**: a call decide could not answer. Two kinds differ. An
  **outage** (auth failure, server error, network error) **pauses** the
  check for a minute. A **suspect** failure is one the command text can
  cause (timeout, input too large, rate limit, request rejected); it never
  pauses.
- **Setup error**: no credentials or an unknown `DECIDE_PROVIDER`. It is
  not an outage: dashi finds it at start and turns the checks off.

## Use cases

### UC-1: Turn features on and see them

The user adds `{"decide": ["command_risk", "turn_watcher"]}` to
`~/.dashi/settings.json`, or starts `dashi --decide command_risk`.

Acceptance:
- [ ] With no setting, flag, or variable, every feature is off and dashi
      sends no decide request.
- [ ] `/settings` has a "decide" section: each feature on or off with its
      source, the line `command_risk: shell commands only`, the provider
      state (`typesafe, not used yet`, `ready: typesafe jev-1.13.0`, or
      `off: no credentials`), and this run's requests, input and
      output tokens, checked, flagged, and unchecked. `--json` has the same
      fields under `decide`.
- [ ] `--decide off` turns every feature off for this launch.

### UC-2: A prompt shows the command-risk answer

Default mode, `command_risk` on. The agent proposes
`git push --force origin main`. The prompt shows at once with
`decide: checking…`. Under a second later that line reads
`decide: severe 90% — flagged`. The user answers `n`.

Acceptance:
- [ ] The prompt never waits for decide. `y` and `n` work at once; an answer
      given first cancels the check, and the record says so.
- [ ] The line shows the answer within 3 seconds, or reads
      `decide: unchecked (timeout)`.
- [ ] One call gets one prompt. A flagged prompt offers `y` and `n` only,
      so a flagged command is never remembered.
- [ ] A command remembered with `a` runs without a check, as before.

### UC-3: Bypass mode still asks about a severe command

`dashi --dangerously-skip-permissions --decide command_risk`.
`go test ./...` runs at once. `git push --force origin main` stops at a
prompt with the answer line. The user answers `n`, and the model reads why.

Acceptance:
- [ ] A command that is not flagged runs without a prompt. Its row shows
      `decide: checking…` while it waits, at most 3 seconds.
- [ ] A flagged command asks. Calls from delegated tasks are checked the
      same way.
- [ ] `write_file`, `edit_file`, and MCP calls still run without a prompt;
      the guide and `/settings` say so.
- [ ] A suspect failure asks, like a flag, with the line
      `decide: unchecked (timeout)`.
- [ ] `dashi run --yes --decide command_risk` refuses a flagged command and
      a suspect failure; the model reads a reason that names the check.
      Other commands run.
- [ ] A checked row ends with `decide ✓`, or shows one line under it with
      the flagged question and probability. The turn footer adds
      `decide 4 checked, 1 flagged`. A reopened session shows the answers
      from the turn record.

### UC-4: No key, or decide fails

A feature is on, and `TYPESAFE_API_KEY` is not set. Later, with a key, a
request times out.

Acceptance:
- [ ] A setup error gives exactly one warning per launch, before the first
      turn, naming the variables to set. The status area then shows
      `decide: off (no credentials)`. No prompt, row, or footer shows
      decide, and the check never pauses.
- [ ] `dashi run --yes` with `command_risk` on and a setup error refuses
      to start, naming the variable to set.
- [ ] In default mode an unchecked prompt reads `decide: unchecked
      (timeout)`; the user answers as usual.
- [ ] Only an outage pauses a check. While paused, the status area shows
      `decide: command_risk paused (server error), resumes in 45s`, and
      bypass mode runs commands without asking. The guide says so.
- [ ] The status area is quiet when every check is answering.

### UC-5: Outside content gets a warning (PR 2)

`prompt_injection` on. A `curl` result says "ignore previous instructions".

Acceptance:
- [ ] The check reads what `toolcheck.PromptInjection` reads: every MCP
      result, and `bash` results whose command runs `gh`, `curl`, `wget`, or
      `http`. Workspace file tools are not sent.
- [ ] A flagged result reaches the model with a warning; it is not hidden.
      The row shows the flagged question and probability.

### UC-6: The agent gets a reminder when it loops (PR 2)

`turn_watcher` on. `go test` fails three times the same way. The transcript
shows `decide: reminded the agent (repeating 88%)`, live and on reopen.

### UC-7: Compaction keeps exact details (PR 3)

`selective_compaction` on. Automatic compaction and `/compact` use it.
`/settings` marks it experimental. A fallback to stock compaction shows its
cause in the compaction line.

## Requirements

- R-1: Features: `command_risk`, `prompt_injection`, `turn_watcher`,
  `selective_compaction`, named after the library types. Each is off unless
  turned on.
- R-2: Sources, highest first: `--decide LIST`, then `DASHI_DECIDE`, then the
  `decide` list in `~/.dashi/settings.json`. A list replaces lower tiers.
  `off` turns all off. An unknown name is a startup error that lists the
  valid names.
- R-3: A `decide` key in a project `.dashi/settings.json` is ignored with
  one warning.
- R-4: The library's default decide client chooses the provider, and
  reports it and any setup error before a request:
  `DECIDE_PROVIDER` (`typesafe` or `cloudflare`), then `TYPESAFE_API_KEY`
  and `TYPESAFE_BASE_URL`, or `CLOUDFLARE_AUTH_TOKEN`,
  `CLOUDFLARE_ACCOUNT_ID`, and `CLOUDFLARE_BASE_URL`, as the decide CLI
  reads them. Every decide package honors it. The CLI reads none of these
  variables itself. `--env-file` works for them.
- R-5: Thresholds are the library's (80% and 60%). Users cannot change them.
- R-6: Model advice never grants permission. A check may add a prompt, add
  a line to a prompt, refuse a call where nobody can answer, or warn. It
  never approves, never creates a remembered rule, and never removes a
  prompt the policy would show.
- R-7: Every checked approval records the check's one-line reason
  (probability, request ID, model version) in the turn record, in the
  approval decision's `Reason`.
- R-8: The CLI reaches each feature through library packages only.
- R-9: `docs/guides/cli.md` and `docs/guides/approvals.md` say what each
  feature sends to the provider and what bypass mode still skips.

## Not in scope

- Context ranking. The CLI has nothing to rank; AGENTS.md and skills load
  by their own rules.
- List tools (`judge_items`, `pick_item`). A coding agent has no host list.
- Dollar spend. decide reports tokens only and has no price table.
- A model pin and user-set thresholds. Wait for a user who asks.
- An answer cache. It belongs in the library check.

## Decisions

- **Every feature is off by default.** No surprise spend, latency, or data
  sent out. Rejected: on when a key is set; a key is not consent.
- **Fewer prompts come from bypass mode with the check, not from the check
  approving calls.** The cost is plain: bypass mode also drops the prompts
  for edits, writes, and MCP calls, and no check covers them. Rejected:
  auto-approve calls the check scores safe, as Claude Code auto mode does.
- **One approval path, one prompt.** The CLI calls
  `toolcheck.CommandRisk.Check` inside its own approval step, shows the
  answer in the one prompt, and writes the answer's reason into the
  approval's record. Rejected: join the `CommandRisk` hook with the policy;
  its hold beats the person's `y` and stops the turn.
- **The command-risk check runs in both modes.** In default mode it adds a
  line to the prompt the user sees anyway; in bypass mode it adds a prompt
  for a flagged command. Rejected: bypass mode only, as the plugin does;
  the brief's main case is the default mode.
- **The prompt shows at once.** Rejected: wait for decide, which freezes
  the prompt.
- **`dashi run --yes` refuses a flagged command.** Nobody can answer.
  Rejected: let it run; then the check does nothing in CI.
- **Checks fail open only for an outage.** An outage pauses the check, the
  status area shows the pause, and bypass mode runs commands unasked. A
  suspect failure (timeout, input too large, request rejected) asks, even
  in bypass mode, and `run --yes` refuses it. Injected text can make the
  model write a huge command that times out; failing open there would open
  an unchecked window on demand. Timeouts and cancels never pause.
  Rejected: fail open on every failure, as the plugin does; fail closed on
  every failure, which an outage would turn into a prompt per command.
- **User settings, flag, and variable only.** A project file that turns a
  feature on would send the user's commands to a third party with the
  user's key. Rejected: honor project settings, as for model controls.
- **One word, `decide`, and library names for features.** A user who sees
  `decide ✓` finds the `decide` setting. Rejected: `decision_models`, a
  second name for one thing.
- **Provider choice lives in the library's default client.** Every host
  gets Cloudflare by setting variables. Rejected: build the client in the
  CLI, which every other host would have to copy.
- **A setup error is found at start, through the library.**
  `defaultclient.Load` builds the default client without a request and
  reports the provider and any setup error, so the CLI still reads no
  decide variable. Interactive modes warn and continue; `dashi run --yes`
  refuses to start, because nobody watches its output. Rejected: learn it
  at the first check, which made an unknown provider look like an outage
  and ran the first command before the warning.
- **Three implementation PRs.** PR 1: the client helper, settings, flag,
  `/settings`, warnings, footer, row marks, and the command-risk check. PR
  2: prompt-injection check and turn watcher. PR 3: selective compaction,
  experimental until its eval passes on recorded CLI sessions. Rejected:
  one PR that waits on every dependency.

## Dependencies

- #204 (merged): decide packages in the root module, on decide v0.3.0.
- The `toolcheck` package: `CommandRisk.Check(ctx, dashi.ToolPlan)
  (Verdict, error)`, where `Verdict` holds flagged, probability, request ID,
  model version, a one-line reason, and usage; a typed paused error with
  its end time; an error kind that tells an outage from a suspect failure;
  and `PromptInjection` with an `OnCheck`.

## Success

- A live bypass-mode session asks about a severe command and lets routine
  commands run; the PR shows the transcript and the flag rate.
- Median added wait per bash call in bypass mode under one second; never
  more than 3 seconds.
- In the dogfood week, we count sessions that use bypass mode with the
  check outside a container, and whether the user kept the check on.
- Must not get worse: startup and every turn with all features off.

## Risks

- **False flags** make users turn the check off. The `severe` question
  reduces this; PR 1 reports the flag rate from a live session.
- **A text judge misses hidden effects** (`make clean`, a script). The
  guide says the check is a second opinion, not a sandbox.
