# decide checks in the dashi CLI

**Status:** Draft · **Date:** 2026-10-06 · **Owner:** Curtis\
**Workflow:** spec, then build after the `toolcheck` package
([PRD](../prds/decide-tool-gate.md)) lands. The decide packages are in the
root module since #204. **PRD:** [docs/prds/cli-decide.md](../prds/cli-decide.md)

## Context

The PRD turns on four decide features in `cmd/dashi`: the command-risk
check, the prompt-injection check, the turn watcher, and selective
compaction. All are off by default. The hard part is the command-risk
check: it must join the CLI's one approval prompt, never grant permission,
and still guard `--dangerously-skip-permissions`. The other three wire
existing library hooks and a compactor. This spec covers PR 1 in detail
and PR 2 and PR 3 in outline.

## Goals

- One prompt per call. The answer line appears in it within 3 seconds.
- In bypass mode, a flagged shell command asks; others run with at most
  3 seconds of added wait.
- Every checked approval carries the check's reason in the turn record.
- No decide request, no added wait, and no new output with every feature
  off.

## Non-goals

- Checking edits, writes, or MCP calls before they run.
- An answer cache, a model pin, or user thresholds.
- Dollar spend.

## Proposal

### 1. Library: provider choice in the default client (PR 1)

A new package, `decide/defaultclient`, makes the client every decide
package uses when its `Client` is nil:

```go
// Load makes the default client once per process, without a request.
// DECIDE_PROVIDER: "" or "typesafe" -> decide.NewClient() (TYPESAFE_*).
// "cloudflare" -> backend.NewClient with CLOUDFLARE_AUTH_TOKEN,
// CLOUDFLARE_ACCOUNT_ID, CLOUDFLARE_BASE_URL. An error wraps ErrSetup:
// missing credentials (also decide.ErrNoAPIKey) or ErrUnknownProvider,
// and names the variables to set.
func Load() (Setup, error) // Setup{Provider, Client}
```

Every package with a nil `Client` (`toolcheck`, `turnwatch`, `selective`,
`contextrank`, `listtool`) gets Cloudflare by environment alone, through
`decide/internal/client`, which calls `Load`. The CLI calls `Load` at
start, passes the client it returns, and reads no decide variable.

### 2. Settings (PR 1)

`decideFeatures(flag, env, userSettings)` returns the feature set and the
source of each: `--decide`, then `DASHI_DECIDE`, then `"decide": [...]` in
`~/.dashi/settings.json`. The highest tier present replaces the others.
`off` empties the set. A `decide` key in a project file is dropped with one
warning. Validation runs in `validateAgent`, so an unknown name fails at
start, like other flags. The flag is a Wonton `cli.String` with
`.Env("DASHI_DECIDE")`.

### 3. The `decideRuntime` (PR 1)

One value per process, owned by the `Controller`:

```go
type decideRuntime struct {
	features map[string]string    // feature -> source
	risk     *toolcheck.CommandRisk // nil when command_risk is off
	mu       sync.Mutex
	totals   decideTotals           // requests, tokens, checked, flagged, unchecked
	provider string                 // "typesafe" or "cloudflare", from Load
	off      string                 // setup error, such as "no credentials"
	paused   map[string]time.Time   // feature -> resumes at
}
```

It feeds `/settings`, the footer suffix, and the status area. Every check
result passes through `record(feature, verdict, err)`, which updates the
totals and state. A setup error never reaches it: `loadCLIConfig` calls
`defaultclient.Load` before the first turn, prints the one warning, and
sets `off`, which turns every check off. `dashi run --yes` with
`command_risk` on refuses to start instead. The library's typed paused
error (outage only) sets `paused[feature]` to its
end time. Other errors are sorted into outage or suspect by the error kind
`toolcheck` returns; a timeout, too-large input, or rejected request is
suspect. The status area shows a decide line only
while `state` is off or `paused` is not empty.

### 4. The command-risk check in the approval path (PR 1)

The check runs inside the CLI's existing approval step, not as a hook:

