# Spend cost display

Status: Approved · Author: Codex · Date: 2026-09-29  
Workflow: Spec, checkpoint, then build with independent implementation review.

## Context

The [PRD](../prds/spend-string.md) defines a display method for issue #93.
Existing examples duplicate currency loops. One also handles incomplete costs manually.
`money.Amount.String()` supplies exact decimal text. `Spend` already contains the cost categories and uncertainty counters.
The implementation must display those facts without changing accounting.

## Proposal

Add `func (s Spend) String() string` in a focused `spend_string.go` file.
Use a value receiver so both Spend values and pointers implement `fmt.Stringer`.
The method reads its maps and counters. It must not modify or clone their contents.

A private helper formats one cost map. Use `slices.Sorted(maps.Keys(costs))` to obtain sorted currency keys.
Format each amount through `money.Amount.String()`. Join amounts with `, ` and sections with `; `.
The method assembles these sections in order:

1. `model: <amounts>` when `Cost` has entries.
2. `tools: <amounts>` when `ToolCost` has entries.
3. `partial: N` when `CostPartial` is positive.
4. `unknown: N` when `CostUnknown` is positive.

When both maps lack entries, use `no recorded cost` before any counters.
Keep explicit zero entries. Do not infer cost from token usage or currency names.
Prefix model amounts with `at least` when either counter is positive.
Prefix tool amounts with `at least` only when `CostUnknown` is positive.

`Spend.Add` can omit an overflowing sum from either map and increment `CostUnknown`.
That counter does not identify the affected category. Qualifying both categories avoids displaying an incomplete tool subtotal as complete.
`CostPartial` alone represents unpriced model requests, so it does not qualify tools.
Do not introduce fields or reconstruct the counter's cause.

Replace cost loops in `examples/tools/main.go` and `examples/hosted_tools/main.go` with `fmt.Printf("Cost: %s\n", spend)`.
The hosted example keeps its token, hosted-request, and unpriced-meter diagnostics. Remove duplicate unknown and partial counter prose.
The existing unpriced-meter diagnostic may use the counter to decide when to print details.
Update `docs/guides/budgets.md` with the method's purpose, category labels, missing-cost behavior, and uncertainty rules.

## Verification

Add focused same-package tests in `spend_string_test.go` and a public `ExampleSpend_String` in an external-package example file.
Cover nil and empty maps, explicit zeros, tool-only cost, multiple currencies, nanos, and signed Amount bounds.
Cover unknown-only, partial-only, overlapping counters, and both category qualification rules.
Construct an actual tool-cost overflow through `Spend.Add`, rather than setting its counter manually.
Assert retained exact amounts, the unknown flag, and lower-bound text on both categories.
Check repeated formatting remains stable and preserves Spend maps and usage.
The runnable example prints mixed model/tool charges and incomplete model costs without provider requests.

Run focused Spend tests and the example, then root `go vet ./...`, `go build ./...`, and `go test -race ./...`.
Verify changed Go files with `gofmt`. The implementation reviewer also runs the public example.

## Alternatives and tradeoffs

- Extend the delegate formatter. It prints only model cost and has incompatible empty/partial semantics. A focused method avoids changing delegate behavior.
- Reuse the CLI footer. It rounds money and omits missing charges. Exact library output needs a separate presentation.
- Print every Spend field. That preserves detail but defeats the short cost-summary job. Structured fields remain available.

The method allocates sorted key slices and output strings. Cost maps are small, and deterministic human display warrants that cost.
An unknown counter conservatively qualifies both categories, even if only one is incomplete. Existing accounting cannot distinguish them.
Default `%v` display changes through `fmt.Stringer`. JSON and stored records retain their existing forms.
The docs must state that this human-readable text is not a machine format.

## Rollout

Ship the additive method, guide, tests, and the two example updates together.
No migration, provider call, dependency, flag, or accounting change is required.
The CLI and delegate helper remain unchanged. No unresolved decision blocks the build.
