# Design principles

Practices to adopt when designing Kernel, derived from Dive's [framework
comparisons][survey] dated 2026-09-26. These are proposed design guardrails, not
approved interfaces or claims about implemented behavior. Read alongside
[Design pitfalls](design-pitfalls.md) and [Ubiquitous language](ubiquitous-language.md).
Source links use the temporary local research copies of Dive's `docs/comparison/`.

**1. Keep a small, replaceable Go core.** Separate one model call from the agent
loop, and implement the loop in ordinary Go. Public interfaces must describe
everything their implementations need; built-ins should use those same
interfaces. Keep workflow engines, protocol adapters, sandbox management, and
gateway infrastructure optional. Eino's graph-backed loop and ADK-Go's hidden
interface requirements show the cost of extra machinery. [Evidence][core]

**2. Make configuration explicit and safe to share.** Treat agent definitions,
model clients, and tool schemas as immutable shared values. Keep mutable state
with the turn and pass request-specific settings explicitly. Record the
configuration accepted for recoverable work, with an explicit policy for later
changes. This addresses Eino's concurrent-reuse bugs and OpenHands' conflicting
configuration paths. [Evidence][eino-issues], [OpenHands lessons][openhands-issues]

**3. Preserve provider meaning.** Use one content-block model with provider and
model provenance, preserving opaque reasoning data where replay requires it.
Separate wire dialect, provider, and model capabilities; represent compatibility
differences explicitly. Transform history for the target provider and report
unsupported settings. Pi's provider design and the cross-framework replay bugs
make fidelity a core responsibility. [Evidence][pi], [tracker synthesis][trackers]

**4. Use one pipeline for every model call.** Normal execution, compaction,
summaries, retries, and child turns should use the same request preparation,
policy, accounting, and observation mechanisms, with explicitly scoped settings.
Reconsider context and available tools before each call. Pi's separate compaction
path repeatedly lost headers, hooks, and output controls. [Evidence][pi-issues]

**5. Derive context from recorded facts.** Keep execution history authoritative
and build model-facing messages as a projection. Cache that projection
incrementally. Record compaction coverage and context provenance without erasing
the underlying history. Validate tool pairing, ordering, and reasoning-block
constraints when constructing requests and choosing compaction boundaries.
OpenHands' incremental view supplies the positive example. [Evidence][openhands]

**6. Make recording an acknowledged execution boundary.** For durable execution,
commit model-call intent before dispatch; commit the model result and pending
tool calls together; record tool start before invoking it; and commit each result
before dependent work advances. Preserve completed siblings immediately while
projecting results in call order. Define storage-failure and ownership checks
explicitly. Nvoken demonstrates the commit boundaries; pi's experimental harness
demonstrates independent parallel completion. [Evidence][nvoken], [pi harness][pi]

**7. Centralize tool validation and authorization.** Apply one dispatch pipeline
to local, remote, and delayed tools. Validate effective arguments and bind each
approval to its recorded call and arguments. Keep policy separate from execution
and observation; waiting should not require re-entering tool code. Return rich
content and distinguish model-correctable tool failures from execution failures.
In Kernel a tool's Go error is model-facing and a sentinel stops the turn; see
[decisions](decisions.md) entry 1. MAF's approval binding is useful; its batch
approval limitation is instructive.
[Evidence][tools], [approval failures][maf-issues]

**8. Return explicit lifecycle outcomes.** Distinguish completion, limits,
waiting, cancellation, failure, and uncertain effects. Report pending work and
what can happen next without requiring callers to reconstruct status from
progress events. Resume by stable IDs with defined duplicate handling. Return
durable waits to the host, and distinguish an invocation ending from all work
settling. [Evidence][resume], [pi lifecycle lessons][pi-issues]

**9. Bound work across continuation.** Carry accounting across retries, approvals,
continuations, and delegated work; define how child usage charges its parent.
Enforce call, concurrency, and time limits, with token or cost policies where
supported. Separate idle-stream timeouts from request deadlines. Report a limit
as a typed outcome; reserve budget for any final summarizing call. MAF's tracker
shows why an inner-loop cap alone fails. [Evidence][maf-issues], [timeouts][pi-issues]

**10. Give streaming one execution path and stable identity.** Streaming and
collected results must agree. Forward interleaved text, reasoning, and tool
arguments as previews; decide completion after the full stream. Separate committed
facts from previews, using shared message and call IDs plus attempt identity.
Define bounded buffering,
cancellation, and preview-loss behavior. Nvoken and OpenHands independently
converged on this separation. [Evidence][stream], [OpenHands protocol][openhands]

**11. Design durable formats for evolution.** Use explicit schema versions and
stable wire names, preserving unknown data where safe. Make commits and migration
behavior visible. Reading unfamiliar content for inspection must not imply that
execution can safely resume through an unknown state transition. MAF's raw JSON
preservation is useful; Eino's gob migrations show the opposing pressure.
[Evidence][durability], [MAF analysis][maf]

**12. Turn guarantees into reusable conformance checks.** Provide scripted models,
recorded provider fixtures, and storage contract cases. Cover mixed stream chunks,
provider switching, approval ordering, concurrent reuse, cancellation, interrupted
commits, and repeated resume. Test examples against releases. Pi's reusable
storage cases and the trackers' repeated regressions make this part of the
design contract. [Evidence][pi], [tracker synthesis][trackers]

Exact streaming signatures, approval representation, argument strictness,
uncertainty defaults, journal batching, and graph scope remain [open design
choices][open]. These principles constrain their behavior without selecting an API.

[survey]: ../tmp/research/comparison/README.md
[core]: ../tmp/research/comparison/README.md#2-core-packages-and-go-types
[eino-issues]: ../tmp/research/comparison/eino-issues.md#7-lessons-for-dive
[openhands-issues]: ../tmp/research/comparison/openhands-issues.md#7-lessons-for-dive
[pi]: ../tmp/research/comparison/pi.md#9-strengths-weaknesses-and-lessons-for-dive
[trackers]: ../tmp/research/comparison/README.md#12-what-the-issue-trackers-say
[pi-issues]: ../tmp/research/comparison/pi-issues.md#7-lessons-for-dive
[openhands]: ../tmp/research/comparison/openhands.md#9-strengths-weaknesses-and-lessons-for-dive
[nvoken]: ../tmp/research/comparison/nvoken-cloud.md#9-strengths-weaknesses-and-lessons-for-dive
[tools]: ../tmp/research/comparison/README.md#5-tool-systems-and-their-go-interfaces
[maf-issues]: ../tmp/research/comparison/agent-framework-go-issues.md#7-lessons-for-dive
[resume]: ../tmp/research/comparison/README.md#7-suspendresume-and-error-handling
[stream]: ../tmp/research/comparison/README.md#11-nvoken-durability-and-streaming-in-production
[durability]: ../tmp/research/comparison/README.md#4-execution-durability-and-guarantees
[maf]: ../tmp/research/comparison/agent-framework-go.md#8-strengths-weaknesses-and-lessons-for-dive
[open]: ../tmp/research/comparison/README.md#16-open-questions-this-survey-raises-for-dive
