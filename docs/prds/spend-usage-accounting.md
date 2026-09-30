# Keep failed usage aggregation separate from uncertain cost

Status: Approved\
PRD PR: #123  Implementation PR: #126  Updated: 2026-09-30

## Problem and users

Application developers need accurate spend reports and safe limits after loading a recorded turn.
[#116](https://github.com/deepnoodle-ai/dashi/issues/116) identifies a concrete failure: `Spend.Add` marks usage overflow as `CostUnknown` despite exact monetary sums.
`Spend.String()` consequently labels those amounts `at least`.
Replay instead rejects cumulative overflow, which can prevent recording an individually valid settlement.

The primary job is to distinguish incomplete usage from uncertain money without reconstructing accounting causes in application code.

## Context

`llm.Usage.Add` validates both inputs and rejects an overflowing sum without changing its receiver.
`Spend.Add` independently accumulates model and tool money.
Turn records store settlement facts, not Spend snapshots. `Replay` derives Spend, and delegation combines it with `Spend.Add`.
The [budgets guide](../guides/budgets.md) already distinguishes estimated usage, partial model charges, and local tool charges outside `MaxCost`.

[Pydantic AI](https://pydantic.dev/docs/ai/api/pydantic-ai/usage/) separates usage accumulation from limit checks.
The [OpenAI Agents SDK](https://openai.github.io/openai-agents-python/usage/) preserves independent usage snapshots through checkpoints and aggregation.
Dive's unchecked addition supplies no overflow signal. Dashi must retain its stronger validation and durable accounting boundary.

## Concepts

- **`Spend.UsageIncomplete`:** a boolean indicating incomplete usage because derivation overflowed or aggregation omitted a contribution after addition failed.
  The retained Usage is not a complete total. This differs from `Estimated`, which identifies calls with incomplete provider reports.
- **`CostUnknown`:** existing conservative monetary uncertainty, including missing charges, incompatible cap currencies, and monetary overflow.
  Older snapshots can also contain unidentified usage failures. Their positive counters remain conservative.
- **`CostPartial`:** existing model charges with unpriced hosted requests. It keeps its current meaning and can overlap `CostUnknown`.

## Use cases and acceptance

### UC-1: Combine spend without losing exact money

A developer calls `combined := first.Add(second)` and inspects its fields or prints it.

- [ ] Two individually valid usage values whose sum overflows produce `UsageIncomplete == true`.
- [ ] Failure retains the receiver's complete prior Usage, including maps and flags. Neither input changes.
- [ ] Both monetary categories still accumulate independently. Usage-only failure does not increase `CostUnknown` or `CostPartial`.
- [ ] Existing incomplete usage propagates through `Add` and `Clone`. Adding zero never clears the flag.
- [ ] Request-count and modality-count overflow receive the same distinction as token overflow.
- [ ] Actual model or tool monetary overflow retains the known amount and marks monetary uncertainty without wrapping.

### UC-2: Store, reopen, and report

A developer serializes Spend or records valid settlements, reopens storage, and calls `Replay(turn)` or `delegate.Report`.

- [ ] New Spend JSON preserves `usage_incomplete`. Missing or false values keep the existing zero-value behavior.
- [ ] Old JSON with positive `cost_unknown` retains that counter. No reader subtracts, relabels, or infers its cause.
- [ ] Memory and file recorders preserve individually valid settlement facts when their cumulative usage cannot fit.
- [ ] Replay exposes incomplete usage and all representable model/tool amounts instead of rejecting solely for cumulative overflow.
- [ ] Overflow while enriching valid reported usage with inferred hosted-request counts retains the settlement and independent charges.
  For `Requests[llm.MeterUnreported] == math.MaxInt`, `RequestsFinal == false`, and a declared hosted tool, replay marks usage incomplete.
  Enrichment discards failed mutations and retains the original reported Usage as a partial contribution. Both recorders preserve the raw fact.
  Invalid recorded usage remains invalid.
- [ ] Per-attempt pricing never consumes incomplete derived usage. Without an independent charge, derivation overflow keeps monetary cost unknown.
- [ ] Monetary aggregation overflow stays conservative during replay. Invalid individual usage, charges, and record structure remain invalid.
- [ ] Delegated reports carry the flag through child and inclusive totals. Repeated loading and reporting produce the same meanings.
- [ ] `Spend.String()` appends `usage incomplete` when the flag is true. This flag alone does not qualify money as `at least`.
- [ ] Existing partial/unknown labels remain conservative. CLI and delegation summaries disclose incomplete usage when reporting affected totals.

For example, usage-only overflow with USD 0.75 model cost and USD 5 tool cost prints:
`model: 0.75 USD; tools: 5 USD; usage incomplete`.

### UC-3: Enforce the affected limit

A developer runs or continues a turn whose recorded usage aggregation is incomplete.

- [ ] An active output-token or total-token cap prevents another model dispatch. Retained counts cannot prove remaining allowance.
- [ ] The recorded `limited` stop uses the corresponding token `LimitKind`, explains incomplete usage aggregation, and sets `Stop.Terminal == true`.
- [ ] The ended view has `ReadyToContinue == false` and `Next() == NextNone`, including after storage and replay.
- [ ] `Continue`, including a budget raise, returns `ErrTurnEnded`. Raising a cap alone cannot repair incomplete usage.
- [ ] Before that stop is recorded, valid reconciliation can restore complete usage and ordinary admission if every applicable limit permits execution.
- [ ] After a crash before stopping, `Continue` can recover recorded work or record the terminal token stop, but cannot dispatch new work while usage remains incomplete.
- [ ] Permitted late settlements can update reported Spend, including clearing `UsageIncomplete`, but never revive the ended turn.
  Its recorded terminal stop, fixed answer, `NextNone`, and rejection of `Continue` remain unchanged.
- [ ] Guidance tells the host to resolve outstanding effects as required, then start a new turn carrying prior history.
  It never directs the host to resume the ended turn.
- [ ] With only `MaxCost`, usage incompleteness alone does not prevent admission when recorded charges and the next monetary bound are exact.
- [ ] Unknown, partial, incompatible-currency, and overflowing costs preserve conservative cost-cap behavior.
- [ ] Uncapped execution can proceed while retaining the visible flag. Price selection retains its current rules for complete derived usage.

## Requirements and decisions

- Add one boolean to Spend. Developers need a completeness signal, not a count of failed calls. Reject another ambiguous failure counter.
- Keep aggregation atomic for Usage and independent for money. Reject saturation presented as an exact usage total.
- Derive turn completeness from authoritative facts. Reject a second durable ledger or manual repair flag.
  Before a terminal token stop, reconciliation can restore complete usage. A budget raise cannot change accounting facts.
- End a turn when incomplete usage prevents token-cap enforcement. Late settlements may update accounting but cannot revive execution.
  Preserve `NextNone` and existing ended-turn rules. Reject a resumable stop whose later accounting changes could revive an ended turn.
- Preserve ambiguous `CostUnknown` values unchanged. Reject retroactive guesses that could make old amounts appear exact.
- Deliberately broaden replay to accept individually valid facts with overflowing derivations or aggregates.
  Reject dropping valid settlements because derived counts or totals cannot fit.
  Preserve recorded stop validity, terminal-state rules, nondispatch evidence, and reconciliation safeguards.
- Update public comments, the budgets guide, affected report guidance, and the formatter's historical overflow description.
  Add a provider-free `Example` showing overflow, exact money, and the public flag.

## Scope, risk, and success

No currency conversion, pricing redesign, billing ledger, per-bucket completeness, or count of affected calls belongs in this track.
Do not rename existing Outcome or Stop types, change `llm.Usage.Add`, or relax validation of malformed facts.
Older binaries can ignore the additive JSON field. They cannot be trusted to interpret new incomplete-usage snapshots correctly.
Support new readers of old records. Do not promise old-reader budget safety for new accounting states.

The [technical spec](../design/spend-usage-accounting.md) defines this terminal-stop contract, admission timing, reconciliation, and legacy-record compatibility.
Focused overflow and round-trip tests must meet every criterion without provider requests.
Existing budget, recovery, replay, and delegation checks must pass. Independent implementation and billing-security reviews remain required.
