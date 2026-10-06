# Decision models in Dashi

**Status:** Proposed · **Date:** 2026-10-02 · **Owner:** Curtis · **Builds
on:** [`examples/decide`](../../examples/decide) (PR #174)

[Decide](https://github.com/deepnoodle-ai/decide) is our Go client for
decision models: TypeSafe's Jev and Cloudflare's Clef. A decision model
answers typed questions (yes-or-no, choice, score) about a piece of text,
with probabilities, in well under a second and for a small fraction of an LLM
call. It writes no prose.

PR #174 used Decide inside hooks. These designs use it in five more places:

| Design | Package | Where Decide sits | What it buys |
| --- | --- | --- | --- |
| [List tools](list-tools.md) | `listtool` | Two tools the agent calls | `judge_items` hands bulk judgments (300 tickets) to Decide; `pick_item` chooses an item from a host's list |
| [Context ranking](context-ranking.md) | `contextrank` | A hook before the turn's first model call | The most relevant notes and files go into the turn, under a size limit |
| [Selective compaction](selective-compaction.md) | `selective` | A `session.Compactor` | Old messages still needed stay word for word; only the rest is summarized |
| [Turn watcher](turn-watcher.md) | `turnwatch` | A hook before each model call | A repeated failing call triggers a check, and Decide confirms before the agent gets a reminder |
| [Tool checks](../../docs/design/decide-tool-gate.md) | `toolcheck` | Hooks before a shell call and after a tool result | A likely disaster is held for approval; a likely prompt injection reaches the model with a warning |

## Words

A *decision model* answers typed questions about a text, its *state*, with
probabilities. An *item* is one entry in a host's *list*. A *segment* is a run
of old messages that compaction keeps or drops as a whole. These docs do not
use step (a recorded `dashi.Step`), notice (an agent message), evidence
(model capability evidence), worker (the process running a turn), or report
(`tool.Report`) for anything else.

## Shared decisions

1. **Packages under `decide/`, in the root module.** One package for each
   thing a host configures; none is in the root package `dashi`. The root
   module needs Go 1.27 because Decide does. Go builds only the packages a
   host imports, and Decide's library adds no third-party dependencies, so
   hosts that never use a decision model pay nothing for them.
2. **Model advice never grants permission.** It may hold, deny, hide, or
   warn. It never approves a call or raises a budget.
3. **Tools return errors to the model; hooks and compactors fail open.** A
   tool that cannot get an answer returns an ordinary failed result and never
   guesses. A hook or compactor does what Dashi would do without it, and the
   failure is recorded as a warning. Nothing fails a turn that would succeed
   without Decide.

   *Exception, for hostile input:* `toolcheck.CommandRisk` fails open only
   on an outage. When its check fails in a way the command can cause (a
   timeout, a rate limit, a command too long to check, a rejected request,
   or an invalid answer), its hook holds the call for approval, since an
   attacker could otherwise shape a command to skip the check. A hold is
   advice, which decision 2 allows.
4. **Zero means the most likely answer wins.** No package ships a tuned
   threshold. A zero floor means "the model leans yes": P(yes) above 0.5.
   A pick takes the top choice; a match on a choice needs P above 0.5.
   A negative value turns a floor off. Hosts tune their own
   with Decide's `calibrate` and pin a version such as `jev-1.13.0` with
   `decide.WithModel`, because `jev-latest` can move.

   *Exception, with evidence:* `toolcheck` ships the thresholds of the
   decide plugin. `CommandRisk` flags `severe` at 0.8: on decide's set of
   151 commands, it scored 12 of 13 disasters at 0.85 or more and every
   other command at 0.63 or less (decide PR #54). `PromptInjection` flags
   at 0.6, decide's default flag, above the unsure band of 40 to 60
   percent. A threshold of 0.5 would sit inside that band. See
   [the PRD](../../docs/prds/decide-tool-gate.md).
5. **Spend.** Tools report Decide spend as `tool.Outcome.Cost`, which Dashi
   adds to `Spend.ToolCost`, outside `Budget.MaxCost`. Hooks and compactors
   pass usage to their `On…` callback.
6. **The record says what Decide changed.** A hook decision records a one-line
   `Reason` (probabilities and Decide request ID) and the decision model's
   version in `RulesVersion`. A tool keeps full answers in `Outcome.Data`.
   `On…` callbacks receive every probability.
7. **A nil `Client` means `decide.NewClient()` from the environment.** If that
   fails, a tool constructor returns the error, and a hook or compactor acts
   as it would without Decide.
8. **Keyless tests and examples** use `decidetest` for Decide and `dashitest`
   for the model, as in PR #174.
9. **Copied template text tracks decide.** `toolcheck/questions.go` copies
   question text from decide's built-in templates, which decide does not
   export. When Dashi upgrades decide, compare that text with the new
   templates and copy any change.

## Dashi changes

All small; none puts a Decide type in the root package.

- `Reason` and `RulesVersion` on `ModelDecision` and `ResultDecision`, recorded
  like `ToolDecision`'s.
- `TurnView.Input`: the turn's recorded input, so a tool or hook can read the
  user's request.
- `session.Compactor.Start` takes a `context.Context` and runs outside the
  session lock, so a slow compactor never blocks `Steer` or `Enqueue`.

## Open questions

- Should tool cost count toward `Budget.MaxCost`? Not yet: it changes what a
  budget means.
- Should hooks and compactors get a way to add spend to the turn? Wait for a
  host that needs it.
- Should Decide carry a price table, so hosts need no `Price` function? Yes,
  later, in Decide.
