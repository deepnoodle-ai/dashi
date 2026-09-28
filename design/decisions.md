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
direction. Committed steps are never dropped; previews are lossy and a gap
signal tells the consumer to resync; UIs follow the record by `Seq` through a
read-after-cursor path so several readers can replay independently and a
disconnect never cancels work. The backend shape is open and should be the
simplest thing that meets those properties; a design note precedes the code.

**4. Capability checks warn and adjust by default; they do not fail.** Decided
in direction. When a request asks for something the model cannot take, prefer
the nearest supported value (an effort of `extra-high` becomes `high`) and
record a warning; when support is unknown, send it and record that. Failing the
request is reserved for cases with no safe substitute, and strict mode is
opt-in. Warnings must be visible to the caller on the prepared request and on
the result. Dive's model catalog, with pricing and features, is to be ported so
the defaults work for known models.

**5. Prior history: reference large content, more analysis for text.** Design
first. Referencing attachments and other large content through an
application-owned reference is agreed in principle. Whether text history is
copied, referenced with a digest, or handled by an immutable session history
needs its own document with growth numbers before choosing.

**6. Context cancellation must not end the turn.** Design first. Agreed: the Go
context ending is not a terminal event. Not agreed: the shape of pause and
resume. The design document must explain how the production gateway runs a turn
today (whether a goroutine stays live per turn, how a dead worker's turn is
picked up) and propose the Kernel model from that.

**7. Budgets.** Design first. A product-level document should define where
limits are declared, how they are checked and reserved, how a budget stop is
represented and resumed, and what stays in the application.
