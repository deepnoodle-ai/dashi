# 18. Inspecting newer record versions — draft PRD

**Status:** proposal for review; no API approved. **Size/difficulty:** M / high ([roadmap](../remaining-work.md)). **Reference:** [research](18-newer-record-versions-research.md), [Turn record](../turn.md), `design/gaps/D-protocols-streaming.md`.

## User problem

A service rolls back to an older Dashi binary after a newer binary has written a Turn step. The old binary correctly refuses `Continue`, but `Replay` and `Project` also fail, leaving an operator unable to answer basic questions: which Turn is affected, where did understanding stop, what later facts exist, and what raw record should be handed to a developer? They should be able to inspect the evidence without triggering a model call, tool, policy hook, or write. The UI must never present a partial projection as the current Turn state.

## Goals

- Preserve the entire JSON object for each unknown step version or kind when loading it, including fields older `Step` does not model.
- Offer one **read-only inspection** call that lists step headers and raw facts, identifies the first unsupported step, and labels any derived known-prefix information as partial.
- Keep execution semantics strict: `Replay`, `Project`, `Runner.Run`, `Continue`, `Settle`, and `Cancel` do not skip or guess at unknown steps.
- Give developers an actionable result (`turn ID`, `sequence`, `version`, `kind`, and reason) and give operators a safe summary with an authorized raw-export path.

## Non-goals

No schema migration framework, dynamic step-handler registry, best-effort continuation, provider replay from unknown facts, reinterpretation of later steps, or automatic downgrade/rewrite. No promise to inspect an unknown **file-recorder log format**; this slice concerns a known Turn container with unknown step semantics. Corrupt JSON or broken Turn identity/sequence is a different error, not a “newer version.”

## Proposed smallest API and behavior

1. **Retain unknown step JSON at the decode boundary.** Extend `Step` decoding so it reads the minimal header first (`version`, `turn_id`, `seq`, `kind`, `at`), then decodes typed payload only for version 1 and known kinds. For an unknown version/kind, save the whole original step object in an opaque raw field, and marshal that opaque object from the saved raw value. `filerecorder.Load` already reads each line independently; its typed decode must carry those raw bytes forward. Do not normalize an opaque step into a known kind or silently drop its extra fields. Existing valid v1 records stay wire-compatible.
2. Add a read-only operation such as `Inspect(Turn) (Inspection, error)`. `Inspection` contains Turn ID; ordered `InspectedStep` records with header, recognized/unsupported/corrupt classification, and optional raw JSON; `KnownThroughSeq`; and `BlockedAt` with a stable reason (`unsupported_version`, `unknown_kind`, or `invalid_record`). It may include a **known-prefix** view, but must label it `partial` and never call it the Turn's current stop, spend, transcript, or pending work. Steps after the first unsupported one appear as raw/header entries only, even if their kind is familiar: a newer step could have changed their meaning.
3. Keep `Replay` and `Project` unchanged in principle: they reject any unsupported step before constructing a usable view or model messages. `Runner.Run` refuses all mutating commands on that Turn before any dispatch/commit. The inspect operation never invokes a provider, executor, hook, or recorder `Commit`, and does not repair a record.
4. Provide a small `filerecorder` example showing `Load` → `Inspect` and a host UI mapping: “Turn requires a newer Dashi version; execution is paused at step N (kind K, version V). Earlier information is partial.” Raw JSON is exposed only through an authenticated diagnostic/export path; normal user-facing summaries show headers and the reason, not tool arguments, secrets, or private model content. Hosts using their own Recorder need only preserve raw step JSON on load; no new persistence service is required.

Illustrative API shape, not committed Go names:

```go
turn, err := recorder.Load(ctx, turnID)
inspection, err := dashi.Inspect(turn)
// inspection.BlockedAt: {Seq: 7, Version: 2, Kind: "model_settled",
//                        Reason: UnsupportedVersion}
// inspection.Steps[6].Raw is the original step JSON for authorized export.
// dashi.Replay(turn) still fails; no continuation is attempted.
```

## Acceptance criteria

- A fixture with a future step version and incompatible payload field types loads from the file recorder into `Turn`, and `Inspect` returns its correct header and **unaltered raw step bytes**. Marshal/reload preserves that opaque object. Another fixture covers a new kind at version 1. An unknown step followed by known-looking steps never produces a claimed current view.
- Known v1 Turns inspect with the same ordered headers and no compatibility block; their `Replay`, `Project`, and normal continuation behavior is unchanged.
- `Replay`, `Project`, and `Runner.Run` with every mutating command reject a Turn containing an unknown version/kind before provider/tool/hook calls or a new commit. Repeating the request does not change the record.
- The load/inspect path distinguishes unsupported semantics from malformed JSON, wrong Turn ID, duplicate/out-of-order sequence, or missing first start. It does not mark corruption as upgrade-required. A malformed known payload is reported as invalid, not treated as an opaque future fact.
- The raw step remains available after load and is not emitted by default in logs or the normal inspection summary. A host can opt into authorized export; inspection itself is deterministic and side-effect free.
- A test reads a newer Turn with a known prefix, verifies the prefix label is explicit, and confirms that cost, pending effects, and final status after the block are reported as **unknown**, never zero/completed.

## Risks and open decisions

`Step` is a public struct: an opaque raw field and custom JSON decoding need clear precedence if a caller mutates header fields after load. The simplest rule is that opaque steps are immutable data; execution rejects them, and export uses the saved raw object. File-recorder line framing and `logFormat` remain strict; a future container format needs its own reader. Decide whether `Inspect` exposes a known-prefix `TurnView` at all: it can help diagnosis, but a headers-only first version is safer and smaller. Hosts may persist typed `Step` rows that already discarded unknown fields; documentation must require raw JSON retention at their storage boundary for this guarantee to hold.
