# Continue after compaction or context limits

Status: Draft\
PRD PR: pending · Implementation PR: pending · Updated: 2026-10-07

## Problem and context

Developers use Dashi for investigations that read many files during one Turn.
Issue [#216](https://github.com/deepnoodle-ai/dashi/issues/216) reports active summaries reaching their output limit and stopping with `compaction_failed`.
Later `dashi run -c` prompts refuse that session.
Issue [#218](https://github.com/deepnoodle-ai/dashi/issues/218) reports the same trap after a large file-reading Turn exceeds its context window.
Issue [#217](https://github.com/deepnoodle-ai/dashi/issues/217) reports two completion notices for one history compaction.

Active compaction preserves recorded work, but its prompt does not state the output allowance.
Between-Turn compaction runs only after successful Turns and checks the last model call's usage.
That usage does not include tool output produced afterward.
The automation preflight also rejects stopped parents that Session can already close safely.
Line output prints completion from both the stop event and the final outcome.

This amends the [active context requirements](active-turn-context.md) for one bounded output-limit retry and later-request recovery.
The primary user runs the CLI. Embedded Go applications must receive the same Session behavior.
The job is to finish long work, or send a new request in the same session without manual record repair.

## Use cases and acceptance

### UC-1: Finish after a summary hits its output limit

The developer sends a file-reading request through `Session.Send` or `dashi run`.
The summary reaches `llm.StopOutputLimit`. Dashi tries one shorter summary, then continues the same Turn when it fits.

- [ ] Initial and retry prompts state the effective output allowance and request concise findings, changes, verification, and remaining work.
- [ ] A keyless fixture returns an output-limited summary followed by a complete summary. The original Turn reaches its answer.
- [ ] The first result remains rejected and unapplied. Both calls retain separate identities, usage, charges, and normal admission checks.
- [ ] At most one automatic retry occurs for the same candidate and recorded coverage, including after interruption or authoritative reload.
- [ ] Retry leaves an ordinary model call available. Exhausted model calls, budget, cancellation, and recorder ambiguity keep their existing outcomes.
- [ ] Transport uncertainty, refusal, tool requests, empty summaries, and non-shrinking summaries do not acquire an automatic retry.
  A second output-limited result stops clearly with previous context intact. Neither partial summary becomes a reply or replacement.

### UC-2: Send another request after oversized finished work

The developer reopens the session and sends a new prompt after `compaction_failed` or `LimitContextWindow`.
All earlier dispatched work has settled. Session closes the stopped Turn through its existing rules, then compacts eligible history before new work.

- [ ] A keyless fixture produces large finished tool output with low last-model usage, then stops at the context limit.
  A fresh Session with a configured Compactor accepts a smaller next prompt, compacts the prior work, and completes.
- [ ] The same fixture succeeds through saved-session `dashi run -c` preflight. The CLI does not require manual `/tasks` recovery for this settled parent.
- [ ] A separate fixture exhausts both output-limited summary attempts, reloads, and completes a smaller next prompt after history compaction.
  The failed retry does not trap future requests once work settles.
- [ ] Compaction considers current projected history size as well as reported usage. Failed and context-limited finished work can qualify after safe closure.
- [ ] Full conversation history and source facts remain available. Reload applies the recorded replacement once. Completed effects never run again.
- [ ] A running worker, unresolved effect, pending approval, unresolved child task, or missing task notice still blocks a new request.
  Summary failure retains history and reports why the request cannot fit. Recovery never fabricates an answer or silently drops work.
- [ ] Protected input that cannot fit still returns the context limit without ordinary dispatch. Repeated failures cannot create an unbounded summary loop.

### UC-3: Read one completion per compaction

The developer runs automatic or manual compaction in print or plain mode.

- [ ] Each compaction prints one running notice and one notice for each changed final state.
  Repeated delivery of the same stop and outcome prints no duplicate completion or failure.
- [ ] Distinct compactions each print their own notices. Print mode keeps notices on stderr and the ordinary answer on stdout.
  Screen mode retains its existing state updates. Internal summary content stays hidden in every mode.

## Decisions and scope

- Keep the resolved summary limit at 2,048 tokens by default. State its budget and permit one shorter output-limit retry.
  Reject arbitrary default inflation and provider-specific CLI behavior.
- Put safe closure and history compaction in Session. Size from projected history, including output produced after the last model call.
  Reject rewriting caller-owned `Continue.History` or automatically continuing an impossible Turn.
- Let CLI preflight admit only these settled recovery cases through Session's existing settled-work predicate.
  Session remains responsible for actual closure and compaction.
  Reject treating every failure or non-running worker as proof that effects settled.
- Deduplicate presentation by compaction identity and displayed state. Preserve authoritative outcome updates and distinct compactions.
  Reject deleting an outcome notification needed after interrupted streaming.

No model fallback, paid provider evaluation, new policy knob, media estimator, arbitrary tool truncation, or general transport retry belongs here.
Update the existing active context design, compaction/session guides, CLI recovery instructions, and changelog to match the final behavior.

## Success and risks

Success requires the keyless cases above, a public Session example, CLI transcripts, and independent review.
Run `make check`, including all released modules and devtools. Existing recovery, accounting, media, and provider replay checks must pass.
No paid request is required or authorized for this batch.

Summary quality remains model-dependent. This work guarantees bounded retries and safe recoverability, not lossless summaries or provider-tokenizer accuracy.
Crash fixtures must prove the retry bound from recorded facts. A process-local counter alone cannot satisfy it.
