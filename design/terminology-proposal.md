# Foundation terminology

Status: accepted, 2026-09-27. Implemented across the foundation; public contracts
remain provisional. This decision follows the [conceptual model][model] and
includes changes to stored JSON and replay validation.

The agent-layer names (`Agent`, `Runner`, `Resolve`, `CommandID`) came later; see
[decision 30](decisions.md).

## Public vocabulary

| Previous | Accepted |
| --- | --- |
| `llm.Request` | `llm.Input` |
| `llm.Prepared` | `llm.Request` |
| `llm.Preparation` | `llm.RequestSnapshot` |
| `Preparation.Request` | `RequestSnapshot.Input` |
| `llm.Provider` | `llm.Client` |
| `ID()`, `Config.ID` | `Provider()`, `Config.Provider` |
| `Call(ctx, Prepared, sink)` | `Send(ctx, Request, sink)` |
| `ContentData`, `Content.Data`, `llm.Data` | `ContentBlock`, `Block`, `llm.Block` |
| `Opaque.Scope` | `Opaque.Unit` and `Opaque.Partial` |
| `PricingInfo` | `Pricing` |
| `tool.Call.Requested` | `tool.Call.RequestedArgs` |
| `ChoiceAuto` and siblings | `ToolChoiceAuto` and siblings |

The model-call flow is:

```text
Input → Prepare → Request → Send → Result
                     └→ RequestSnapshot
```

**Input** is the caller's complete model specification: model selection,
messages, instructions, tools, settings, and stream selection. **Request** is
immutable and bound to a destination, retaining original input, effective
settings, adjustments, model evidence, and exact wire bytes. **RequestSnapshot**
is its recordable form. `NewRequest(snapshot)` reconstructs trusted preparation
evidence without requalifying it or granting permission to dispatch.

**Client** is the configured object that prepares and sends. **Provider identity**
is the application's stable name for a configured destination, such as
`azure-production`. **API** identifies its wire format. Provider identity alone
never establishes replay compatibility or qualifies model evidence for another
endpoint. `Input.Model` remains a string; `ModelRef` pairs it with provider
identity.

**Attempt** is an execution occurrence. `Send` performs at most one dispatch and
collects its result; cancellation can prevent dispatch and failure can leave
receipt uncertain. Sending the same request again is another attempt. Changing
its model or settings requires another preparation. The caller owns attempt
identity, retry grouping, and execution records. A snapshot carries no execution
status. `llm.Generate` remains the Prepare-then-Send convenience helper.

Use **turn input** and **model input** in prose. Packages distinguish their
levels in Go. Keep `Content` for model-message entries and `content.Block` for
shared payloads; `Content.Block` makes that relationship explicit.

## Replay and support

`OpaqueUnit` is a closed structural vocabulary owned by `llm`:

| Constant | JSON value | Meaning |
| --- | --- | --- |
| `OpaqueUnitItem` | `item` | Top-level provider item |
| `OpaqueUnitContentBlock` | `content_block` | Provider content block |
| `OpaqueUnitMessagePart` | `message_part` | Part with parent metadata |

Each provider accepts only units it can encode. `Opaque.Type` remains an
open-ended provider discriminator. `Partial` marks incomplete data independently
of its unit. Partial payloads are valid recorded evidence but cannot enter a
request through opaque content or a known-content replay attachment. An
incomplete response can still contain complete individual payloads.
`Origin.Scope` keeps its endpoint/credential compatibility meaning.

Keep `map[Feature]Support`: an ordinary lookup must distinguish unknown,
supported, and unsupported. A boolean map would make every reader responsible
for preserving that distinction. `StopFinished` and the other existing stop
names remain unchanged. Pricing, evidence, and effective settings remain
separate concepts; encoded controls do not prove what the model honored.

The subsequent [content replay contract](provider-content-replay.md) unifies
known-content attachments as `Content.Replay` and adds wrapper versions, message
provenance, file origins, and content-change records. It keeps `Adjustment` for
settings; these additions are separate from the terminology changes below.

## Record changes

This is a coordinated breaking change to the provisional API and records:

- Request snapshots use `input` instead of `request`.
- Shared model content uses kind `block` and field `block`, replacing `data`.
  Provider wire payloads and `content.Source.Data` retain their own encodings.
- Opaque records replace `scope` with required `unit` and optional `partial`.
  Old `content` maps to `content_block`. Old `partial` requires recovering the
  structural unit from its producer and setting `partial: true`; do not guess.
- Tool calls use `requested_args` instead of `requested`.

Older stored records require explicit migration before reuse. There are no
legacy aliases or compatibility decoder. Missing or unknown opaque units fail
validation; unfamiliar provider types remain preservable. Fixture tests cover
serialization and replay; they do not establish live provider qualification.

[model]: ../tmp/research/design/2026-09-26-dive-v2-conceptual-model.md
