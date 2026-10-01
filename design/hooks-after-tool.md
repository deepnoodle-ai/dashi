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
    Turn       TurnView       // the state before this result is recorded
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
  executor-returned `not_executed`), and a `Resolve` result. Not for denied,
  invalid, unknown or submitted calls, calls finished with no outcome, waiting
  results, or steering's not-run outcomes: the runner wrote those.
- **Where.** In the drain loop, one result at a time, before the finish step
  is committed, so the finish step carries the hooks' decisions. A slow hook
  delays draining, and so steering's urgent-steer check during a batch. In the
  `Resolve` command, before its commit, including on an ended turn. A `Resolve`
  with a tool result records `ActiveHooks` like `Start` and `Continue`; hosts
  pass the same hooks to it.
- **What it changes.** Only what the model sees: the next model call and the
  next turn's history through `TurnMessagesFull`. The record, `Observer` and
  `Stream` events, and `ToolView.Outcome` keep the raw output. A redactor is not
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
  finish step is still committed with a warning, withheld unless every hook left
  to decide fails open; the outcome and its cost are kept. A `Resolve` runs its
  hooks under the command's context, not the turn's deadline. A context that
  ended before the chain started keeps today's rule. `TurnMessagesFull` never
  refuses a turn because of hooks.

**Recording.** `ToolFact.Result *llm.ToolResult` (JSON `result`) is set on
the finish step only when a hook changed it; `TurnMessagesFull` uses it, and
`ToolView.Result()` returns what the model sees, derived by the same function
`TurnMessagesFull` uses. `HookResults` gain the point `after_tool` and the verdicts
`replace` and `failed`. `ActiveHooks` gains `after_tool` and is recorded on a
`Resolve` with a tool result.

**Tests.** Chains of replacements; an equal replacement as no opinion;
error, panic, invalid replacement, `FailOpen`; the ended context; parallel
calls presenting one at a time; `Resolve` results, on a running and an ended
turn, with `ActiveHooks`; which outcomes run hooks; `TurnMessagesFull` and
`ToolView.Result()` agreeing; `Turn.View` of recorded results; `MapText` keeping
non-text blocks; and the redactor example run live.

## 2b and 2c

- **2b, `ModelDecision.Hide`:** hooks' lists union and must match exactly;
  names no tool of the call has (with a dynamic toolset, also a tool absent
  from this call) and the submit tool are ignored with one
  `WarningHideIgnored` per call; no effect on the reserved final-reply call.
  A forced tool choice naming a tool any hook so far hid, or a forced choice
  with no tools left, is cleared with `WarningToolChoiceCleared`, whatever the
  hooks' order. With no tools left, the tool choice and `ParallelTools` are
  cleared too, as providers reject them without tools. Each hook's candidate
  input is validated on top of earlier hooks, so an invalid decision skips that
  hook, hides included. The `model_call_started` step records the offered
  `Tools` and the `Hidden` names; a call to a hidden tool finishes as not run, "This tool is
  not available now.", by that record, without hooks, and never reaches
  `BeforeTool`. `BeforeModel` fails open, so a hide is not a guarantee: fail
  closed by handling errors in the hook, or pair it with a `BeforeTool` deny.
  Changing the offered tools resets the whole cached prompt. Plus
  `tool.Join`, which fails `Tools` on a duplicate name and names the set.
- **2c, state:** recorded only when it changes, keyed by hook name and shared
  across a bundle's points, with a typed accessor.

## Risks

| Risk | Mitigation |
| --- | --- |
| A slow result hook holds up draining and steering | Documented; hooks should be fast |
| A redactor mistaken for storage redaction | Stated in the docs; redact in the executor to keep secrets out of the record |
| A withheld result confuses the model | The text says the tool ran; a warning names the hook |
