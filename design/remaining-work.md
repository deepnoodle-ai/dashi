# Roadmap status

Status snapshot: 2026-09-28, through PR #68. Hooks, live delivery, and
steering parts 1–3 have landed; see their [design](hooks.md),
[design](live-delivery.md), and [PRD](steering.md). This inventory covers other
gaps against Dive and the temporary framework survey in
`tmp/research/comparison/README.md`.
The older [gap reports](gap-analysis.md) describe discovery-time code. A
delivered first slice does not imply every provider route is qualified.

**Size** estimates implementation scope: S = focused change, M = package or
adapter, L = several packages or a new layer. **Difficulty** estimates design
and qualification uncertainty, independently of size. An existing reference is
not an approved design unless stated. Items are grouped for reading, not ranked.

## Model and provider behavior

### 1. Native structured output for model calls

**Status:** [PR #64](https://github.com/deepnoodle-ai/dashi/pull/64) added
strict-schema output to direct OpenAI Responses calls; turns still use
`dashi_submit_output`. Live provider acceptance is unverified. **Size:** M.
**Difficulty:** Medium. **Reference:** [structured output](structured-output.md)
describes the turn contract; [provider gaps](gaps/E-providers-cost-budgets.md)
compare Dive. **Next:** Run a dated OpenAI live probe, then qualify other
provider routes without changing the turn's recorded output fact.

**Packet:** [source research](roadmap-research/01-native-structured-output-research.md) · [PRD](roadmap-research/01-native-structured-output-prd.md).

### 2. Generation controls and provider options

**Status:** `llm.Settings` covers tokens, temperature, reasoning, tool choice,
parallel tools, and citations; stop sequences, `top_p`, seed, service tier, and
typed provider options are absent. **Size:** L. **Difficulty:** High.
**Reference:** [provider gaps](gaps/E-providers-cost-budgets.md) and
[model controls](model-controls.md); no dedicated proposal for these fields.
**Next:** Choose portable fields versus route-specific options, then implement
and record their effective values in prepared requests.

**Packet:** [source research](roadmap-research/02-generation-controls-research.md) · [draft PRD](roadmap-research/02-generation-controls-prd.md).

### 3. Prompt-cache controls

**Status:** Dashi measures and prices cache usage but cannot request
breakpoints, TTL, or cache keys. **Size:** M. **Difficulty:** High.
**Reference:** [provider gaps](gaps/E-providers-cost-budgets.md) and
[model catalog](model-catalog.md); no cache-control design. **Next:** Design
provider-specific cache placement and 5-minute versus 1-hour accounting
together; test that later-turn reminders do not defeat the reusable prefix.

**Packet:** [source research](roadmap-research/03-prompt-cache-research.md) · [draft PRD](roadmap-research/03-prompt-cache-prd.md).

### 4. Provider dialect conformance

**Status:** [PR #60](https://github.com/deepnoodle-ai/dashi/pull/60) added a
Chat Completions wire corpus and cross-route replay checks. All-pairs replay
and other compatible dialects remain incomplete. **Size:** M.
**Difficulty:** Medium. **Reference:** [adjacent-library findings](gaps/C-adjacent-libraries.md)
and [provider content replay](provider-content-replay.md). **Next:** Extend
the corpus to other routes and real provider captures; keep fixture results
separate from live qualification.

**Packet:** [source research](roadmap-research/04-provider-dialect-research.md) · [PRD](roadmap-research/04-provider-dialect-prd.md).

### 5. Live provider qualification

**Status:** Selected Anthropic, OpenAI, and Gemini routes passed dated live
checks; many hosted routes, Vertex, and the Gemini error probes remain
unqualified. **Size:** M. **Difficulty:** High because external services vary.
**Reference:** [live-test record](../docs/dev/live-tests.md) and
[hosted-tool plan](2026-09-27-resources-and-hosted-tools.md). **Next:** Maintain
an exact route/model/feature matrix and run the missing paid probes when keys
are available; label fixture-only support separately.

**Packet:** [source research](roadmap-research/05-live-qualification-research.md) · [draft PRD](roadmap-research/05-live-qualification-prd.md).

## Tools and integrations

### 6. Local MCP client

**Status:** Provider-hosted MCP works without authentication; Dashi cannot
connect to a local or app-managed MCP server and expose its tools. **Size:** M.
**Difficulty:** High. **Reference:** [example-port gaps](gaps/F-example-ports.md)
and Dive's `experimental/mcp`; no Dashi design. **Next:** Specify discovery,
name stability, credential scope, tool results, and transport ownership around
the existing `tool.Toolset` boundary. Use `BeforeTool` hooks for approval.

**Packet:** [source research](roadmap-research/06-local-mcp-research.md) · [draft PRD](roadmap-research/06-local-mcp-prd.md).

### 7. Authenticated provider-hosted MCP

**Status:** Disabled because exact request snapshots could persist credentials.
**Size:** M. **Difficulty:** High. **Reference:** [resource and hosted-tool
plan](2026-09-27-resources-and-hosted-tools.md), especially its credential
boundary. **Next:** Separate exact in-memory dispatch bytes from a redacted,
verifiable recordable snapshot before adding auth headers or OAuth flows.

**Packet:** [source research](roadmap-research/07-hosted-mcp-auth-research.md) · [draft PRD](roadmap-research/07-hosted-mcp-auth-prd.md).

### 8. Speech transport

**Status:** [PR #63](https://github.com/deepnoodle-ai/dashi/pull/63) added
prepared batch transcription and synthesis for two named OpenAI models.
Streaming, other providers, and paid live qualification remain open. **Size:** M.
**Difficulty:** Medium. **Reference:** [media and speech proposal](media-and-speech-proposal.md)
and [batch guide](../docs/guides/batch-speech.md). **Next:** Run the opt-in
live probes, then design streaming and qualify additional providers.

**Packet:** [source research](roadmap-research/08-speech-transport-research.md) · [PRD](roadmap-research/08-speech-transport-prd.md).

### 9. Nullable typed-tool arguments

**Status:** Resolved by [PR #57](https://github.com/deepnoodle-ai/dashi/pull/57):
pointer nullability uses draft 2020-12-compatible schema and agrees with
Go decoding. **Size:** S. **Difficulty:** Low. **Reference:**
[example-port follow-ups](gaps/F-example-ports.md).

**Packet:** [source research](roadmap-research/09-nullable-tool-args-research.md) · [PRD](roadmap-research/09-nullable-tool-args-prd.md).

### 10. Tolerance of extra tool arguments

**Status:** Resolved by [PR #66](https://github.com/deepnoodle-ai/dashi/pull/66):
typed tools ignore extra keys by default, with `WithStrictArguments` for a
closed schema. Typed decoding matches recorded last-key JSON meaning. **Size:**
S. **Difficulty:** Medium. **Reference:** [adjacent-library findings](gaps/C-adjacent-libraries.md)
and [recorded-value decision 27](decisions.md).

**Packet:** [source research](roadmap-research/10-extra-tool-args-research.md) · [PRD](roadmap-research/10-extra-tool-args-prd.md).

### 11. Tool-result size and projection

**Status:** A tool can return an unbounded result that is sent on later model
calls. **Size:** M. **Difficulty:** High. **Reference:** [adjacent-library
findings](gaps/C-adjacent-libraries.md) and [design pitfalls](design-pitfalls.md);
no approved design. **Next:** Define a recorded, visible truncation or
reference rule that preserves the original result for audit.

**Packet:** [source research](roadmap-research/11-tool-result-projection-research.md) · [draft PRD](roadmap-research/11-tool-result-projection-prd.md).

### 12. Skills

**Status:** [PR #59](https://github.com/deepnoodle-ai/dashi/pull/59) added an
optional in-memory skill catalog and loader, outside the turn core. **Size:** M.
**Difficulty:** Medium. **Reference:** [example-port gaps](gaps/F-example-ports.md),
Dive's skill guide, and [functional areas](functional-areas.md). **Next:**
Qualify with an application consumer before expanding the package.

**Packet:** [source research](roadmap-research/12-skills-research.md) · [PRD](roadmap-research/12-skills-prd.md).

### 13. Child turns and delegation

**Status:** A tool can start another Runner, but Dashi has no parent/child
identity, budget roll-up, or lifecycle contract. **Size:** L.
**Difficulty:** High. **Reference:** [functional areas](functional-areas.md) and
[provider/budget gaps](gaps/E-providers-cost-budgets.md); no Dashi proposal.
**Next:** Design parentage, accounting, cancellation, and recovery using one
real application flow before adding a general orchestration API.

**Packet:** [source research](roadmap-research/13-child-turns-research.md) · [draft PRD](roadmap-research/13-child-turns-prd.md).

## Context and record health

### 14. Session continuity

**Status:** Each Turn receives prior history from the application; Dashi owns
no cross-turn Session. This is an explicit open question, not missing Turn
persistence. **Size:** L. **Difficulty:** High. **Reference:** [decision 19](decisions.md)
and [history storage](history-storage.md). **Next:** Decide whether an optional
Session layer earns its storage and concurrency rules against real consumers.

**Packet:** [source research](roadmap-research/14-session-continuity-research.md) · [draft PRD](roadmap-research/14-session-continuity-prd.md).

### 15. Context compaction

**Status:** No built-in compaction or overflow-recovery policy. **Size:** L.
**Difficulty:** High. **Reference:** [history storage](history-storage.md),
[runtime reminders](runtime-reminders.md), and Dive's compaction package; no
Dashi proposal. **Next:** First settle Session ownership, then define a
versioned summary with coverage, provenance, and replay behavior.

**Packet:** [source research](roadmap-research/15-context-compaction-research.md) · [draft PRD](roadmap-research/15-context-compaction-prd.md).

### 16. Turn-owned attachment references

**Status:** Prior history is no longer copied into each Turn, but bytes in a
Turn's own input or tool result still enter its record. **Size:** M.
**Difficulty:** High. **Reference:** [decision 19](decisions.md) and
[history storage](history-storage.md). **Next:** Design an application-owned
reference and resolver with digest, lifetime, and authorization recheck at
Prepare; never fetch a private resource implicitly.

**Packet:** [source research](roadmap-research/16-attachment-references-research.md) · [draft PRD](roadmap-research/16-attachment-references-prd.md).

### 17. Incremental projection

**Status:** `Project` and replay rebuild state from the first step; there is no
cached projection for long turns. **Size:** M. **Difficulty:** Medium.
**Reference:** [adjacent-library findings](gaps/C-adjacent-libraries.md) and
[design principles](design-principles.md); no implementation design.
**Next:** Measure long tool-heavy turns, then cache within one run without
making the cache authoritative.

**Packet:** [source research](roadmap-research/17-incremental-projection-research.md) · [draft PRD](roadmap-research/17-incremental-projection-prd.md).

### 18. Inspecting newer record versions

**Status:** An unknown step kind prevents `Replay` and `Project` on that Turn.
**Size:** M. **Difficulty:** High. **Reference:** [Turn record](turn.md),
[protocol gaps](gaps/D-protocols-streaming.md), and [adjacent-library
findings](gaps/C-adjacent-libraries.md); no approved design. **Next:** Separate
inspect-only reading from execution replay; block continuation on unknown
semantics while retaining the facts.

**Packet:** [source research](roadmap-research/18-newer-record-versions-research.md) · [draft PRD](roadmap-research/18-newer-record-versions-prd.md).

## Recovery, limits, and accounting

### 19. Durable retries after proven rejection

**Status:** Typed failure disposition and retry hints exist; a proven 429
rejection stops the run. A generic 503 remains dispatch-uncertain even when
retryable. **Size:** M. **Difficulty:** High. **Reference:**
[adjacent-library findings](gaps/C-adjacent-libraries.md), [provider gaps](gaps/E-providers-cost-budgets.md),
and [interruption design](turn-interruption.md); no retry PRD. **Next:** Design
a recorded retry wait that counts attempts and rechecks budget and hooks.
Uncertain dispatch must still require reconciliation.

**Packet:** [source research](roadmap-research/19-durable-retries-research.md) · [draft PRD](roadmap-research/19-durable-retries-prd.md).

### 20. Connection and idle-stream timeouts

**Status:** [PR #61](https://github.com/deepnoodle-ai/dashi/pull/61) added
opt-in response-start, stream-idle, and total-call timeouts for supported HTTP
model clients. Gemini and media clients remain outside this slice. **Size:** M.
**Difficulty:** Medium.
**Reference:** [adjacent-library findings](gaps/C-adjacent-libraries.md) and
[interruption design](turn-interruption.md). **Next:** Qualify other transports
only with their own timeout and interruption semantics.

**Packet:** [source research](roadmap-research/20-stream-timeouts-research.md) · [PRD](roadmap-research/20-stream-timeouts-prd.md).

### 21. Hosted tools under a cost cap

**Status:** A call declaring unbounded provider-hosted work stops before
dispatch under `MaxCost`; reporting its settled partial cost works. **Size:** M.
**Difficulty:** High. **Reference:** [hosted-tool pricing](hosted-tool-pricing.md)
and [finding 11.2](gaps/F-example-ports.md). **Next:** Qualify a provider-enforced
pass bound and its full rebilling behavior before admitting that route under a
hard cap. Other routes may remain excluded.

**Packet:** [source research](roadmap-research/21-hosted-tools-cost-cap-research.md) · [draft PRD](roadmap-research/21-hosted-tools-cost-cap-prd.md).

### 22. Remaining provider price gaps

**Status:** [PR #58](https://github.com/deepnoodle-ai/dashi/pull/58) made the
provider example label partial/unknown estimates accurately. OpenAI hosted
image-generation image tokens remain unpriced; other models and tiers need
ongoing evidence checks. **Size:** S per route. **Difficulty:** Medium.
**Reference:** [example-port follow-ups](gaps/F-example-ports.md),
[model catalog](model-catalog.md), and [pricing contract](pricing-proposal.md).
**Next:** Add a rate only when billed units and served-model evidence are
qualified; otherwise preserve partial cost.

**Packet:** [source research](roadmap-research/22-provider-price-gaps-research.md) · [first-slice PRD](roadmap-research/22-provider-price-gaps-prd.md).

## Protocol surfaces

### 23. ACP adapter

**Status:** Dashi has no ACP server or client adapter. **Size:** L.
**Difficulty:** High. **Reference:** [protocol gaps](gaps/D-protocols-streaming.md)
and [Dive ACP research](https://github.com/deepnoodle-ai/dive/blob/65c393f221f1/docs/acpresearch/README.md); no Dashi proposal.
**Next:** Use committed-step `Follow` for live delivery, then design separate
v1 and v2 projections from the same Turn facts; qualify reconnect and approval
semantics with a real client.

**Packet:** [source research](roadmap-research/23-acp-adapter-research.md) · [draft PRD](roadmap-research/23-acp-adapter-prd.md).

### 24. A2A and OpenTelemetry adapters

**Status:** Dive ships optional modules; Dashi has neither. **Size:** M each.
**Difficulty:** Medium. **Reference:** [example-port gaps](gaps/F-example-ports.md),
Dive's `a2a` and `otel` modules; no Dashi proposal. **Next:** Choose a real host
for each adapter and preserve the Turn record as the source of execution facts.

**Packet:** [source research](roadmap-research/24-a2a-otel-adapters-research.md) · [draft PRD](roadmap-research/24-a2a-otel-adapters-prd.md).

## Follow-ups from hooks and steering

Small known gaps, recorded so they are not lost; none has a design yet.

- **Hook state (hooks part 2c).** Deferred from decision 31; see
  [AfterTool hooks](hooks-after-tool.md).
- **Output, then a deadline.** A turn whose structured output is recorded
  stops `limited` at the deadline instead of completing.
- **Tool-argument deltas** carry no byte offset or call ID.
- **Context after a paused hosted call.** A `BeforeModel` hook's `Deliver`, or
  a host `Deliver` while paused, can land after an unfinished paused hosted
  search; steers already wait (`pausedMidHostedCall`).
- **Urgent steer during `BeforeSend`** of a paused continuation cancels the
  call before sending, and hooks run again: wasted work only.
- **Non-text tool results** (images, cited text) stop the turn on Chat
  Completions and Gemini; send a text placeholder instead.
- **Flaky live test.** `TestLiveUrgentSteerCutsStream` once failed on OpenAI
  with no cut; cause unknown.
- **Steer skips in a parallel batch** omit running tools' output from the
  `MaxCost` fit estimate; the pre-send check still enforces the cap.
- **Resume without a binding.** An allowed call whose tool binding has
  disappeared fails the turn rather than being skipped.
