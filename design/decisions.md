# Decision log

Outcomes of the decisions raised by [gap-analysis.md](gap-analysis.md). A
decision here is a direction the code should follow until a later entry
supersedes it. Items marked "design first" are not decided; each gets its own
design document before any code changes.

## 2026-09-27

**1. A tool's Go error is a model-facing failure; a sentinel stops the turn.**
Decided. A plain error returned by a tool becomes a failed result the model
sees, as Dive did. A typed sentinel marks an error as fatal to the turn, which
stops it in place. Uncertainty remains the engine's word for a call whose result
was never observed (crash, lost result, panic); it is not something a tool
returns. Rationale: almost any error in a tool call should go back to the model,
and returning `err` is the idiomatic way to say so. Supersedes the "Go execution
errors represent uncertainty" rule for tools.

**2. Tool panics are recovered and recorded as uncertain.** Decided. Recover
once at dispatch, commit the settled step with the panic value as the reason,
and stop the turn as uncertain. Nothing is re-run.

**3. Live updates use three delivery classes and a cursor.** Decided in
direction; proposal in [live-delivery.md](live-delivery.md). Committed steps are
never dropped; previews are lossy and a gap signal tells the consumer to resync;
UIs follow the record by `Seq` through a read-after-cursor path so several
readers can replay independently and a disconnect never cancels work. The
backend shape is open and should be the simplest thing that meets those
properties; a design note precedes the code.

**4. Capability checks warn and adjust by default; they do not fail.** Decided
in direction. When a request asks for something the model cannot take, prefer
the nearest supported value (an effort of `extra-high` becomes `high`) and
record a warning; when support is unknown, send it and record that. Failing the
request is reserved for cases with no safe substitute, and strict mode is
opt-in. Warnings must be visible to the caller on the prepared request and on
the result. Dive's model catalog, with pricing and features, is to be ported so
the defaults work for known models.

**5. Prior history: reference large content, more analysis for text.** Design
first; proposal in [history-storage.md](history-storage.md). Referencing
attachments and other large content through an application-owned reference is
agreed in principle. Whether text history is copied, referenced with a digest,
or handled by an immutable session history needs its own document with growth
numbers before choosing.

**6. Context cancellation must not end the turn.** Design first; proposal in
[turn-interruption.md](turn-interruption.md). Agreed: the Go context ending is
not a terminal event. Not agreed: the shape of pause and resume. The design
document must explain how the production gateway runs a turn today (whether a
goroutine stays live per turn, how a dead worker's turn is picked up) and
propose the Kernel model from that.

**7. Budgets.** Design first; proposal in [budgets.md](budgets.md). A
product-level document should define where limits are declared, how they are
checked and reserved, how a budget stop is represented and resumed, and what
stays in the application.

## 2026-09-28

These refine decision 6; the design is
[turn-interruption.md](turn-interruption.md).

**8. The context ending pauses the turn; only `Cancel` ends it.** Decided. The
engine records a resumable stop, and `Continue` resumes the turn on any worker.
A crash is a pause whose stop was never written. Rationale: deploys, crashes,
and lease loss are common and recoverable, not user decisions.

**9. A cut-off model call is sent again on resume.** Decided. It is recorded as
interrupted (`ModelFact.Interrupted`, not `Abandoned`); partial output and usage
are kept for display and accounting, never acted on. Rationale: a model call
costs money but changes nothing outside the turn, and our production hosts
already resend it.

**10. Provider-hosted tools follow the local-tool rule.** Decided. If a cut-off
call may have run a hosted tool not known to be read-only or idempotent, resume
does not resend it and the application decides; otherwise it is resent. Kernel
marks built-in hosted tools it knows are read-only, such as web search;
definitions can say so for others.

**11. Safe local tools are rerun on resume.** Decided. A started call with no
recorded result is rerun when annotated `ReadOnly` or `Idempotent`; otherwise
it is uncertain and the application reports it. This does not wait for
execution-identity work.

**12. Interruptions have their own limit.** Decided. A new
`Limits.Interruptions` (default 10) counts resumes; exceeding it stops the turn
as `limited`. Interruptions do not consume `ModelAttempts`: a model call
recorded as interrupted is not counted as an attempt, though its usage and cost
still count toward spend. Rationale: they are
usually the hosting's fault, a different budget from model retries.

**13. Tool results arriving after the context ends are dropped.** Decided. No
late-result window. Rationale: rerunning safe tools covers the common case
without timing-dependent behavior.

**14. Names: `interrupted` and `uncertain`.** Decided. The resumable stop is
`StopInterrupted`. A call that started with no saved result, so Kernel cannot
tell whether it ran, stays `uncertain`.

**15. Graceful shutdown uses a built-in pause signal.** Decided. A per-call
input (such as `Bindings.Pause`) makes the engine stop before the next model
call or tool call and record `interrupted` ("pause requested"); in-flight work
is not cut off. Rationale: `Policy` is a four-method interface every app would
have to wrap, and `BeforeModel` does not fire during a long tool loop.

**16. `Accept` on an interrupted turn only records.** Decided. `Continue`
resumes the turn. Generalized by decision 18.

**17. The caller can declare that an ended context means cancel.** Decided.
Interrupted is the default; a per-call option (such as
`Bindings.OnContextDone`) set to cancel makes a context end, including leaving
`Engine.Stream`, record a final `StopCanceled`. Rationale: a web handler whose
client disconnected knows the turn will not resume, and `net/http` cancels
without a cause, so the cause alone cannot say so.

**18. `Accept` only records; only `Continue` runs a turn.** Decided. This
applies everywhere, including approvals and tool results delivered to a waiting
turn, which today carry on immediately. The report is committed before `Accept`
returns, and the outcome and saved turn say when the turn is ready to continue.
Rationale: one consistent rule, and the request that records an approval should
not run the model inline; the ready flag keeps a forgotten `Continue` visible.
