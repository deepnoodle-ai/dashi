# E. Provider coverage, cost, and budgets

Source review of Kernel main at 9d28264 on 2026-09-27; findings are directions,
not decisions.

**Names:** written before [decision 30](../decisions.md); read `Definition` as
`Agent`, `Engine`/`Bindings` as `Runner`, `Accept` as `Settle`, and a command's
`ID` as `CommandID`.

Kernel keeps Dive's four wire implementations and the same thin wrappers:
Messages (with Ollama), Responses (with Grok and Meta), Chat Completions (with
OpenRouter, Mistral, DeepInfra), and Gemini/Vertex. It is ahead of Dive on
hosted tools, tool choice, error metadata, and replay safety. It falls behind on
model catalog and prices, prompt caching, native structured output, and
automatic retry. Its cost facts are honest, but nothing prices a call, sums turn
cost, or declares a budget; `Policy.BeforeSend` and recorded `Admission` are the
only hooks, so the application supplies the estimate, the ledger, and the stop
semantics. nvoken already built these on Dive, and Kernel should absorb the
reusable half of that design.

## Provider feature matrix

S = Supported, P = Partial, M = Missing, – = not applicable. The Kernel columns
are routes, and wrappers inherit from them: **Msg** = anthropic/messages
(+ollama), **Resp** = openai/responses (+grok, meta), **CC** =
openai/completions (+openrouter, mistral, deepinfra), **Gem** = google/gemini.
The Dive columns are the equivalent packages. Kernel cells are grounded in the
files cited in the evidence column. Dive cells come from a delegated read of
Dive's code.

| Behavior | K Msg | K Resp | K CC | K Gem | D Anth | D OAI-R | D CC | D Goog | Kernel evidence / note |
|---|---|---|---|---|---|---|---|---|---|
| Streaming | S | S | S | S | S | S | S | S | `providers/*/*/stream.go`; `llm/stream.go:27` |
| Tool calling | S | S | S | S | S | S | S | S | `messages.go:~185`, `responses.go:~185` |
| Parallel-tools control | S | S | S | M (rejected) | S | S | S | M | `messages.go:~220` disable_parallel_tool_use; Gem rejects `gemini.go:~219` |
| Tool choice auto/none/any/named | S | S | S | S | S | S | S | M | `llm/llm.go:216-228`; Gem `gemini.go:~255-271` (**Kernel ahead**) |
| Structured output / JSON schema (native) | M | M | M | M | M | S | P | M | Only through the turn-level `kernel_submit_output` tool (`output_contract.go`); not available in `llm.Generate`. **Regression on Responses** |
| Reasoning effort | S (adaptive) | S | S | M (rejected) | S | S | S | S (level) | Msg `messages.go:~150-175`; Gem effort rejected `gemini.go:~211` |
| Reasoning budget | S | M (rejected) | M | S | S | – | – | S | `llm/llm.go:306-312`; Resp rejects budget `responses.go:139` |
| Reasoning replay (signatures / encrypted) | S | S | P (OpenRouter `reasoning_details`) | S (thought sigs) | S | S | P | S | `Content.Replay` `llm/llm.go:137`; Resp `include reasoning.encrypted_content` `responses.go:~121`; `internal/providercontent/gemini_replay.go` |
| Prompt-cache controls (breakpoints, TTL, cache key) | M | M | M | M | S (auto, 1h) | P (key) | P (key) | M | No `cache_control` or `prompt_cache_key` anywhere in `providers/` (**major regression**) |
| Cache-token usage | P (no 5m/1h split) | S | S | S | S | S | S | P | `llm/usage.go:93-150`; `gemini/wire.go:75` |
| Citations (round-trip) | S | S | M | M (rejected) | S | P | M | M | Msg `messages.go` encodeData; Resp `responses.go:264-280`; Gem `gemini.go:484` |
| Image / PDF inputs | S | S | P (no docs on DeepInfra) | S | S | S | S | S | `content/content.go`; `deepinfra.go` Prepare |
| File IDs / URIs (reference) | S | S | P (no image file ID) | S (fileData) | S | S | S | P | `messages.go:626`; `completions.go:352`; `gemini.go:518`; origin-scoped (`provider-content-replay.md`) |
| Files API upload | M | M | M | M | M | M | M | M | Neither library has one |
| Server tools: web search | S | S (+grok, meta) | M | S (google_search) | S | P | M | M | `anthropic/.../hosted_tools.go:16`; `responses/hosted_tools.go:23`; `gemini/hosted_tools.go:14` |
| Server tools: code exec / fetch / file search / image gen | S (code, web_fetch) | S (code, file_search, image_gen; grok x_search, collections) | M | S (code, url_context, file_search) | P (+computer use) | P | M | M | Computer use missing in Kernel (Dive had it) |
| MCP passthrough | P (no auth) | P (no auth headers) | M | M | S | S | M | M | `MCPToolsetOptions{Name,URL}` `hosted_tools.go:163`; `responses/hosted_tools.go:82-99` |
| Hosted approval flow | M | S | – | – | M | P | – | – | `llm/hosted.go:96-120` |
| Batch API | M | M | M | M | M | M | M | M | – |
| Stop sequences, top_p, top_k, seed | M | M | M | M | M | M | M | M | `llm.Settings` has MaxTokens, Temperature, Reasoning, ToolChoice, ParallelTools only (`llm/llm.go:327-345`) |
| Temperature | S (rejects >1) | S | S | S | S (drops) | S | S | S | Kernel rejects out-of-range values; Dive drops them with a warning |
| System / developer roles | S | S | S (configurable) | S | S | S | S | S | `llm/llm.go:26-39`; `openrouter.go` SystemRole |
| Usage detail (cached, reasoning, modality) | S | S | S | S | S | S | S | S | `llm/usage.go:39-83`. Server-tool request counts are missing in both |
| Service tier / speed (request) | M | M | M | M | S (speed) | S | S | S | Kernel records only the reported `Result.ServiceTier` (`llm/llm.go:476`) |
| Provider-reported cost | – | – | S (OR, DI) | – | – | – | S | – | `completions.go:469-488` |
| Retry-After / rate-limit headers | P (parsed) | P | P | P | S | S | S | P | `internal/providerhttp/errors.go:143`; `x-ratelimit-*` ignored |
| Automatic retry | M | M | M | M | S | S | S | S | `Client` sends once (`llm/llm.go:522-546`); the engine has no retry loop |
| Error taxonomy | S | S | S | S | M | M | M | M | `llm/llm.go:548-613`: category, disposition, retryable (**Kernel ahead**). No context-length category |
| Model catalog + pricing tables | M | M | M | M | S | S | S | S | Application must fill `Config.Models`; `ModelInfo.Pricing` is supplied by the application (`llm/model.go:148-158`) |

