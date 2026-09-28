# Prior history storage

**Status:** Decided differently: see [decision 19](decisions.md) ·
**Date:** 2026-09-27

**Outcome (2026-09-28).** Dashi will not store prior history in the turn
record at all. The application passes the earlier conversation on `Start` and
on every `Continue`, as Eino's callers do, and the turn stores a fingerprint of
it so a mismatched resume produces a warning, or, when the host opts in, a
rejected `Continue`. The analysis below, which
recommended copying text and referencing attachments, is kept for its growth
numbers and production evidence. Whether Dashi gains sessions is still open.

An integrator runs a session as one Turn per user message. Dashi copies the
prior conversation into each turn's first step so later invocations cannot
substitute different history, so the integrator stores the same messages, and
every attachment, again on every turn. This document measures that cost and
compares three alternatives, including an immutable session history.

## What is stored today

Per turn, the recorder receives one `Commit` per step (`recorder.go:19-21`). For
a turn with one tool call that is about nine steps. The bytes that matter:

| Step | Carries | Source |
|---|---|---|
| `turn_started` | Definition, full `History` copy, full `Input` | `engine.go:158`, `turn.go:172-178` |
| `model_intent` | Request digest, effective settings, tool definitions (no body) | `engine.go:566` |
| `model_settled` | Complete assistant message, usage | `turn.go:197-219` |
| `tool_settled` | Call and complete `tool.Output` | `turn.go` `ToolFact`, `tools_loop.go:30` |
| others | Headers, decisions, stop | `turn.go:138-154` |

Attachments are `content.Source.Data` (`content/content.go:37-62`), a `[]byte`
that JSON encodes as base64, so a 2 MB image is 2.67 MB in every record that
holds it. It lands in the `Input` of the turn that received it and in the
`History` of every later turn. `Project` rebuilds model messages from step 1 and
clones the history on each call (`project.go:13-14,56-62`). `replay` marshals
every step on each pass (`state.go:83`); reading the code for this turn shape
gives about 12 replays in the engine plus one per commit in `MemoryRecorder`
(`recorder.go:107`), roughly 20 per turn, each re-encoding the start fact.

## Growth model

Assumptions, per turn: one 1 KB user message; two model calls whose messages
average 1 KB; one tool call returning 4 KB. That is 7 KB of new history per
turn. One 2 MB image arrives at turn 3 (2.67 MB as base64 JSON). History-
independent overhead (definition, tool definitions on two intents, step headers)
is about 10 KB per turn in every option. A reference is 0.2 KB.

Options: **(a)** copy everything, as today; **(b)** copy text, store large
content once and reference it; **(c)** reference prior history by turn and
sequence range plus digest; **(d)** an immutable, append-only session log where
a turn records only the range it consumed. (c) and (d) below also use content
references; without them add 2.67 MB once, since the image is stored once.

Bytes committed to the database over the whole session (blob store separate):

| Option | 10 turns | 50 turns | 200 turns | Growth |
|---|---|---|---|---|
| (a) copy everything | 21.8 MB | 137 MB | 671 MB | Text N²; image ×(N−2) |
| (b) copy text, reference content | 0.49 MB | 9.4 MB | 143 MB | Text N²; +2 MB blob once |
| (c) history ref plus digest | 0.17 MB | 0.86 MB | 3.4 MB | Linear; +2 MB blob |
| (d) immutable session log | 0.24 MB | 1.2 MB | 4.8 MB | Linear; +2 MB blob |

(d) exceeds (c) because the log stores each turn's outputs again beside the
turn's facts: 7 KB twice per turn, still linear. The largest single write is the
last `turn_started` row: 2.7, 3.0, and 4.1 MB under (a); 0.06, 0.34, and 1.4 MB
under (b); only the new input plus a reference under (c) and (d).

