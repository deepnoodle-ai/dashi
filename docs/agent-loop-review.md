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
| Late work | Background and external outcomes use separate paths. | `Accept` correlates model results, verified nonexecution, external tools, and approvals to pending IDs. | One path for late facts; the host must authenticate them. |
| Integration | Dive owns much session and tool coordination. | `Bindings` supplies clients, tools, policy, and recorder; external tools can run in a host worker. | Keeps the core small, but the host seam is not proven in Nvoken. |

The two Dive v2 proposals agree on this direction but differ on dispatch:
one proposes host-dispatched effects, while the other puts dispatch in the
engine behind an acknowledged recorder. Kernel has chosen the latter. Keep
that choice provisional until a real host demonstrates budget admission,
worker ownership, and recovery without duplicate effects. A pure public
reducer should follow evidence of need rather than precede it.

## Implemented and tested locally

1. Cancellation checks precede tool dispatch, stop waiting when an executor
   ignores context, and derive not-executed outcomes for calls without a
   recorded start. Started effects remain pending for late reconciliation.
2. Provider errors carry explicit rejection, pre-dispatch, or unknown effect
   knowledge. Partial content or usage keeps a failure uncertain. A host can
   submit a verified nonexecution fact, with an evidence reference, before a
   new attempt.
3. `Start` pins origin and prior history in the turn. Scheduled work retains
   its origin; a changed definition or duplicate command with changed history
   conflicts. Only an operator-origin start may carry privileged roles.
4. A host-shaped fixture atomically reserves budget from prepared-request
   admission metadata, loses an acknowledgement, changes configuration and
   ownership, reconciles non-dispatch, accepts external work, and counts usage
   once. A shared wire fixture checks blocking and streaming text, stop,
   identity, and usage facts for Anthropic, OpenAI, and Gemini.
5. Incomplete client calls and hosted approval requests do not become
   actionable pending work or dangling projected calls. Duplicate commands
   return the current turn when the recorder supports `Load`.

The tracked `design/` decision set now makes the README and `AGENTS.md`
references available in a fresh checkout. Local race tests, vet, and build
support this implementation; they do not constitute live qualification.

## Remaining qualification

Use a production-style durable recorder and worker in Nvoken to verify the
same atomicity and takeover behavior under process failure. Qualify selected
live provider routes, including streaming and hosted-tool behavior. An opt-in
six-call final-result probe is in `providers/live_agent_test.go`. The
engine-dispatch decision stays provisional until that consumer proof exists.
Session runners, child turns, and compaction remain separate areas. A host
must authenticate commands, operator origin, external results, and
nonexecution evidence; Kernel validates correlation, not the external truth.
