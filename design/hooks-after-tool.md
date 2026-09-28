# Hooks after tools, hiding, and state

**Status:** 2a (#55) and 2b in review; 2c planned (decision 31) · **Date:**
2026-09-28 · **Owner:** Curtis · **Builds on:** [hooks.md](hooks.md)

## Plan

- **2a (this PR):** `AfterTool`, which changes what the model sees of a tool
  result, plus `FailOpen` and a redactor recipe.
- **2b, after steering PR 1:** `ModelDecision.Hide`, which removes tools from
  one model call, and `tool.Join`.
- **2c, later:** hook state, with the recipe that needs it.

Terms: a *chain* is the hooks at one point, run in order. The *drain loop* is
the part of the tools loop that receives executor results. A *bundle* is a set
of hooks with one name at several points, shipped together by a package.

## 2a: AfterTool

```go
type ResultHook Hook[ResultPlan, ResultDecision] // Hooks.AfterTool

type ResultPlan struct {
    Call       tool.Call
    Definition tool.Definition
    Outcome    tool.Outcome   // what happened; authoritative, never changed
    Result     llm.ToolResult // what the model will see, after earlier hooks
    Turn       TurnView       // the state before this settlement
}

type ResultDecision struct {
    // Replace is what the model sees instead. The runner sets CallID. A
    // replacement equal to Result is no opinion.
    Replace *llm.ToolResult
}

// Hook gains FailOpen, allowed only for AfterTool and BeforeFinish.
func (o Output) MapText(f func(string) string) Output // package tool
```

A redactor that keeps images and returns no opinion when nothing matched.
`MapText` rewrites text blocks only; a strict redactor errors on blocks it
cannot inspect:

```go
redact := dashi.ResultHook{Name: "redact", Func: func(
    _ context.Context, p dashi.ResultPlan) (dashi.ResultDecision, error) {
    for _, block := range p.Result.Output.Content {
        if block.Kind != content.KindText && block.Kind != content.KindImage {
            return dashi.ResultDecision{}, errors.New("cannot inspect " +
                string(block.Kind)) // withholds the result
        }
    }
    r := p.Result
    r.Output = r.Output.MapText(func(s string) string {
        return secrets.ReplaceAllString(s, "[redacted]")
    })
    return dashi.ResultDecision{Replace: &r}, nil
}}
```

- **When it runs.** Once for each final outcome whose content came from a tool
  or the host: an executor's result (succeeded, failed, `tool.Fatal`, or an
  executor-returned `not_executed`), and a `Settle` result. Not for denied,
  invalid, unknown or submitted calls, calls settled with no outcome, waiting
  results, or steering's not-run outcomes: the runner wrote those.
- **Where.** In the drain loop, one result at a time, before the settlement
  is committed, so the settlement carries the hooks' answers. A slow hook
  delays draining, and so steering's urgent-steer check during a batch. In the
  `Settle` command, before its commit, including on an ended turn. A `Settle`
  with a tool result records `ActiveHooks` like `Start` and `Continue`; hosts
  pass the same hooks to it.
- **What it changes.** Only what the model sees: the next model call and the
  next turn's history through `Project`. The record, `Observer` and `Stream`
  events, and `ToolView.Outcome` keep the raw output. A redactor is not
  storage redaction; a host that must not store a secret removes it in the
  executor.
- **Failure.** Closed by default: the model sees "The tool ran, but its result
  was withheld: check "<name>" failed." (for a tool that returned
  `not_executed`, "The call finished without running, …"), not marked as an
  error so the model does not retry a call that took effect, with
  `WarningHookFailed`. The hook name is shown to the model. With `FailOpen`, the
  result so far stands and a warning is recorded. `BeforeFinish` with `FailOpen`
  treats an error as no opinion, with a warning. An invalid replacement is a
  failure.
- **Ended context.** If the run's context ends while the chain runs, the
  settlement is still committed with a warning, withheld unless every hook left
  to answer fails open; the outcome and its cost are kept. A `Settle` runs its
  hooks under the command's context, not the turn's deadline. A context that
  ended before the chain started keeps today's rule. `Project` never refuses a
  turn because of hooks.

**Recording.** `ToolFact.Result *llm.ToolResult` (JSON `result`) is set on
the settlement only when a hook changed it; projection uses it, and
`ToolView.Result()` returns what the model sees, derived by the same function
projection uses. `HookResults` gain the point `after_tool` and the verdicts
`replace` and `failed`. `ActiveHooks` gains `after_tool` and is recorded on a
`Settle` with a tool result.

**Tests.** Chains of replacements; an equal replacement as no opinion;
error, panic, invalid replacement, `FailOpen`; the ended context; parallel
calls presenting one at a time; `Settle` results, on a running and an ended
turn, with `ActiveHooks`; which outcomes run hooks; projection and
`ToolView.Result()` agreeing; replay of recorded results; `MapText` keeping
non-text blocks; and the redactor example run live.

## 2b and 2c

- **2b, `ModelDecision.Hide`:** hooks' lists union; names the call does not
  offer are ignored; the submit tool cannot be hidden (ignored with a
  warning); no effect on the reserved final-answer call. The hide clears a
  forced tool choice naming a hidden tool, or `any` with no tools left, with
  `WarningToolChoiceCleared`. Each hook's candidate input is validated, so an
  invalid answer skips that hook. The intent's `Tools` records what was
  offered; a call to a hidden bound tool settles as not run, "This tool is
  not available now.", without hooks. A hidden tool is not guaranteed
  unused, since `BeforeModel` fails open, and changing the offered tools
  resets the provider's prompt cache. Plus `tool.Join`, which fails `Tools`
  on a duplicate name.
- **2c, state:** recorded only when it changes, keyed by hook name and shared
  across a bundle's points, with a typed accessor.

## Risks

| Risk | Mitigation |
| --- | --- |
| A slow result hook holds up draining and steering | Documented; hooks should be fast |
| A redactor mistaken for storage redaction | Stated in the docs; redact in the executor to keep secrets out of the record |
| A withheld result confuses the model | The text says the tool ran; a warning names the hook |