The quadratic text term is 7 KB × N(N+1)/2: 0.39, 8.9, and 141 MB. It scales
with the new bytes per turn. A coding-agent turn with 20 tool calls of 4 KB and
their model messages adds about 100 KB; then the term is 5.5, 128, and 2,010 MB,
against 1, 5, and 20 MB linear. So the owner's instinct is right with a
condition: text copying is tolerable for chat-sized turns up to about 50, and
not for tool-heavy or long sessions. Attachments dominate (a) at every size: at
50 turns the image is 128 of the 137 MB.

**Read side.** Every option sends the full history to the provider on every
model call; with stateless APIs that quadratic is paid in tokens and network,
not storage, and prompt caching addresses its cost. What differs is what a
resume, `Project`, or inspection must load for turn k:

| Option | Load for turn k | Notes |
|---|---|---|
| (a) | One turn record, 7k KB plus 2.67 MB | Self-contained; about 20 re-encodes per turn |
| (b) | One record, 7k KB; resolve 2 MB per model call | Resolver may cache by digest |
| (c) | Prior turn records or a host index, 7k KB total | Verify digest (sha256 of 1.4 MB is milliseconds) |
| (d) | One range query on the log, 7k KB | Same verification |

Under (c) and (d), `Replay` no longer touches history; only projection does.
Caching the projection per Apply removes the ~20 re-encodes in every option.

## How the production apps store history

**Nvoken** already has shape (d). `execution_stream_messages` is an append-only
per-conversation log keyed by `sequence`
(`internal/adapters/postgres/schema.sql:940-960`);
`turns.through_message_sequence` marks progress (`schema.sql:1572`); compaction
is a new row covering a prefix, `covers_through_message_sequence`
(`schema.sql:913-918`). Each execution segment reads the eligible transcript
from scratch (`internal/services/generation.go:225-252`). Media is stored once,
inline base64 in the message content, up to 5 MB per image and 16 MB per
document (`internal/services/media_input.go:36-45,52-53`). Nothing pins or
digests the prefix a segment consumed; it trusts the append-only log.

**Mobius** stores session messages with artifact references, hydrates bytes at
model time (`internal/services/document_hydration.go:90-114`), and caps stored
text blocks at 256 KB (`internal/services/session_transcript.go:17-27`). Its
suspension checkpoint is rewritten on every model and tool call, so an inline
attachment was "not written once but once per hook fire"
(`document_hydration.go:618-622`). The fix dehydrates base64 to artifact refs
(`:632`), restores them with tenant and storage rechecks on resume (`:683-694`;
`internal/agentruntime/executors/agent_resume.go:944-985`), and fails on a
leftover ref (`:994`). This is workaround A #14.

## The immutable session history question

Yes, it addresses the problem, and it is (c) in a friendlier shape. What
immutable would mean for Dashi: a session is an append-only sequence of entries
with positions. Entry p has a chained digest D(p) = H(D(p−1), H(entry)). A
turn's start records (session, through p, D(p)) instead of a copy. The turn's
input and projected outputs are appended as new entries after it. An edit is a
new entry that supersedes an earlier one, or a branch: a new session forked at
(session, p). Compaction is an entry "summary covering 1..p"; projection uses
the latest summary at or before the turn's position plus the entries after it.

It solves dedup: each message is stored once. (session, p) names a fixed prefix,
so substitution protection comes mostly from immutability; the digest catches a
buggy or hostile store for milliseconds of hashing. History loads are one range
read. Compaction and edits gain a recorded home, which (c) lacks: a chain of
prior turns has nowhere to put "summary of turns 1-40".

What it costs:

- **A session concept** Dashi lacks by design ([turn.md](turn.md), Boundary):
  entry kinds, positions, a compaction entry, and a projection rule.
- **Cross-turn identity.** Session entries must map to turn facts (message and
  call IDs), or outputs are stored twice, as in the (d) row above.
- **Writer fencing.** Two turns in one session must not append concurrently;
  that is a session-level lease the host must own.
