# Model catalog

Status: implemented, 2026-09-27. Ports Dive's catalog (65c393f) as decision 4
in [decisions](decisions.md) requires, so known models get documented limits
and prices without application setup.

## What went wrong in Dive

One Dive model lived in up to five places: `catalog.json`, generated
`models_gen.go` and `pricing_gen.go`, a capability table keyed by ID prefix,
and OpenRouter's copy. Adding Grok 4.7 touched all of them. The history shows
the cost:

- Prefix matching misclassified point releases (`gpt-5` matched `gpt-5.7`;
  `claude-opus-4` would match a future `claude-opus-4-9`), so each release
  needed a guard entry.
- Price rows were keyed separately from models, so a model could lose its price
  when an adapter list changed (GPT-6 Astra).
- An omitted rate meant free. Pro models got free cache reads, and Anthropic
  cache rates were derived by multipliers that were wrong for two models.
- Long-context cache writes had no field, so estimates understated them.

## A catalog entry

An entry is one exact model ID at one provider. It holds everything Dashi
knows about that ID, in Dashi's own types:

```go
type Entry struct {
    Provider     string            // catalog name, e.g. "anthropic"
    Model        string            // exact API model ID
    APIs         []string          // wire APIs that serve it
    Lifecycle    Lifecycle         // "", "preview", or "deprecated"
    Snapshot     string            // dated ID this alias served, if documented
    Note         string
    Capabilities llm.Capabilities
    Pricing      *llm.Pricing
    Source       string            // documentation URL
    QualifiedAt  time.Time
}
```

`Entry.Bind(api, endpoint)` returns an `llm.ModelInfo` whose evidence names the
model, that API and endpoint, owner `dashi/catalog`, the source URL, method
`documentation`, and the qualified date. The catalog never claims `live_probe`.
Only claims a provider page states are recorded. Dive's probe tables were not
carried over; each effort ladder and support value was checked against current
provider documentation, and `catalog/sources_test.go` names the page for every
one. A claim no page states is left unknown, so adjust sends it and records it
as unverified. The entry's `Source` is the page that documents most of it,
usually the models overview.

Decisions on the candidate gaps:

- **Context window**: added to `llm.Capabilities` as `ContextWindow`, because
  compaction and budget code need it and it is a known limit like
  `MaxOutputTokens`. Providers differ on whether output counts against it, so
  it is an upper bound on input. Dashi does not enforce it: it cannot count
  input tokens offline.
- **Kind**: not modeled. The catalog lists only models callable through
  `llm.Client`. Image, video, speech, and embedding models belong to their own
  operations, whose `ModelInfo` types differ.
- **Aliases**: every ID is its own complete entry; there is no prefix or date
  stripping. `Snapshot` records a documented alias target, and
  `Entry.Serves(served)` accepts it, so a caller can check `Result.Served`
  before trusting a price sheet.
- **Lifecycle**: `preview` and `deprecated` are recorded; retired IDs are left
  out rather than listed as unusable.
- **Default model**: one per provider, `catalog.Default(provider)`. Dive's
  recommended flags, CLI order, descriptions, and display names are CLI
  presentation and were dropped.
- **Feature names**: the JSON uses Dashi's `Feature` strings and `Support`
  values directly. A feature is claimed only where the source states it; the
  rest stay unknown, so adjust sends them and records them as unverified.
- **Knowledge cutoff**: not modeled; nothing in Dashi reads it.

## Storage and loading

One JSON file per provider in `catalog/`, embedded with `go:embed`. Each file
names its destinations (API and public endpoint), its default model, its
source URL, and its entries. Entry capabilities and pricing use the exact JSON
shape of `llm.Capabilities` and `llm.Pricing`, so strict pricing decoding
validates every sheet. Files are parsed once on first use; tests parse and
validate every entry, so a bad file fails CI, not a request.

There is no generator and no generated constants. The data was converted from
Dive once; later edits are made by hand in the JSON, one entry per model. Model
IDs are strings the application configures, and constants churned with every
release. JSON rather than Go tables because it is the same shape recorded in
`ModelFact` and `RequestSnapshot`, and a price change is a one-line diff.

## How clients use it

Evidence is scoped by model, API, and endpoint, and the catalog is keyed the
same way. `catalog.Lookup(api, endpoint, model)` returns bound `ModelInfo` only
when a catalog file documents that exact API and endpoint and the entry lists
that API. `providerhttp.Client.ModelInfo` and the Gemini client consult
`Config.Models` first, then `Lookup`. So:

- Public endpoints (Anthropic, OpenAI Responses and Chat Completions, Gemini
  API v1beta, xAI, Meta, Mistral) get catalog facts by default.
- `Config.Models` wins per model; an application overrides a wrong or stale
  entry by supplying its own.
- Custom endpoints, proxies, Vertex, and local servers get nothing by default,
  since the documentation describes the public API. An application can adopt
  an entry with `Entry.Bind(api, itsEndpoint)` and pass it in `Config.Models`.
- Policies are unchanged. Known limits fit under adjust with the existing
  warnings; a documented rejection with no substitute (a budget on a model that
  takes only effort) fails before dispatch instead of after it.

## Pricing

Every sheet states all four rates. Where the source publishes no cache rate,
the catalog states the input rate: no invented discount or surcharge, and an
upper bound for budget estimates. Long-context prices become one `PriceTier`
at Dive's inclusive threshold with a complete rate set, including cache writes
at the tier's rate. Gemini's per-modality input prices become complete
`Modality` rate sets. Sheets describe the default service tier; Anthropic fast
mode and OpenAI flex and priority prices are omitted because Dashi's clients do
not request them.

**Anthropic cache writes have two rates.** `cache_write` is the 5-minute
rate (1.25x input) and the optional `cache_write_1h` is the one-hour rate (2x
input). `Usage.CacheCreation1hInputTokens` reports the one-hour part of the
writes, and `Pricing.Estimate` prices it at the one-hour rate. A sheet without
`cache_write_1h` leaves one-hour writes unpriced, never free. Tests pin both
rates on every Anthropic sheet ([prompt caching](prompt-cache.md)).

The regional multiplier is not modeled. Evidence already names the endpoint,
and a Vertex endpoint names its location, so a regional price is simply a
different sheet for a different destination. Provider-reported cost still wins:
the catalog adds no automatic estimation, and `Result.Cost` remains
provider-reported. Callers estimate explicitly with `Pricing.Estimate` after
checking `Result.Served` with `Entry.Serves`.

## Wire changes

`llm.Capabilities` gains optional `context_window`; existing records decode
unchanged. No field was renamed or removed. For known models on public
endpoints, `RequestSnapshot.ModelInfo` and recorded model facts now carry the
catalog's `ModelInfo` where they were previously absent.

## Left out

- Providers: Ollama (a local server serves whatever was pulled), OpenRouter and
  DeepInfra (route-dependent parameters; OpenRouter reports cost, and Dive's
  prices there were stale). Retired IDs, legacy Mistral `open-*` models, and
  Gemini 1.5 were dropped.
- Media, speech, and embedding models and their per-image, per-second, or
  embedding prices; Dive's provider feature flags (beta headers).
- Speed and service-tier variant sheets, and scheduled price changes, which
  are left as entry notes.
- Deprecation warnings at preparation, and effort-to-budget translation from
  catalog data (gap E, direction 4).