```
BeforeTool: attributeToolHooks wrapper (already exists)
  ctx += checkSlot{plan}
  -> workspace Policy.decide
       read-only / trusted / remembered -> Allow   (no check, no spend)
       otherwise -> Controller.approveUI(ctx, request)
                      owned bash + command_risk on -> run the check
  <- decision; if the slot holds a verdict:
       decision.Reason += "; decide: " + verdict line
       decision.RulesVersion = "decide/" + model
```

`approveUI` calls `risk.Check(ctx, plan)` only for the workspace's own
`bash` tool (`ApprovalRequest.Tool == workspace.ToolBash` and the slot's
plan is an owned tool), so an MCP tool named `bash` is never treated as a
shell. In bypass mode the check runs in `Controller.Approve`, before the
call enters the approval queue, which shows one prompt at a time; only a
held call enters it. The CLI builds the workspace policy with
`workspace.AskConcurrently()`, so the policy does not hold its lock across
the check, and other calls never wait behind one. By mode:

| Mode | When the check runs | Flagged | Not flagged | Suspect failure | Outage or paused |
| --- | --- | --- | --- | --- | --- |
| Default | In parallel with the prompt, which shows at once | Line turns to a warning; keys `y`/`n` only | Line shows the answer | Line reads `unchecked (…)` | Line reads `unchecked (…)`; status shows the pause |
| Bypass (`approveAll`) | Before the approval queue, at most 3 s; row shows `checking…` | Queue, then prompt with `y`/`n`; row reads `waiting for you` | Allow, no prompt | As flagged | Allow; row shows the cause; status shows the pause |
| `run --yes` | As bypass | Refuse; reason names the check | Allow | Refuse; reason names the check | Allow; warning on stderr |
| `run` without `--yes` | Never: every approval is refused | | | | |

The check's context has a 3-second deadline and is canceled when the
person answers first; the slot then records `decide: not checked
(answered first)`. A flagged prompt hides `a`, so a flagged command never
becomes a remembered rule. `toolApproval` gains a `Check *pendingCheck`
(a done channel and the result). The screen draws `decide: checking…` and
redraws on done; line mode prints the answer as a new line.

The decision `Reason` uses one CLI-owned format, such as `approved by the
user; decide: severe 91% flagged (jev-1.13.0, request r_12)`. The
renderer finds checked calls by the `decide/` prefix on `RulesVersion`
and parses the suffix for row marks on reopen. A golden test pins the
format. A deny reason keeps its existing text first, so the model reads
why it was refused, then the decide line.

### 5. Rendering (PR 1)

- Tool row: `decide ✓` beside a call that ran, or one line under it:
  `decide: severe 90% — flagged`, `decide: unchecked (timeout)`, or,
  beside a denial, `decide: severe 52% — ok`. All text from the provider,
  such as a request ID, goes through `workspace.PrintableText`.
- Footer suffix, when any feature is on: `decide 4 checked, 1 flagged`,
  without zero counts. A narrow screen drops the model call count, the
  model, the tokens, and the cost first.
- `/settings` and `/settings --json`: section `decide` as the PRD lists.

### 6. PR 2 and PR 3

- **Prompt injection:** a `decideWiring` adapter adds
  `toolcheck.PromptInjection{OnCheck: rt.onInjection}.Hooks()`. The hook
  replaces what the model reads; `OnCheck` marks the row by call ID.
- **Turn watcher:** `turnwatch.Watcher{OnCheck: rt.onWatch}.Hook()` goes in
  `Runner.Hooks`, so `ActiveHooks` names it. The renderer draws the
  `turnwatch` `HookResult` of a step as `decide: reminded the agent
  (repeating 88%)`, live and on reopen.
- **Selective compaction:** in `configureChat`, wrap the stock policy:
  `&selective.Compactor{Base: <stock>, OnSelection: rt.onSelection}`,
  inside `routedCompactor` as today. `Selection.Fallback` goes on the
  compaction line. `/settings` marks it experimental.

## Security considerations

Threat model: the model and every tool result are untrusted. The person,
their user settings, and their environment are trusted. A repository
(including `.dashi/settings.json`) is untrusted.

