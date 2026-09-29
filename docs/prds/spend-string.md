# Print a readable cost summary from Spend

Status: Approved  
PRD PR: Pending · Implementation PR: Pending · Updated: 2026-09-29

## Problem

Developers print turn costs in logs and examples. Issue [#93](https://github.com/deepnoodle-ai/dashi/issues/93) requests `Spend.String()` to remove repeated currency loops.
The tools example prints only model amounts. The hosted-tools example separately prints unknown and partial charges.
Map iteration gives no stable currency order. A convenience method must preserve the uncertainty that these examples expose.

## Context and users

The primary user is an application developer who wants `fmt.Println(out.Spend)` to show recorded costs accurately.
`money.Amount.String()` already prints exact decimal amounts. The delegate formatter sorts currencies but hides partial charges and unknown-only spend.
The CLI footer rounds amounts and marks lower bounds. Its compact presentation serves a different job and stays unchanged.

`Spend.Cost` contains model charges, including priced hosted requests. `Spend.ToolCost` contains local tool charges outside `MaxCost`.
`CostPartial` counts calls with unpriced hosted requests. `CostUnknown` also includes charges incompatible with a cap and aggregation overflow.
These counters can overlap. They must retain separate labels and must never become a combined count of calls.

## Use cases and acceptance

### UC-1: Print known charges

The developer calls `spend.String()` or passes `spend` to `fmt.Print`.

- [ ] Each amount uses exact `money.Amount.String()` text and its recorded currency.
- [ ] Model costs appear before tool costs. Currencies sort lexically within each section.
- [ ] Both cost maps stay separate, including when they contain the same currency.
- [ ] Printing never converts currencies, rounds values, or changes the Spend.

### UC-2: Print incomplete charges

The developer prints spend with unknown or partial costs.

- [ ] Positive `CostPartial` and `CostUnknown` values appear independently as `partial: N` and `unknown: N`.
- [ ] Known model amounts have the prefix `at least` when either counter is positive.
- [ ] Known tool amounts have the prefix `at least` when `CostUnknown` is positive. `CostPartial` alone does not qualify tool amounts.
- [ ] A real `Spend.Add` tool-cost overflow qualifies both present cost sections and retains the unknown counter.
- [ ] Unknown-only and partial-only spend remain explicit without an invented amount or currency.
- [ ] Hosted request details remain available through fields and the hosted-tools example's separate diagnostics.

### UC-3: Print empty and zero spend

- [ ] Nil and empty cost maps produce `no recorded cost` when both counters are zero.
- [ ] An explicit zero retains its cost section and currency, such as `model: 0 USD`.
- [ ] Token usage alone does not invent a monetary zero. The summary remains `no recorded cost`.

## Output contract

Join nonempty sections with `; `. Omit absent model and tool sections.
When neither cost map has entries, start with `no recorded cost`. Append partial, then unknown counters when positive.
Amounts within a cost section use `, ` separators.

| Recorded spend | String |
| --- | --- |
| Zero value | `no recorded cost` |
| Model USD zero | `model: 0 USD` |
| Model USD 0.000000001 | `model: 0.000000001 USD` |
| Model USD 1 and EUR 0.5 | `model: 0.5 EUR, 1 USD` |
| Model USD 0.5 and local tool USD 2 | `model: 0.5 USD; tools: 2 USD` |
| Unknown counter 1, no known amount | `no recorded cost; unknown: 1` |
| Partial counter 1, no known amount | `no recorded cost; partial: 1` |
| Model USD 0.5, partial 1, unknown 1 | `model: at least 0.5 USD; partial: 1; unknown: 1` |
| Model USD 0.5, tools USD 2, partial 1 | `model: at least 0.5 USD; tools: 2 USD; partial: 1` |
| Model USD 0.5, tools USD 2, unknown 1 | `model: at least 0.5 USD; tools: at least 2 USD; unknown: 1` |

The counters describe recorded accounting conditions. `unknown: N` does not assert that exactly N model calls lack any known price.
`CostUnknown` does not identify the affected category. Qualify both present cost sections because aggregation can omit either model or tool charges.
`CostPartial` alone identifies model charges, so it qualifies only that section.

An actual tool overflow supplies the acceptance case:

```go
spend := dashi.Spend{
    Cost: map[string]money.Amount{"USD": money.MustParseAmount("0.5")},
    ToolCost: map[string]money.Amount{
        "USD": money.MustParseAmount("9223372036.854775807"),
    },
}.Add(dashi.Spend{ToolCost: map[string]money.Amount{"USD": money.Amount(1)}})
fmt.Println(spend)
// model: at least 0.5 USD; tools: at least 9223372036.854775807 USD; unknown: 1
```

Usage totals, estimated usage counts, and unpriced meter details are outside this cost summary.
The text serves human display. JSON and Spend fields remain the interfaces for storage and machine processing.

## Scope and success

Add the value-receiver method `func (Spend) String() string`, its public documentation, focused tests, and `ExampleSpend_String`.
Update `examples/tools` and `examples/hosted_tools` to print Spend directly. Retain token output and hosted-request diagnostics.
Add usage guidance to the budgets guide. Success means these examples need no cost-map loops and disclose incomplete costs.
Root formatter, vet, build, and race tests must pass. The runnable example must require no provider request.

Accounting, budget enforcement, JSON, records, the CLI footer, and the delegate formatter are outside this change.

## Decisions

- Use a cost summary with named model and tool sections. Developers need the existing billing boundary. Rejected: merge both amounts into one total.
- Print exact decimals and recorded currencies. Small model charges need nano precision. Rejected: dollar symbols, fixed precision, or conversion.
- Say `no recorded cost` for absent amounts. Absence cannot prove free execution. Rejected: `0`, `free`, or `no cost`.
- Preserve separate partial and unknown counters. Their meanings overlap. Rejected: total unpriced calls or missing-cost guesses.
- Qualify both cost sections for unknown accounting. The counter carries no category. Rejected: attribute every unknown condition to model cost.
- Exclude usage and meter details. Existing fields support detailed reports. Rejected: a full Spend dump in every log message.

## Risk

Implementing `fmt.Stringer` changes default `%v` display for Spend. Document this deliberate change and keep serialized fields unchanged.
No open question blocks implementation.
