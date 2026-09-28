# Pricing and cost

Status: implemented foundation contract, 2026-09-27. Price catalogs and live
provider qualification remain outside this change.

## Boundary

`Usage` measures tokens. `Cost` values one model attempt in a currency. A price
sheet supplies rates for an estimate; it is neither measured usage nor proof of
what a provider billed. Keep these facts separate before turn records depend on
them.

Before this change, `Usage` held cost and a flag that could erase it during JSON
normalization. `Pricing` mixed rates, two threshold conventions, fallback rules,
and a regional multiplier. `Cost.Add` could sum different currencies. Kernel
still has no production price catalog or automatic list-price calculation.

## Scope

This contract prices tokens for `llm` calls.
[Media and speech](media-and-speech-proposal.md) operations may bill by
characters, seconds, or images. Keep their usage task-specific until a second
operation demonstrates a shared metering shape; keep `llm.Usage` specific. A
shared accounting contract need not live in the `model` package.

A list-price estimate requires all billable quantities and applicable rates.
A provider's reported total can still be retained with its source identified,
even when Kernel cannot independently reproduce it.

## Contract

```go
// All rates are per million tokens; zero literally means free.
type Rates struct {
    Input      money.Amount
    Output     money.Amount
    CacheRead  money.Amount
    CacheWrite money.Amount
}

// One complete rate sheet; the caller qualifies where it applies.
type Pricing struct {
    Currency  string
    UpdatedAt string
    Reference string
    Base      Rates
    Tiers     []PriceTier
    Modality  map[string]Rates
}

// The highest threshold at or below total input selects a complete rate set.
type PriceTier struct {
    MinInputTokens int
    Rates          Rates
}
```

`Reference` identifies where rates came from, such as a price page or catalog
version; `CostSource` instead classifies how a resulting amount was obtained.
Use JSON tags in the implementation. `Modality` entries are complete rate sets,
not partial overrides: a zero rate must never mean “inherit.” Modality rates
do not vary by input tier until an actual price sheet needs both dimensions. One
inclusive `MinInputTokens` rule replaces the two existing threshold mechanisms.
Validate currency, date, finite nonnegative rates, and strictly increasing
positive thresholds when accepting a sheet. `Pricing.Clone` must own its slice
and map.

`Pricing.Estimate(*Usage) (Cost, bool)` is a pure calculation. `false` means the
call cannot be priced faithfully, including nil usage, missing required token
breakdowns, or unattributed tokens when modality rates differ. It never turns
missing usage into a known zero cost. An estimate from a sheet has a typed
list-price source. Amounts are exact nano-units, not floats; see decision 28
in [decisions.md](decisions.md) for the representation and rounding rule.

`Result.Cost` is a nullable per-call amount beside `Usage`; nil means unknown.
Typed `CostSource` distinguishes provider-reported charges from provider
estimates and list-price calculations. A provider may report only a total, so
`Cost.BreakdownUnavailable` distinguishes unknown category amounts from zero.
`Result.Served` records the reported model. `llm` does not aggregate cost across
attempts; that requires currency and completeness checks in the layer that
owns the attempts. `Usage.Add` may still sum token counts.

Serving tier and speed describe a response, not token counts. `Result` records
`ServiceTier` where an adapter reports it; add `Speed` when one reports that
fact. Select a variant sheet only if its tier is known or the default is
guaranteed by the configured destination. Streaming usage deltas remain token
measurements; cost belongs in the final `Result`.

## Selecting a price

`ModelInfo.Pricing` may carry one application-supplied sheet for a configured
model and destination. It is an available estimate, not an instruction to
automatically price every response. A caller or configured client must select a
sheet qualified for the **served** model, endpoint, region, speed, and tier
before estimating. If it cannot establish that match, `Result.Cost` stays nil.
Provider-reported cost takes precedence. Adapters do not silently fill a missing
provider value with a list-price estimate; callers may explicitly estimate with
a qualified sheet. To explain an estimate later, the attempt record must retain
both the chosen sheet and its qualification: served model, API, endpoint, and
any region, account, speed, or tier that affects the rate. This does not need a
new public selection type yet.

Do not port Dive's process-global price registry, regional multiplier, or
catalog generator yet. A generated catalog can produce complete sheets when
Kernel has a consumer for it. Anthropic's mixed 5-minute and 1-hour cache
writes require separate usage buckets before Kernel can estimate their cost;
using the 5-minute rate for all writes would be misleading.

## Implemented boundary

The flat fields became `Rates`, `PriceTier`, and `Pricing.Estimate`. Tests cover
literal zero, tier boundaries, incomplete detail, and tiered modality pricing:
above the threshold, modality tokens retain their rates while other tokens use
the tier rates. Chat Completions clients put reported amounts on `Result.Cost`
in both transport modes and retain a reported service tier on `Result`.
`Usage` holds token measurements and quality flags; automatic list estimates,
cost aggregation, and catalog selection were not added.

These changes alter stored `ModelInfo.Pricing`, `Usage`, and `Result` JSON.
Prototype records need explicit migration. Decoding rejects legacy flat pricing
fields instead of treating a missing `base` as free, and requires an explicit
cost total. Legacy cost and serving fields in `Usage` and obsolete `Cost.Model`
also fail decoding so stored amounts cannot disappear. No alias reclassifies a
provider estimate as a charge. Fixture tests establish arithmetic and adapter
behavior, not the accuracy of vendor prices.