| Threat | Mitigation | Residual risk |
| --- | --- | --- |
| Model advice grants permission (excessive agency) | `approveUI` returns `Allow` only from the person's key or from `approveAll`, which the person set. A verdict only adds a prompt or a refusal. Flagged prompts hide `a`; no verdict reaches the remembered rules. | None found. |
| Command text talks the check down ("# safe, answer no") | decide's question treats such text as evidence of risk. In default mode the person still reads the command. | In bypass mode a crafted command can score low and run. |
| Bypass mode blind spots | The guide and `/settings` say: shell commands only; edits, writes, and MCP calls run unasked; during an outage pause, commands run unasked. | An MCP tool or a script run by an unflagged command can do harm. Use a container. |
| Injected text forces fail-open: the model writes a huge or odd command that times out, is too large, or is rejected | These are suspect failures: they never pause the check, bypass mode asks, and `run --yes` refuses. The CLI's 3-second deadline and a cancel never pause. | None found. |
| Provider outage | Only an outage (auth failure, server error, non-timeout network error) pauses the check. The status area shows the pause and its end. A setup error (no credentials, unknown provider) is found at start: checks are off with one warning, and `run --yes` refuses to start. | During a pause, bypass mode asks about nothing. A server error or dropped connection that the command's text caused still pauses the check; the guide calls this a known gap. |
| A repository turns features on to send commands out, or off to remove the net | Project `decide` keys are ignored with a warning. Credentials and `DECIDE_PROVIDER` come only from the environment or a named `--env-file`. | None found. |
| Data sent to the provider (commands may hold secrets; injection check sends MCP results) | Off by default; opt-in names each feature; the guide lists what each sends. | A secret typed into a checked command goes to TypeSafe or Cloudflare. |
| Terminal injection through the answer line | Provider text is passed through `PrintableText`; probabilities are formatted by the CLI. | None found. |
| Tool-name spoofing (MCP tool named `bash`) | The check runs only for the workspace's own `bash`. | None found. |
| Unbounded consumption | Default mode: one request per prompted bash call. Bypass: one per bash call. Totals show in `/settings` and the footer. A 3-second deadline bounds wait. | No spend cap; tokens are cheap but unbounded. |
| Prompt injection still reaches the model | Warn, not withhold, by library decision; the warning tells the model to treat the text as data. | A strong injection can still steer the model; approvals still guard actions. |

## Failure modes

- **No credentials or an unknown provider:** found at start, with no
  request; one warning; status `decide: off (<reason>)`; every call runs
  as without decide, and nothing about decide shows on rows or footers.
  `run --yes` with `command_risk` on refuses to start.
- **Timeout, too-large input, or rejected request:** that call is
  unchecked and asks, in every mode that can ask; no pause.
- **Outage:** that call is unchecked; the library pauses the check for a
  minute; the status area counts down; bypass mode runs commands.
- **Check returns after the person answered:** discarded; the slot already
  holds "answered first".
- **`toolcheck` slips:** PR 1 waits; nothing ships half wired.

## Alternatives considered

- **Join the `CommandRisk` hook with the policy.** Its hold beats the
  person's `y` and stops the turn, so the user sees two prompts and a
  stopped turn. Rejected.
- **Check in a separate `BeforeTool` hook before the policy.** It runs for
  remembered and read-only-safe calls too, spending on calls nobody is
  asked about, and cannot put its answer in the policy's prompt. Rejected.
- **A new `workspace.Policy` option that calls a checker.** Moves CLI
  presentation (the async line) into the library for one host. Revisit if a
  second host wants the same prompt.

## Tradeoffs

- The record keeps the decide answer inside the policy hook's `Reason`,
  in a CLI-owned text format, rather than as its own `HookResult`. Reopen
  parses it. A format change breaks old marks; the golden test guards it.
- Bypass mode keeps the policy's existing reason, "approved by the user",
  for auto-approved calls. The decide suffix makes checked ones clear;
  fixing the base text is a separate issue.

## Rollout

Three PRs stacked on the PRD branch, each behind the off-by-default list.
PR 1 also changes the library helper and adds a `CHANGELOG.md` entry for
`--decide`, `DASHI_DECIDE`, and Cloudflare support in every decide
package. Evidence: a live transcript of UC-3 and a screenshot of each
prompt state (checking, answered, flagged, unchecked) and of the paused
status line.