- **Digest stability.** A digest over Dashi's JSON encoding of `llm.Message`
  breaks when a field is added without `omitempty`. The digest must be over a
  versioned canonical form, or over host-assigned entry digests.
- **Erasure.** Retention and user erasure (nvoken's `content_expires_at` and
  `user_erased_at`, `schema.sql:1441-1447`) remove bytes from an immutable log.
  An erased entry keeps its digest and loses its content: the same case as a
  content reference whose bytes are gone (below).

Is it the same idea as (c)? Yes. Both replace the copy with a name plus a digest
and need a host resolver. They differ in who names the prefix: (c) names prior
turns, (d) names a session position. (d) is the better name, because compaction
and edits need one and because hosts already have the log. Dashi should not own
the log: it needs only a reference type and a resolver.

## Content references

A new `content.Source` location, exclusive with Data, URL, ID, and URI:

```go
type Ref struct{ ID, Digest string; Size int64 } // opaque app ID, sha256, bytes
// Source.Ref *Ref; Source.MIMEType stays required.
```

**Resolution.** Replay, `Project`, and inspection never resolve. Immediately
before `Prepare`, the engine passes the projected messages through an optional
`Runner.Content` resolver: `Resolve(ctx, Ref) (Source, error)`. It returns
inline bytes, a signed URL, or a provider file ID with origin (upload once,
reuse the ID); the engine checks digest and size of returned bytes. The recorded
intent keeps the reference; the request digest already covers the resolved body
(`engine.go:566`). A reference with no resolver fails before dispatch with a
clear error.

**Access and retention stay in the host.** The resolver runs with the host's
context on every model call, so a revoked share, a deleted file, or a tenant
mismatch is rechecked each time, as mobius does at resume. Dashi stores no
bytes and sets no retention.

**Missing bytes.** Resolution failure is a known pre-dispatch failure: no model
intent is recorded, and the turn stops with a typed `content_unavailable` reason
naming the reference. It is not uncertainty. A host may opt in to a substitution
policy, recorded as a content change like `omit_reasoning`, that replaces the
block with a text note; Dashi never drops it silently. Inspection of old turns
is unaffected because it never resolves.

A tool returning large media stores it and returns a `Ref`; Dashi does not
externalize bytes at commit. This removes mobius's dehydrate and restore pass:
the record never holds the bytes, and the resolver is the recheck.

## Recommendation

**Now.** Add content references and the resolver binding. At 50 turns this cuts
the example from 137 MB to 9.4 MB and removes the multi-megabyte start row.
Cache the projection within an Apply (replay once per step, not about 20 times
per turn); this is design principle 5 and needs no format change. Keep copying
text into `turn_started`. Document the growth formula so integrators can see 7
KB × N²/2 coming.

**Defer** history by reference: the digest chain, session entries, and
compaction semantics. No host runs Dashi sessions yet; we would be guessing.

**Trigger.** Build (d)-shaped history references, as a `HistoryRef{Session,
Through, Digest}` on the start fact and a host `HistorySource`, when either
happens first: a host integrates Dashi under an existing append-only log
(nvoken), where copying would double-store every message; or measured
`turn_started` rows exceed 1 MB at p95 in any host, which in this model is about
145 turns of chat or 11 turns of tool-heavy work.

**Not worth building yet.** A Dashi-owned session store; automatic
externalization of large text; a per-turn chain of prior-turn references (c),
which (d) supersedes.

## Open questions

- Should large tool text (a 2 MB log) become a reference by tool convention, or
  should Dashi adopt mobius's recorded size cap as a projection adjustment?
- Does the resolver run once per Apply with a cache keyed by digest, or once per
  model call? Access recheck argues per call; cost argues per Apply.
- For (d), who appends a finished turn's messages to the session: the host from
  an exported "generated messages" projection (gap A #11), or a Dashi helper?
- Is a digest over host-assigned entry digests enough, or must Dashi verify
  message content itself to keep the substitution guarantee?
