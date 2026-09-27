# Turn loop review against Dive

**Date:** 2026-09-27. **Status:** Experimental; the loop contract is not yet ready to freeze.

This review uses the local research snapshot in `tmp/research/`, especially
`proposal-comparison.md`, `design/2026-09-26-dive-v2-conceptual-model.md`,
`design/2026-09-26-dive-v2-conceptual-design.md`, and
`comparison/nvoken-cloud.md`, plus the copied Dive v1 source in `dive-api/`.
That directory is git ignored, so these are research inputs rather than links
available from a fresh checkout.

## Direction

| Concern | Dive v1 | Kernel now | Assessment |
| --- | --- | --- | --- |
| Unit of work | Response creation owns the loop and session behavior. | An identified `Turn` advances through commands; a session can sit above it. | Addresses the central organizing problem. |
| Durable authority | Transcript and callbacks carry much of the lifecycle. | Versioned steps are authoritative; `Project` derives model history. | Better recovery boundary, subject to a durable host test. |
| Effects | Hooks mix control and observation. | Typed policy, acknowledged intent and result steps, and a lossy observer have distinct roles. | Prevents many invisible effects; does not guarantee remote exactly-once behavior. |
| Late work | Background and external outcomes use separate paths. | `Accept` correlates model results, external tools, and approvals to pending IDs. | One useful path, with cancellation edge cases below. |
| Integration | Dive owns much session and tool coordination. | `Bindings` supplies clients, tools, policy, and recorder; external tools can run in a host worker. | Keeps the core small, but the host seam is not proven in Nvoken. |

The two Dive v2 proposals agree on this direction but differ on dispatch:
one proposes host-dispatched effects, while the other puts dispatch in the
engine behind an acknowledged recorder. Kernel has chosen the latter. Keep
that choice provisional until a real host demonstrates budget admission,
worker ownership, and recovery without duplicate effects. A pure public
reducer should follow evidence of need rather than precede it.

## Gates before stabilizing the API

1. **Cancellation.** A canceled context can enter the tool scheduler before
   the loop checks it; an executor that ignores context can keep `Apply`
   blocked. Explicit `Cancel` leaves unstarted calls in `Pending` although
   they cannot execute or be accepted. Define and test nonexecution versus
   uncertain started work, including partial batches and late results.
2. **Provider failure knowledge.** The loop treats every `Send` error as
   uncertain. Distinguish a proven pre-dispatch or rejected request from an
   ambiguous transport failure, based on evidence supplied by the client;
   preserve any partial result and usage. Only the former can permit an
   automatic new attempt.
3. **Accepted inputs.** `Start` currently accepts only user-role messages,
   which excludes the proposed sessionless scheduled origin. `Bindings.History`
   can change between invocations without a pinned version or digest. Decide
   how recoverable turns bind trusted origin, history, and durable context.
4. **Host proof.** Build an Nvoken-shaped recorder and external-tool fixture:
   atomically admit an attempt with spending authority, lose its acknowledgement,
   change configuration, take over ownership, and reconcile without resending
   the effect or double-counting usage. Verify the recorded model intent gives
   the host enough information for admission; its digest and effective settings
   may be insufficient for prompt-token budgeting.
5. **Provider parity.** Run shared blocking and streaming result fixtures
   across Anthropic, OpenAI, and Gemini, then qualify selected live routes.
   Package tests and local fixtures do not establish that cross-provider claim.
6. **Publish the design.** The README and `AGENTS.md` refer to `design/`, but
   those files are not tracked in this PR's base or head. Commit a focused
   decision set so a fresh checkout can review the intended contract.

The current branch has regression tests for incomplete client calls and
provider-hosted approval requests: neither becomes an actionable pending item
or a dangling projected call. Duplicate commands can return the current turn
when the recorder supports `Load`; other recorder implementations still require
the caller to reload. Local race tests, vet, and build pass. These results
support the implementation slice; they do not close the host and provider
gates above. Session runners, child turns, and compaction remain separate areas.