## Consistency findings

1. **Structured output is a turn feature, not a model-call feature.**
   `OutputSchema` only works through a synthetic submission tool
   (`output_contract.go`, `SubmitOutputTool`). A direct `llm.Generate` caller
   cannot ask for JSON-schema output. Native modes are also never used,
   including OpenAI `text.format`, which Dive already supported, Anthropic's
   structured output, and Gemini `responseSchema`. *Direction:* add a portable
   `Settings.Output` (schema) that Prepare encodes natively when capabilities
   allow, with tool emulation as a recorded Adjustment.
2. **Provider-specific controls have no outlet.** The only per-provider surface
   is hosted-tool constructors. Stop sequences, top_p, seed, service tier, cache
   keys, and cache breakpoints cannot be expressed at all. Dive at least had
   `Speed`, `ServiceTier`, `Features`, and `RequestHeaders`. *Direction:* add
   typed portable fields for the common controls (stop, top_p, seed, service
   tier, cache policy). Also add a per-route typed options value that Prepare
   validates and records in `RequestSnapshot`, rather than an untyped "extra
   body".
3. **Strict model validation fails even the simplest call by default.**
   `FeatureText` is always requested (`llm/model.go:206-209`), and
   `SupportUnknown` is rejected under the default `UnknownReject`
   (`llm/model.go:112-125`). As a result, a client without a `Models` entry
   rejects a plain text call. Dive routed and priced ~180 models from generated
   catalogs. *Direction:* ship per-provider generated catalogs whose evidence
   method is `documentation`. Keep strictness for claims that go beyond the
   catalog, and make "catalog or reject" the default.
4. **Reasoning settings do not carry across providers.** Effort works on
   Messages, Responses, and CC but is rejected on Gemini. A budget works on
   Messages and Gemini but is rejected on Responses. The rejections are correct,
   but an application that switches models has to branch on provider.
   *Direction:* use the existing `Adjustment` mechanism to translate between
   effort and budget from capability data, and record the translation. Clamping
   should stay forbidden.
