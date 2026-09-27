# Foundation packages

Provisional contracts for one model call; agent and session design remains open.

| Package | Owns |
| --- | --- |
| `content` | Shared blocks and replay origin |
| `tool` | Definitions, calls, outputs, outcomes |
| `llm` | Requests, preparation, results, usage |
| `providers/*` | Configured clients and API conversion |

Dependencies flow from providers → `llm` → `tool` → `content`. HTTP and SSE
code is internal. These packages do not own turns, persistence, or retry policy.
Provider adapters may depend on a vendor SDK; the core does not. Following Dive,
Chat Completions uses HTTP and Gemini uses Google's Go SDK. Gemini preparation
captures the SDK's serialized body through an in-memory transport; sending
checks that the SDK still emits those bytes before a real dispatch. A change in
SDK serialization therefore fails before sending instead of making the recorded
body inaccurate. Vertex uses ADC or an application-supplied authenticated HTTP
client and requires a stable credential scope for private replay.

## Model calls

An `llm.Client` names its configured destination with `Provider()` and its wire
format with `API()`. Custom endpoints require `Config.Provider`. `ModelRef`
pairs provider identity with model name; results retain requested and reported
model refs.

`Prepare` validates and encodes the full `Input` without network access.
`Input.Stream` chooses the transport. The immutable `Request` retains the input,
effective settings, adjustments, content changes, model evidence, and exact
wire body; `RequestSnapshot` is its recordable form. `Send` performs at most one
dispatch; a sink observes streaming deltas. Each send is a separate attempt,
including retries of the same request. The caller owns attempt identity and
recording.
`llm.Generate` combines preparation and sending for simple use.

`ModelInfo` contains application-supplied capability facts. Its `ModelEvidence`
scopes them to an exact model, API, and endpoint with owner, source, date, and
evidence method; pricing is separate. Known unsupported features fail preparation.
Unknown support fails unless `UnknownAllow` is explicit, in which case the
prepared snapshot records the policy and unverified features. Typed adjustments
account for defaults; replacing or omitting explicit controls is unsupported.
Preparation does not prove dispatch or receipt, and these facts do not guarantee
API acceptance. See [model controls](model-controls.md) for the contract.

`Usage` records token measurements; `Result.Cost` holds a reported amount when
available. Application-supplied `ModelInfo.Pricing` is one complete rate sheet.
`Pricing.Estimate` returns unknown when usage cannot support a faithful
estimate. Kernel does not select a price variant or silently replace missing
provider cost. See [pricing](pricing-proposal.md).

## Replay and outcomes

Private content carries provider identity, API, requested model, and a scope
from endpoint and credential. Compatible replay requires an exact match;
recognized portable text and tool calls may instead shed incompatible replay,
with each strip recorded. A per-call policy may explicitly omit reasoning that
cannot replay; default rejection and compatible replay remain intact. Citations
and unknown content stay strict. File IDs carry an origin checked by provider,
API, and scope, independently of model.

`Content.Replay` holds private data on known content; `Content.Opaque` holds
unknown content. Opaque wrapper version 1 supports typed structural units and
`Partial`, which prohibits replay. Message source survives detachment from its
result, while results retain call facts even when no message was received.

Preparation requires completed, unambiguous tool exchanges and preserves saved
call IDs. Unsupported IDs are deterministically remapped for the prepared body,
with paired call/result decisions recording original and outgoing IDs. Tool
schemas remain raw JSON. Definite tool failures are outcomes; Go execution
errors preserve uncertainty. See [content replay](provider-content-replay.md)
for supported adaptations, record changes, and deferred loss policies.

## Provenance

Parts of the provider foundation are adapted from Dive
`65c393f221f193e85ce8a8c73c5e01d8ba056948`; source headers record that
provenance, and the repository uses the [Apache-2.0 license](../LICENSE).
Local tests cover preparation, replay boundaries, streaming equivalence,
accounting, and tool validation. Blocking and streaming final-result calls
passed on 2026-09-27 for Anthropic Messages (`claude-haiku-4-5-20251001`),
OpenAI Responses (`gpt-4.1-mini`), and Gemini API (`gemini-2.5-flash`). Other
routes and Vertex ADC still need qualification. Media, resource references,
and provider-hosted tools have
separate implementation and qualification notes in this directory.
