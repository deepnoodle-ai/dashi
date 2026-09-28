# 14. Session continuity — draft PRD

**Status:** Draft proposal, 2026-09-28. **Roadmap:** [item 14](../remaining-work.md). **Basis:** [source research](14-session-continuity-research.md) and [decision 19](../decisions.md). **Decision sought:** Offer a small optional history-reference boundary; do not introduce a Dashi-owned `Session` store yet.

## User problem

An application that already owns a conversation log must reconstruct the exact prior model history for every `Start` and `Continue`. Dashi correctly avoids copying that history into the Turn, but the API asks the caller to move a potentially large message slice repeatedly and to remember that a resumed Turn must use the same prefix. Its fingerprint makes mistakes visible; default `Continue` only warns. A durable host also needs to know that Turn A consumed conversation prefix N before it accepts Turn A's output into a log that may have advanced.

For a simple Go caller, `History: []llm.Message` is adequate. The first target is an Nvoken- or Mobius-like host with an existing append-only log and crash recovery. The proposal must reduce integration mistakes without creating another owner of transcript, tenancy or scheduling.

## Goals and non-goals

**Goals**

- Let an application name an immutable, model-visible history prefix instead of passing the entire slice on every command.
- Resolve and validate that prefix before a provider call, and give a resumed Turn the same history or a clear error.
- Keep Turn records small and preserve the current direct-history path.
- Make the conversation writer boundary explicit in examples and errors.

**Non-goals:** A Dashi conversation database, default `Session` type, cross-turn lease manager, fork/tree navigation, summarization, retention or access-control policy. This proposal does not make separate Turns mutually exclusive or make external tool effects safe to repeat.

## Smallest API and user flow

Add an **optional** opaque `HistoryRef` on `Start` and `Continue`, and a `HistoryResolver` supplied to the Runner. `History` and `HistoryRef` are mutually exclusive on a command. A ref is an application-scoped key for one immutable *model-visible* prefix; the application owns its syntax, storage, authorization and lookup. The resolver returns `[]llm.Message`. Dashi validates those messages, computes the existing history fingerprint, and records the ref plus fingerprint on `turn_started`. On `Continue`, the ref must match the started Turn's ref; Dashi resolves it again and rejects a fingerprint mismatch before any step or model call. Direct `History` continues to follow decision 19, including its default warn policy. A missing resolver, missing prefix, invalid messages or changed prefix is a typed preflight error. Dashi does not cache a resolved history across process boundaries.

Sketch (names illustrative, pending API review):

```go
runner := dashi.Runner{
    Recorder: recorder,
    HistoryResolver: appHistory.Resolve,
}
// The host has already admitted this conversation writer.
base := dashi.HistoryRef("conversation/42/through/105")
out, err := runner.Run(ctx, dashi.NewTurn("turn-106"), dashi.Start{
    CommandID:  "start-106",
    HistoryRef: base,
    Input:      userInput,
})
// On restart, Continue uses the same base; Resolve returns the same projection.
saved, err := recorder.Load(ctx, "turn-106")
out, err = runner.Run(ctx, saved, dashi.Continue{
    CommandID:  "continue-106",
    HistoryRef: base,
})
// After settlement, the host projects Turn-owned messages and compare-and-
// appends them to its log only if the conversation still has the admitted base.
```

The real signatures must follow Dashi's existing `Run` command shape. The host should store `(conversation ID, base cursor, turn ID)` beside its admission or lease and use `Project(turn)` when committing the Turn's messages. A conflicting append remains a host error requiring reconciliation; Dashi must not silently rebase the completed Turn onto newer history. If a host has no conversation lease, it must accept that two Turns may run from the same base and one can lose the append race after spending tokens or executing tools.

## Acceptance criteria

1. Direct `Start.History` / `Continue.History` behavior and records stay compatible with decision 19. Ref mode stores only the opaque ref and fingerprint, never resolved prior messages.
2. A ref resolves once per `Run` call before any provider call. The returned messages are cloned/validated and used for every model projection in that call; source mutation cannot alter the in-flight view.
3. Missing resolver, unresolved ref, invalid history, mixed `History` + `HistoryRef`, a changed ref on `Continue`, or a changed projection for the same ref fail before committing a step. Errors distinguish configuration, missing data and mismatch.
4. A durable pilot demonstrates start, suspension, cross-process continue, completion and host compare-and-append against one real conversation log. Two Turns from one prefix show one append conflict without corrupting transcript; the loser retains its Turn record for audit/reconciliation.
5. Documentation provides a short stateless example and a host integration example that shows prefix ownership, writer admission, Turn projection and conflict handling.

## Risks and open decisions

- **May not earn the API yet.** Current `History` already avoids storage duplication. Build this only if a real Nvoken/Mobius integration shows repeated history transfer or mismatch handling is a material source of bugs or cost. Until then, document the direct-history pattern and use `HistoryMismatchReject` in durable hosts.
- **What does a ref pin?** It must pin the exact model-visible projection, including compaction and media hydration, not just a raw message high-water mark. Host tests should prove stable resolution after a process restart.
- **Authorization and erasure:** An opaque ref is not an access token. The application's resolver must scope reads to the caller and report an erased prefix as unavailable; Dashi should never log resolved content in errors.
- **Writer admission is separate:** A ref proves history identity, not exclusive right to continue a conversation. An optional full `Session` layer would need a real host-backed claim/lease, CAS append, failure and uncertain-effect semantics. Revisit it only after this pilot exposes a reusable contract across at least two consumers.
