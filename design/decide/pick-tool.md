# Pick tool

**Status:** Proposed · **Date:** 2026-10-02 · **Owner:** Curtis · **Builds
on:** [README](README.md), Decide's `Pick` and `patterns/pick`

An agent often has to name one thing from a list the host owns: email the
right Sam among 40 contacts, refund the order the customer means among their
12. Today the model reads the list and types an ID, and it can invent or
mistype one. A pick tool lets the model say what it wants in words. A decision
model chooses among the host's items, and the tool returns the item or says
nothing fits.

Terms: a *decision model* (Jev, Clef) answers typed questions with
probabilities. A *Choice* question picks one option from a list; Decide's
`Pick` adds an *abstain* option, "none of these". The *margin* is the winner's
probability minus the runner-up's. A *ref* is a short code the tool gives the
model in place of the item's ID. `Outcome.Data` is structured detail that the
turn record keeps for the host and the model never sees.

## What the model sees

One tool, `pick`, with two arguments: `list` (an enum of the host's list
names; the tool description gives each list's description) and `looking_for`
("Describe the one item you need, with every detail from the conversation that
tells it apart"). The result has one of three statuses:

```json
{"status": "picked", "ref": "pick_3f9a2c1d", "item": {"name": "Sam Lee", "team": "Design"},
 "probability": 0.91, "runner_up": {"item": {"name": "Sam Ortiz"}, "probability": 0.05}}
{"status": "unsure", "candidates": [{"item": {...}, "probability": 0.48}, {"item": {...}, "probability": 0.41}],
 "note": "Ask the user which one they mean, then pick again with their answer."}
{"status": "no_match", "note": "Nothing in contacts fits. Ask the user."}
```

**Decisions.**

- **One tool, lists by name.** Its definition is fixed when the tool is built,
  so the cached prompt survives list contents changing. One tool per list
  would add a definition per list.
- **The model never sees the whole list.** It sees the winner and the
  runner-up, which saves context on every later model call.
- **Only `picked` carries a ref.** `unsure` shows the top two candidates without
  refs, so the model cannot act on a guess; it must ask the user and pick
  again. `no_match` is Decide's abstain winning.

## Where items come from

The host registers typed lists. `Items` runs inside the tool call, so its
`ctx` carries the host's own values (user, tenant) and `dashi.CurrentTurn`, and
contents can depend on the user or the turn. It also receives `looking_for`,
so a host with 5,000 contacts can pre-filter with its own search first.

A Choice takes at most 255 options (Jev 1.13; Clef's documented 2 to 255), so
`Pick` accepts 254 items plus abstain. Over that, the tool runs a two-round
tournament: groups of 254 in parallel, then a final pick among the group
winners; thresholds apply to the final round. The record marks `rounds: 2`,
since a final-round probability compares finalists only. Over 254 × 254 items,
the call fails. An empty list is `no_match` with no request.

## Refs, records, and approvals

The ref is `pick_` plus 8 hex digits of a hash of the turn ID and call ID, so
a rerun after a crash makes the same ref. `Outcome.Data` records the list,
`looking_for`, status, the item's host key, the ref, top probabilities, the
resolved model version, the request ID, and token usage. An action tool takes
the ref and calls `picktool.Resolve`, which reads the current turn's record
through `dashi.CurrentTurn`, checks the list name, and returns the host key.

**Decision: refs are worth it.** Checking a typed ID against the host's data
catches an invented ID but not a wrong valid one (order 1042 for 1024). A ref
proves the item came out of a pick of that list in this turn. Recovery works
because `Resolve` reads only the record and never calls Decide again: a refund
that waited a day for approval resolves the same order.

Limits, by choice: a ref works only in the turn that made it; a ref from an
earlier turn fails, and the model picks again, which is cheap and uses fresh
data. A hook or result limit that changes the pick result drops its `Data`
(the tool guide's rule), so its ref no longer resolves.

**A pick never authorizes.** The action tool still passes `BeforeTool` hooks
and `RequireApproval`. The approval binds to the arguments, and the ref's
meaning is fixed in the record, so approving the ref approves exactly that
item. An approval screen calls `picktool.Lookup(view, ref)` to show the person
the item, not the code.

## Thresholds and failure

`MinP` and `MinMargin` belong to the host (README rule 4). Below either, the
result is `unsure`. Zero means no minimum: the top candidate is picked unless
abstain wins. That is Decide's plain answer, with no claim of accuracy.

**Decision: a pick fails closed.** It feeds actions, and a wrong item sends a
wrong email. When Decide fails, or the answer is inconsistent, the model gets
an ordinary failed result, "Could not pick from contacts. Ask the user to name
it exactly.", never `tool.Fatal`, and never a guessed item. The turn goes on,
so rule 3 holds.

## Go API

The package is `picktool` in the `decide/` sub-module. It avoids the name
`pick`, which Decide's pattern package already uses.

```go
package picktool // github.com/deepnoodle-ai/dashi/decide/picktool

type List[T any] struct {
    Name        string // "contacts"; shown to the model
    Description string // "People the user emails."
    Items       func(ctx context.Context, lookingFor string) ([]T, error)
    Key         func(T) string // host ID; recorded, never sent to a model
    Describe    func(T) any    // what Decide and the model see
}

type Config struct {
    Name      string  // tool name; "" is "pick"
    MinP      float64 // 0: no minimum
    MinMargin float64 // 0: never unsure
    Price     func(model string, u decide.Usage) *tool.Cost // nil: no cost
}

func New(client *decide.Client, cfg Config, lists ...Source) (tool.Tool, error)
func Resolve(ctx context.Context, list, ref string) (Picked, error)
func Lookup(view dashi.TurnView, ref string) (Picked, bool)

type Picked struct {
    List, Key, Ref   string
    P, Margin        float64
    Model, RequestID string
}
```

`List[T]` implements `Source`, so lists of different types share one tool.
`Config{}` works. `New` fails on a nil client, no lists, or a bad or repeated
list name, so errors surface at startup. The tool is marked `tool.ReadOnly()`,
so a crashed pick is rerun. A host adds it like any tool:
`runner.Tools = tool.List{sendEmail, pick}`. Inside, it uses
`patterns/pick` with positional keys (`c1`, `c2`, ...), so host IDs never
reach either model.

**Spend** belongs to the turn that called the tool. Token usage is always in
`Data`. With `Price` set, it becomes `Outcome.Cost`, which already appears in
`Spend.ToolCost` and, like all tool cost, does not count against `MaxCost`.

**Tests and example** need no keys: `dashitest` scripts the model's calls and
`decidetest.ChoiceFor` answers by position. Cases: each status, both
thresholds, the abstain winner, a tournament, Decide failures, rerun after a
crash, `Resolve` and `Lookup` on a recorded turn, a ref from another list or
turn. The example picks "the Sam from the design review" and sends an email
that waits for approval.

## Alternatives

- **The model types the ID.** This is the failure we are fixing, and the list
  costs context on every call.
- **An enum of IDs in the action tool's schema.** A per-turn enum changes the
  tool definition and resets the cached prompt, and it stops invented IDs but
  not wrong ones. It gives no probability and no "none".
- **A search tool that returns matches with refs.** The main model still
  judges, at full cost, and with no abstain probability. It suits open-ended
  lookups; `Items` is where a host's search fits here.
- **A `BeforeTool` hook that rewrites a fuzzy name to an ID.** The model never
  learns which Sam was chosen, so it cannot tell the user or handle "unsure".

## Relationship to the judgment tool

Both send a host's items to Decide from a tool call, and both record evidence
in `Data` and report spend through `Cost`. They differ in what the model gets
back. The judgment tool asks many typed questions and returns counts and flags.
The pick tool asks one Choice across the items and returns one ref. **My
view:** two tools in one package, sharing `List[T]`, so a host registers
"contacts" once and gets both. That goes against README rule 1's "one package
per design". This doc says `picktool` until the two designs agree.

## Open questions

1. **Fail closed, or fall back?** When Decide is down, should the tool instead
   return the top candidates with refs and let the model choose, as it would
   have without Decide? I chose to refuse and have the model ask the user.
2. **Should Decide see the user's words?** It sees only the model's
   `looking_for`, which can paraphrase the user wrongly. Tools cannot read the
   user's message today (a PR #174 friction note).
3. **Tournament or error over 254 items?** A tournament works by default but
   costs a request per 254 items, and its probabilities are weaker. Requiring
   the host to pre-filter is simpler.
4. **Refs across turns.** In chat, "yes, email her" comes in the next turn.
   Picking again is cheap, but a session-wide lookup would remove the round
   trip.
5. **One package with the judgment tool?** See above.
