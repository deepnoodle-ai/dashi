# Roadmap status

Implementation snapshot: 2026-09-28, `main` at `efcec37` (through PR #70).
Hooks, live delivery, and steering parts 1–3 have landed; see their
[design](hooks.md), [design](live-delivery.md), and [PRD](steering.md).
This inventory covers other
gaps against Dive and the temporary framework survey in
`tmp/research/comparison/README.md`.
The older [gap reports](gap-analysis.md) describe discovery-time code. A
delivered first slice does not imply every provider route is qualified.

Of the 24 items below, **2 are resolved** (9, 10), **7 have a first
slice with work remaining** (1, 4, 8, 11, 12, 20, 22; item 11 awaits its PR), and
**15 remain open** (2, 3, 5–7, 13–19, 21, 23, 24). Thus 22 still have some work, including
qualification and measurement as well as implementation. The separate hooks
and steering follow-ups at the end are outside this count. At this snapshot,
no roadmap-item implementation PR is open.

**Size** estimates implementation scope: S = focused change, M = package or
adapter, L = several packages or a new layer. **Difficulty** estimates design
and qualification uncertainty, independently of size. An existing reference is
not an approved design unless stated. Items are grouped for reading, not ranked.

## Model and provider behavior

### 1. Native structured output for model calls

**Status:** First slice merged in
[PR #64](https://github.com/deepnoodle-ai/dashi/pull/64). It added strict-schema
output to direct OpenAI Responses calls; turns still use
`dashi_submit_output`. Live provider acceptance is unverified. **Size:** M.
**Difficulty:** Medium. **Reference:** [structured output](structured-output.md)
describes the turn contract; [provider gaps](gaps/E-providers-cost-budgets.md)
compare Dive. **Next:** Run a dated OpenAI live probe, then qualify other
provider routes without changing the turn's recorded output fact.

**Packet:** [source research](roadmap-research/01-native-structured-output-research.md) · [PRD](roadmap-research/01-native-structured-output-prd.md).

### 2. Generation controls and provider options

**Status:** Open. `llm.Settings` covers tokens, temperature, reasoning, tool
choice, parallel tools, and citations; stop sequences, `top_p`, seed, service tier, and
typed provider options are absent. **Size:** L. **Difficulty:** High.
**Reference:** [provider gaps](gaps/E-providers-cost-budgets.md) and
[model controls](model-controls.md); a draft PRD is linked below.
**Next:** Choose portable fields versus route-specific options, then implement
and record their effective values in prepared requests.

**Packet:** [source research](roadmap-research/02-generation-controls-research.md) · [draft PRD](roadmap-research/02-generation-controls-prd.md).

### 3. Prompt-cache controls

**Status:** Open. Dashi measures and prices cache usage but cannot request
breakpoints, TTL, or cache keys. **Size:** M. **Difficulty:** High.
**Reference:** [provider gaps](gaps/E-providers-cost-budgets.md) and
[model catalog](model-catalog.md); no approved cache-control design.
**Next:** Design provider-specific cache placement and 5-minute versus 1-hour accounting
together; test that later-turn reminders do not defeat the reusable prefix.

**Packet:** [source research](roadmap-research/03-prompt-cache-research.md) · [draft PRD](roadmap-research/03-prompt-cache-prd.md).

### 4. Provider dialect conformance

**Status:** First slice merged in
[PR #60](https://github.com/deepnoodle-ai/dashi/pull/60). It added a Chat
Completions wire corpus and cross-route replay checks. All-pairs replay
and other compatible dialects remain incomplete. **Size:** M.
**Difficulty:** Medium. **Reference:** [adjacent-library findings](gaps/C-adjacent-libraries.md)
and [provider content replay](provider-content-replay.md). **Next:** Extend
the corpus to other routes and real provider captures; keep fixture results
separate from live qualification.

**Packet:** [source research](roadmap-research/04-provider-dialect-research.md) · [PRD](roadmap-research/04-provider-dialect-prd.md).

### 5. Live provider qualification

**Status:** Open. Selected Anthropic, OpenAI, and Gemini routes passed dated live
checks; many hosted routes, Vertex, and the Gemini error probes remain
unqualified. **Size:** M. **Difficulty:** High because external services vary.
**Reference:** [live-test record](../docs/dev/live-tests.md) and
[hosted-tool plan](2026-09-27-resources-and-hosted-tools.md). **Next:** Build
an exact route/model/feature matrix and run the missing paid probes when keys
are available; label fixture-only support separately.

**Packet:** [source research](roadmap-research/05-live-qualification-research.md) · [draft PRD](roadmap-research/05-live-qualification-prd.md).

## Tools and integrations

### 6. Local MCP client

**Status:** Implemented in the `mcp` module (PR pending): config loading and
merge, stdio and streamable HTTP, fingerprinted approval of project servers,
stable tool names, result mapping, `Refresh`, and `Close`. See the
[guide](../docs/guides/mcp.md). **Size:** M.
**Next:** The CLI wires `Open`, the approval prompt, and a `/mcp` status screen.
Deferred: OAuth and token store, an `OnChange` callback, legacy SSE, and a
runner marker for calls of uncertain effect.

**Packet:** [source research](roadmap-research/06-local-mcp-research.md) · [PRD](roadmap-research/06-local-mcp-prd.md).

### 7. Authenticated provider-hosted MCP

**Status:** Open. Authentication is disabled because exact request snapshots
could persist credentials.
**Size:** M. **Difficulty:** High. **Reference:** [resource and hosted-tool
plan](2026-09-27-resources-and-hosted-tools.md), especially its credential
boundary. **Next:** Separate exact in-memory dispatch bytes from a redacted,
verifiable recordable snapshot before adding auth headers or OAuth flows.

**Packet:** [source research](roadmap-research/07-hosted-mcp-auth-research.md) · [draft PRD](roadmap-research/07-hosted-mcp-auth-prd.md).

### 8. Speech transport

**Status:** First slice merged in
[PR #63](https://github.com/deepnoodle-ai/dashi/pull/63). It added prepared
batch transcription and synthesis for two named OpenAI models.
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

**Status:** First slice implemented on branch `tools/result-bounds`; PR
pending. `Limits.ToolResultBytes` (default 64 KiB) shortens the text of each
tool result in the model view, after `AfterTool`. The record keeps the full
outcome and stores the view with `ToolFact.Shortened`. **Size:** M.
**Difficulty:** High. **Reference:** [adjacent-library
findings](gaps/C-adjacent-libraries.md) and [design pitfalls](design-pitfalls.md).
**Next:** Add a marker that points at a `content.Resource`, per-tool limits, a
request-byte ceiling, and CLI display of `Shortened`; see the PRD.

**Packet:** [source research](roadmap-research/11-tool-result-projection-research.md) · [PRD](roadmap-research/11-tool-result-projection-prd.md) · [guide](../docs/guides/tools.md#large-results).

### 12. Skills

**Status:** Second slice implemented on branch `skills/filesystem`, ready for
review. The first slice ([PR #59](https://github.com/deepnoodle-ai/dashi/pull/59))
added the in-memory catalog. The second adds `skill.LoadDirs`, `DefaultDirs`,
`Set.Expand` for `/name` use, and `Bundle.Hooks` for catalog delivery, with a
guide and example. **Size:** M. **Difficulty:** Medium. **Reference:**
[example-port gaps](gaps/F-example-ports.md), Dive's skill guide, and
[functional areas](functional-areas.md). **Next:** wire the CLI: load in
`main`, join `bundle.Hooks()`, and route `/name` to `Expand`. Later: reload,
namespaced commands, and `load_skill` arguments.

**Packet:** [source research](roadmap-research/12-skills-research.md) · [PRD](roadmap-research/12-skills-prd.md).

### 13. Child turns and delegation

**Status:** Open. A tool can start another Runner, but Dashi has no parent/child
identity, budget roll-up, or lifecycle contract. **Size:** L.
**Difficulty:** High. **Reference:** [functional areas](functional-areas.md) and
[provider/budget gaps](gaps/E-providers-cost-budgets.md); no approved Dashi design.
**Next:** Design parentage, accounting, cancellation, and recovery using one
real application flow before adding a general orchestration API.

**Packet:** [source research](roadmap-research/13-child-turns-research.md) · [draft PRD](roadmap-research/13-child-turns-prd.md).

## Context and record health

### 14. Session continuity

**Status:** Open. Each Turn receives prior history from the application; Dashi owns
no cross-turn Session. This is an explicit open question, not missing Turn
persistence. **Size:** L. **Difficulty:** High. **Reference:** [decision 19](decisions.md)
and [history storage](history-storage.md). **Next:** Pilot the draft PRD's
optional history reference with a durable host before considering a Session
store.

**Packet:** [source research](roadmap-research/14-session-continuity-research.md) · [draft PRD](roadmap-research/14-session-continuity-prd.md).

### 15. Context compaction

**Status:** Helper package done. Package `compaction` shortens a host-owned
history with a recorded summary, a coverage digest, and a pure projection. It
is not a Session layer, and nothing wires it into `cmd/dashi` yet. **Size:** L.
**Difficulty:** High. **Reference:** [guide](../docs/guides/compaction.md),
[history storage](history-storage.md), and [runtime reminders](runtime-reminders.md).
**Next:** Wire `/compact` and the 80% check into the CLI. Later: compaction
inside a Turn, compact-and-retry on overflow, and a Session-layer home for the
record (item 14).

**Packet:** [source research](roadmap-research/15-context-compaction-research.md) · [draft PRD](roadmap-research/15-context-compaction-prd.md).

### 16. Turn-owned attachment references

**Status:** Open. Prior history is no longer copied into each Turn, but bytes in a
Turn's own input or tool result still enter its record. **Size:** M.
**Difficulty:** High. **Reference:** [decision 19](decisions.md) and
[history storage](history-storage.md). **Next:** Design an application-owned
reference and resolver with digest, lifetime, and authorization recheck at
Prepare; never fetch a private resource implicitly.

**Packet:** [source research](roadmap-research/16-attachment-references-research.md) · [draft PRD](roadmap-research/16-attachment-references-prd.md).

### 17. Incremental projection

**Status:** Open, measurement gated. `Project` and replay rebuild state from
the first step; there is no cached projection for long turns. **Size:** M.
**Difficulty:** Medium.
**Reference:** [adjacent-library findings](gaps/C-adjacent-libraries.md) and
[design principles](design-principles.md); a gated draft PRD is linked below.
**Next:** Measure long tool-heavy turns; add a private in-run cache only if
projection is a material local cost.

**Packet:** [source research](roadmap-research/17-incremental-projection-research.md) · [draft PRD](roadmap-research/17-incremental-projection-prd.md).

### 18. Inspecting newer record versions

**Status:** Open. An unknown step kind prevents `Replay` and `Project` on that Turn.
**Size:** M. **Difficulty:** High. **Reference:** [Turn record](turn.md),
[protocol gaps](gaps/D-protocols-streaming.md), and [adjacent-library
findings](gaps/C-adjacent-libraries.md); no approved design. **Next:** Separate
inspect-only reading from execution replay; block continuation on unknown
semantics while retaining the facts.

**Packet:** [source research](roadmap-research/18-newer-record-versions-research.md) · [draft PRD](roadmap-research/18-newer-record-versions-prd.md).

## Recovery, limits, and accounting

### 19. Durable retries after proven rejection

**Status:** Open. Typed failure disposition and retry hints exist; a proven 429
rejection stops the run. A generic 503 remains dispatch-uncertain even when
retryable. **Size:** M. **Difficulty:** High. **Reference:**
[adjacent-library findings](gaps/C-adjacent-libraries.md), [provider gaps](gaps/E-providers-cost-budgets.md),
and [interruption design](turn-interruption.md); a draft PRD is linked below.
**Next:** Design a recorded retry wait that counts attempts and rechecks budget
and hooks.
Uncertain dispatch must still require reconciliation.

**Packet:** [source research](roadmap-research/19-durable-retries-research.md) · [draft PRD](roadmap-research/19-durable-retries-prd.md).

### 20. Connection and idle-stream timeouts

**Status:** First slice merged in
[PR #61](https://github.com/deepnoodle-ai/dashi/pull/61). It added opt-in
response-start, stream-idle, and total-call timeouts for supported HTTP
model clients. Gemini and media clients remain outside this slice. **Size:** M.
**Difficulty:** Medium.
**Reference:** [adjacent-library findings](gaps/C-adjacent-libraries.md) and
[interruption design](turn-interruption.md). **Next:** Qualify other transports
only with their own timeout and interruption semantics.

**Packet:** [source research](roadmap-research/20-stream-timeouts-research.md) · [PRD](roadmap-research/20-stream-timeouts-prd.md).

### 21. Hosted tools under a cost cap

**Status:** Open. A call declaring unbounded provider-hosted work stops before
dispatch under `MaxCost`; reporting its settled partial cost works. **Size:** M.
**Difficulty:** High. **Reference:** [hosted-tool pricing](hosted-tool-pricing.md)
and [finding 11.2](gaps/F-example-ports.md). **Next:** Qualify a provider-enforced
pass bound and its full rebilling behavior before admitting that route under a
hard cap. Other routes may remain excluded.

**Packet:** [source research](roadmap-research/21-hosted-tools-cost-cap-research.md) · [draft PRD](roadmap-research/21-hosted-tools-cost-cap-prd.md).

### 22. Remaining provider price gaps

**Status:** First slice merged in
[PR #58](https://github.com/deepnoodle-ai/dashi/pull/58). It made the provider
example label partial/unknown estimates accurately. OpenAI hosted
image-generation image tokens remain unpriced; other models and tiers need
ongoing evidence checks. **Size:** S per route. **Difficulty:** Medium.
**Reference:** [example-port follow-ups](gaps/F-example-ports.md),
[model catalog](model-catalog.md), and [pricing contract](pricing-proposal.md).
**Next:** Add a rate only when billed units and served-model evidence are
qualified; otherwise preserve partial cost.

**Packet:** [source research](roadmap-research/22-provider-price-gaps-research.md) · [first-slice PRD](roadmap-research/22-provider-price-gaps-prd.md).

## Protocol surfaces

### 23. ACP adapter

**Status:** Open. Dashi has no ACP server or client adapter. **Size:** L.
**Difficulty:** High. **Reference:** [protocol gaps](gaps/D-protocols-streaming.md)
and [Dive ACP research](https://github.com/deepnoodle-ai/dive/blob/65c393f221f1/docs/acpresearch/README.md);
a v1-first draft PRD is linked below.
**Next:** Build on committed-step `Follow` and the local MCP client in item 6;
qualify v1 reconnect and approval behavior with a real editor client. Treat v2
as a later, separate projection.

**Packet:** [source research](roadmap-research/23-acp-adapter-research.md) · [draft PRD](roadmap-research/23-acp-adapter-prd.md).

### 24. A2A and OpenTelemetry adapters

**Status:** Open. Dive ships optional modules; Dashi has neither. **Size:** M each.
**Difficulty:** Medium. **Reference:** [example-port gaps](gaps/F-example-ports.md),
Dive's `a2a` and `otel` modules; no approved Dashi design. **Next:** Pilot OTel
with a real host, then qualify A2A with its own host; preserve the Turn record
as the source of execution facts.

**Packet:** [source research](roadmap-research/24-a2a-otel-adapters-research.md) · [draft PRD](roadmap-research/24-a2a-otel-adapters-prd.md).

## Follow-ups from hooks and steering

Small known gaps, recorded so they are not lost. None is counted among the 24
items above; hook state has a deferred design in the linked document.

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
  Completions and Gemini; send a text placeholder instead. PR #65 addressed
  error-flagged text results, not this case.
- **Flaky live test.** `TestLiveUrgentSteerCutsStream` once failed on OpenAI
  with no cut; cause unknown.
- **Steer skips in a parallel batch** omit running tools' output from the
  `MaxCost` fit estimate; the pre-send check still enforces the cap.
- **Resume without a binding.** An allowed call whose tool binding has
  disappeared fails the turn rather than being skipped.