5. **`StopRefused` has two meanings.** A policy denial such as a budget uses
   `StopRefused` with a free-text reason (`engine.go:559`), and so does a model
   refusal (`engine.go:681`). `StopLimited` reasons are also untyped strings
   (`engine.go:408-685`). A UI cannot reliably tell "out of budget" apart from
   "model declined". *Direction:* add typed limit and denial reasons, or a
   `Stop.Limit` field.
6. **Retries are left to the caller, but the metadata is better.** `llm.Error`
   carries `Retryable`, `RetryAfter`, and `Disposition` (`llm/llm.go:576-613`),
   which is more than Dive had. However, neither `Send` nor the engine retries,
   whereas Dive retried and honored Retry-After. *Direction:* add an engine
   retry policy. Count each retry as a model attempt, authorize each one through
   `BeforeSend`, and keep uncertain spend, as recs1 describes.
7. **Kernel is better in several places.** `Prepare` freezes the exact body
   along with its Adjustments and ContentChanges (`llm/request.go:18-48`).
   Replay across models is explicit and origin-scoped
   (`design/provider-content-replay.md`). Hosted tool activity and approvals are
   typed (`llm/hosted.go`). Gemini gets tool choice, and it gets hosted tools
   that Dive lacked.

## Cost and budget model

**Dive:** Each provider registers a global list-price registry keyed by model
name. `PopulateCost` fills `Usage.Cost` on every call. A turn sums usage across
calls, and `Outcome.UsageUnknown` marks calls that never reported usage. There
are no spending limits. The only built-in limits are iteration limits and hook
aborts.

**nvoken:** A turn has `Limits` (`max_output_tokens`, `max_estimated_cost_usd`,
`max_iterations`, timeouts). An agent definition can also carry limits, but a
request may only narrow them, and an installation sets ceilings over both. Each
call reserves a worst-case max-cost-at-risk against a tenant credit account and
records a `model_call_facts` row (prepared, then started, then settled or
uncertain). After usage arrives, the turn is checked against its limits at
checkpoints between steps. A breach produces a typed reason
(`max_estimated_cost`, `insufficient_credits`, and so on). The turn then either
ends as incomplete or enters `budget_hold`, from which it can be resumed with a
raised limit. An uncertain call is charged the full hold. An unpriced model
fails closed whenever a cost ceiling or credits apply.

**mobius-cloud:** A per-turn cost meter checks spend before each iteration, with
no reservation, and may overshoot by one call.

**Kernel:** Kernel has sound pieces, but nothing that enforces a budget. A
caller receives:
- `Usage` and `Cost` with a typed `CostSource` and nil-means-unknown semantics
  (`llm/pricing.go:246-280`);
- a pure `Pricing.Estimate` (`llm/pricing.go:198`), which no code in the repo
  calls;
- `TurnView.Usage`, a token total (`state.go:115-121`);
- `Policy.BeforeSend`, which can deny a call, plus `Admission` JSON that is
  recorded atomically with the model intent (`policy.go`; `engine.go:554-566`);
- deterministic attempt IDs (`engine.go:564`).

