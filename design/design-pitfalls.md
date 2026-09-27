# Design pitfalls

Failure patterns to avoid when reviewing Kernel's design, derived from Dive's
[framework comparisons][survey] dated 2026-09-26. Framework examples describe
that research snapshot; issue reports are evidence of failure modes, not a claim
that every version still has them. Pair this checklist with
[Design principles](design-principles.md). Source links use the temporary local
research copies of Dive's `docs/comparison/`.

**1. Interfaces that promise more than they support.** ADK-Go exposes tools whose
runtime requirements are hidden in internal interfaces; its vendor types also
constrain alternate providers. Eino's two message models multiply the API.
Reject designs where satisfying the public interface is insufficient, wrappers
are bypassed, or unsupported options are silently ignored. Review the experience
of an external implementer. [Evidence][adk], [Eino analysis][eino]

**2. Infrastructure becoming the agent's architecture.** Eino compiles a graph
for its ordinary loop; MAF installs the loop inside provider middleware;
OpenHands accumulated multiple entry points and configuration systems. Avoid
making graphs, servers, containers, or a particular database prerequisites for
basic execution. Translating another language's class structure or matching a
competitor's feature list is not a sufficient design reason.
[Evidence][structure], [OpenHands lessons][openhands-issues]

**3. Several mechanisms owning the same decision.** Callback slices, mutable
contexts, observers that block tools, and adapter-specific approval code make
ordering part of the hidden contract. A later argument rewrite can invalidate
an earlier approval. Avoid any path where observation changes execution or where
two layers independently own authorization, compaction, or retries. Require
explicit ownership and documented composition rules. [Evidence][adk], [OpenHands analysis][openhands]

**4. Saving only successful turns or complete batches.** MAF loses partial agent
progress; OpenHands can lose completed tool results before a batch finishes.
Checkpointing only on a planned pause cannot protect an unexpected crash. A
persisted assistant tool request also does not establish whether dispatch began.
Review failures between every pair of recording and execution boundaries.
[Evidence][durability], [OpenHands analysis][openhands]

**5. Mistaking persistence for exactly-once effects.** A recorded tool start
without a result means the effect may have happened. A stale worker's rejected
commit does not undo its external action. Do not blindly replay unmatched calls,
label uncertainty as known failure, or turn a journal failure into ordinary model
feedback. Require explicit reconciliation or justified retry semantics;
idempotency and fencing protect different boundaries. [Evidence][nvoken], [OpenHands recovery][openhands]

**6. Treating approval, interruption, and uncertainty as one state.** OpenHands'
unmatched-action path conflates these cases; MAF's batch approval creates ordering
problems. A new user message, reconnect, or resume must not imply consent. Model
risk estimates and tool annotations can inform host policy but cannot establish
authorization themselves. Review mixed batches, denied calls, stale responses,
and changed arguments. [Evidence][openhands-issues], [MAF approval failures][maf-issues]

**7. Repairing context by falsifying history.** Synthetic user messages can make
library bookkeeping look like user instructions. Fabricated tool results can
hide uncertainty. Destructive compaction, orphaned calls, and lost reasoning
signatures can make later requests unusable. Preserve the facts; make repairs
and omissions explicit projection decisions, with provenance. Queue steering
input at safe boundaries. [Evidence][openhands-issues], [pi replay failures][pi-issues]

**8. A second execution path that slowly diverges.** Pi's compaction requests
missed normal controls, and MAF's streaming path developed different history
behavior. Buffering the rest of a response when a tool appears hides useful
progress. Deciding completion from an early text chunk misses later calls.
Review streamed and collected execution against the same fixtures, including
metadata-only chunks and interleaved tool arguments. [Evidence][trackers], [pi lessons][pi-issues]

**9. Limits that reset or conceal extra work.** Approval loops, continuation
hooks, retries, and child agents can bypass a local iteration cap. An automatic
final answer still consumes resources; a sleeping retry still occupies a host.
Avoid declaring completion while queued work remains or reporting a routine
limit as an unexplained error. Review accounting and lifecycle across re-entry,
not just inside the innermost loop. [Evidence][maf-issues], [pi lifecycle lessons][pi-issues]

**10. Unbounded work and ambient mutable state.** Unbounded event queues and
parallel tool dispatch can exhaust resources; finalizers do not define reliable
cleanup. Shared schema mutation and process-global clients make unrelated
sessions interfere. Slow display consumers must not hold durable execution
hostage. Require explicit resource ownership, cancellation propagation, bounded
concurrency, and serialization of conflicting tool actions.
[Evidence][eino], [pi analysis][pi], [OpenHands concurrency][openhands]

**11. Durable data coupled to runtime implementation names.** Gob registration,
reflection internals, and class-name discriminators make code refactors into
storage migrations. Silently falling back to memory after storage failure loses
the promised guarantee. Unknown data should remain inspectable where safe;
unknown execution semantics must not be skipped to keep a turn running.
[Evidence][durability], [OpenHands lessons][openhands-issues]

**12. Treating proposals or specifications as working proof.** Pi's experimental
durable harness was not the shipping CLI's loop. OpenHands' newer stream protocol
was not the protocol its frontend consumed. Nvoken's specified preview identity
differed from its implementation. Adopt lessons at their demonstrated scope, and
verify the actual path consumers use before claiming a guarantee or porting an
implementation. [Evidence][pi], [OpenHands analysis][openhands], [Nvoken discrepancies][nvoken]

[survey]: ../tmp/research/comparison/README.md
[adk]: ../tmp/research/comparison/adk-go.md#8-strengths-weaknesses-and-lessons-for-dive
[eino]: ../tmp/research/comparison/eino.md#8-strengths-weaknesses-and-lessons-for-dive
[structure]: ../tmp/research/comparison/README.md#9-anti-patterns-to-avoid
[openhands-issues]: ../tmp/research/comparison/openhands-issues.md#7-lessons-for-dive
[openhands]: ../tmp/research/comparison/openhands.md#9-strengths-weaknesses-and-lessons-for-dive
[durability]: ../tmp/research/comparison/README.md#4-execution-durability-and-guarantees
[nvoken]: ../tmp/research/comparison/nvoken-cloud.md#9-strengths-weaknesses-and-lessons-for-dive
[maf-issues]: ../tmp/research/comparison/agent-framework-go-issues.md#7-lessons-for-dive
[pi-issues]: ../tmp/research/comparison/pi-issues.md#7-lessons-for-dive
[trackers]: ../tmp/research/comparison/README.md#12-what-the-issue-trackers-say
[pi]: ../tmp/research/comparison/pi.md#9-strengths-weaknesses-and-lessons-for-dive