| Need | Dive | nvoken | Kernel status | Evidence | Direction |
|---|---|---|---|---|---|
| Normalized per-call usage | Yes | Via Dive | **Covered** | `llm/usage.go:39-150` | Split cache writes into 5m and 1h. Add server-tool request counts |
| Per-call cost filled automatically | List price from registry | Catalog + Dive | **Partial**: provider-reported only (CC) | `completions.go:469`; `Estimate` has no caller | Let the application optionally configure a qualified price sheet per client, and record the sheet on the ModelFact |
| Price catalog | Generated tables | Own catalog, checked against Dive | **Missing** | `ModelInfo.Pricing` is supplied by the application | Ship a versioned catalog next to the model catalog |
| Turn cost total | Summed `Usage.Cost` | Summed on the turn | **Missing** (tokens only) | `TurnView` has `Usage` but no `Cost` (`state.go:32-44`) | Add `TurnView.Cost` per currency, with a completeness flag |
| Unknown usage in the total | `UsageUnknown` | Charged the full hold | **Partial**: per-call nil and `StopUncertain`, but calls with nil usage are dropped from the sum | `state.go:115-121`; `engine.go:385` | Add unknown and uncertain attempt counts to `TurnView` |
| Budget declared | None | Turn, agent (narrow only), install ceiling | **Missing** | `Limits` has attempts, pauses, concurrency, deadline (`turn.go:35-43`) | Add `Limits.MaxOutputTokens` and `MaxCost`. Allow Start to narrow the Definition's limits |
| Pre-call check / reservation | – | Worst-case at-risk hold | **Partial**: hook plus atomic Admission, no estimate | `policy.go` BeforeSend; `design/turn.md:88-94` | Add `llm.MaxCostAtRisk(Request, Pricing)` (input estimate plus max output) |
| Post-usage check between steps | Hooks only | `checkpointBudgetExceeded` | **Partial**: `BeforeModel` can stop the turn, but has no built-in check | `policy.go` ModelDecision.Stop | Check the new limits in the engine before each attempt, using recorded usage and cost |
| Budget-stop representation | Hook abort error | Typed reasons and statuses | **Partial**: `StopRefused` or `StopLimited` with a string reason | `engine.go:559,681`; `turn.go:244-258` | Typed limit reasons. Keep the answer produced alongside the limit, as reserved final-answer already does |
| Resume after raising a budget | – | `resume` endpoint, credit-resume worker | **Partial**: a refused turn can continue, but `model_attempts` is terminal and a definition revision cannot change mid-turn | `state.go:603-609` | Add a `RaiseLimits` command that is recorded, validated as monotonic, and continues the turn |
| Tool and hosted-tool cost | None | Counts only | **Missing** | `HostedUse` has no metering | Record hosted request counts in `Usage`. Let local tools report cost in `tool.Outcome` |
| Child / subagent roll-up | – | – (mobius `addChild`) | **Missing** | – | Add a parent reference to the turn view so a host can aggregate child turns |
| Billing identity per attempt | Invented keys | `model_call_facts` IDs | **Covered** | `engine.go:564`; `ModelFact.Admission` | Keep as is. This is what recs2 asked for |
| Tenant ledgers, credits, markups | App | App | **Correctly the app's job** | Opaque `Admission`; Recorder transaction | Document the recipe (reserve in the Recorder, settle on the result) |

## Top gaps

1. **No catalog: strict by default, priced never.** Every application must write
   evidence-bearing capability and price entries before its first call succeeds,
   and cost stays nil unless it calls `Estimate` itself. This is the biggest
   regression from Dive. Direction: a versioned, documentation-evidenced catalog
   per route, with price sheets qualified by served model and tier.
2. **No budget in the Turn.** `Limits` cannot express a token or cost ceiling, a
   Definition cannot set a default that a turn narrows, and nothing estimates
   the maximum cost at risk. nvoken shows the complete shape (declared limit,
   reservation, check between steps, typed reason, resumable hold). Kernel has
   most of the plumbing; it lacks the declarative part, a standard estimator,
   and a `RaiseLimits` path.
3. **Turn-level cost and unknown usage are not tracked.** An application cannot
   state "this turn cost at least $X, plus 1 call whose cost is unknown" without
   replaying the steps itself. Direction: `TurnView.Cost` (per currency, with a
   completeness flag) and `UnknownUsageAttempts`.
4. **Prompt caching is missing.** Caching is the largest single cost lever for
   agent loops, so this undercuts both stated goals. Direction: a portable cache
   policy in Settings, with breakpoints and TTL recorded in the prepared body,
   and usage that splits 5m and 1h cache writes.
5. **Common provider controls are missing, with no typed escape hatch**
   (consistency findings 1, 2, and 6, plus Gemini thinking levels, authenticated
   MCP, and computer use). Each missing control pushes applications back to raw
   HTTP. Direction: portable fields plus per-route typed options that Prepare
   validates and records.

## Already strong

- **Honest cost facts.** A nil `Cost` or `Usage` means unknown, and `CostSource`
  separates a charge from an estimate. `BreakdownUnavailable` exists, and legacy
  records are rejected rather than read as zero (`design/pricing-proposal.md`).
- **Enforcement hooks are correct.** `BeforeSend` sees the exact prepared
  request, and `Admission` is committed atomically with the model intent.
  Uncertain attempts are never replayed blindly. This matches the lifecycle that
  recs1 asks for.
- **Settings are explicit.** Prepare records Adjustments and ContentChanges, and
  settings are never silently clamped or dropped.
- **Errors are richer than Dive's.** They carry category, disposition, and
  Retry-After.
- **Hosted tools cover more than Dive did.** Gemini gains search, code
  execution, URL context, and file search. Responses gains file search, code
  interpreter, image generation, and MCP with approvals. Grok gains X search and
  collections.
- **Replay across models is explicit and scoped.** File IDs and citations carry
  an origin (`design/provider-content-replay.md`).
- **The reserved final answer** yields a useful answer together with a `limited`
  stop.
