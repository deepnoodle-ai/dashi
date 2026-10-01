# Finish a long coding request before context fills

Status: Approved\
PRD PR: #144 · Implementation PR: #147 · Updated: 2026-09-30

## Problem and context

A developer asks Dashi to investigate a failure, read files, change code, and run checks.
Repeated file reads and test output grow the next model request during that single Turn.
An individual tool-result limit cannot bound the combined request.
The request can exhaust context before the developer receives a usable answer.

[`Session.Compactor`](../guides/compaction.md) summarizes between ended Turns.
It cannot protect an active Turn.
[`Project`](../../project.go) currently combines all settled Turn messages with caller-owned history.
CLI parity [O07](../research/cli-parity-operations.md) explicitly leaves this workflow incomplete.

[ADK Go #298](https://github.com/google/adk-go/issues/298) requests proactive compaction and retained original history.
[Dive](https://github.com/deepnoodle-ai/dive/blob/da15e916148fcc93f248c08648d22dfab1a4b73a/experimental/compaction/mid_turn.go) rewrites messages before another tool-loop iteration.
The [comparative research](../../design/roadmap-research/15-context-compaction-research.md) describes pi and OpenHands retaining source events beside recorded summaries.
These precedents support the workflow. Dashi must preserve its own Turn, recovery, and accounting contracts.

## Users, job, and concepts

The primary user develops software with the terminal agent.
The secondary user embeds Dashi in a Go application and wants the same protection without a separate transcript-repair loop.

**Active-Turn compaction** replaces older, settled work from the current Turn with a recorded summary in the model's context.
It preserves the Turn's identity, original request, full execution record, and pending work.
**Context allowance** is the declared model window minus the next request's output reserve and other input.
For text-only requests, an estimate predicts this allowance without guaranteeing provider acceptance.
Requests containing non-text media have no qualified context estimate in this slice.

The current Turn owns the replacement of its own messages.
Caller-owned prior history remains unchanged, including its `Start`/`Continue` fingerprint contract.
Between-Turn compaction keeps its existing behavior and name.

## Use cases and acceptance

### UC-1: Complete a long investigation

The developer sends one request: “Read the failing code, fix it, and run the checks.”
During repeated reads and checks, Dashi compacts settled work before another ordinary model call.
The same request continues to its answer.

- [ ] A small-window keyless fixture completes one read/test/fix Turn after at least two compactions.
  The final answer retains the original objective and an earlier diagnostic fact available only through the summary.
- [ ] The library prepares each candidate, including tools, instructions, verified references, hooks, and output allowance.
  Text-only candidates receive active context checks; media candidates follow the explicit limitation below.
- [ ] Compaction runs only after every dispatched model and tool call settles.
  Waiting approval, uncertain effects, or uncommitted results prevent it.
- [ ] Compaction preserves the original request, system/developer instructions, and operator steering with their roles and order.
  It retains a recent complete tool exchange when it fits.
  A cut never splits tool calls from results or required provider replay content.
- [ ] Older tool output becomes labeled evidence in a summary with no operator authority.
  Retained context preserves origin and slot identity.
  Summary prose cannot satisfy an approval, establish verification, or impersonate an operator instruction.
- [ ] Every applied summary identifies its exact covered facts and records the replacement.
  Model preparation, replay, `Session.ProjectedHistory`, and resumed execution apply the same replacement.
  The full inputs, outputs, calls, and results remain in the Turn record for inspection.
- [ ] `Session.History` retains covered ordinary messages and excludes summary requests and results.
  `Session.ProjectedHistory` uses the recorded replacements and excludes the summary-generation exchange.
  After two compactions and reload, both views retain these distinctions and their original message order.

### UC-2: Configure protection in a Go application

The developer enables one active-Turn policy for the agent, then uses ordinary `Run` or `Session.Send`.
The policy becomes part of the accepted Turn configuration.
The CLI enables it for supported models without asking the user to manage a second conversation.

- [ ] One optional configuration selects active compaction. No configuration preserves today's library behavior.
  The [technical spec](../design/active-turn-context.md) defines public names and defaults before implementation.
- [ ] The policy uses the declared model window or an explicit caller cap.
  A positive caller cap supplies the window when the declared window is unknown.
  When both are known, the smaller window applies.
  An unknown window without a cap disables protection and produces one actionable warning.
  The CLI identifies this state without displaying a fabricated percentage or safety claim.
- [ ] Text-only candidates trigger at an estimated 80% of the context allowance.
  If no eligible coverage exists, dispatch the ordinary candidate while estimated input plus output reserve fits the selected window.
  Measurements from completed calls can inform the estimate.
  Every shortened text-only request is checked again before dispatch.
- [ ] A prepared request containing image, PDF, audio, or video content skips proactive compaction and the byte-based hard context check.
  Dashi records one `active_compaction_media_estimate_unavailable` warning per Turn, retained on replay without duplication on continuation.
  Provider preparation and acceptance govern these requests; no context-fit or tokenizer guarantee is claimed.
- [ ] A keyless fixture prepares a generated valid PNG of about 1 MiB with its actual encoded payload at known 128k and 200k windows.
  With sufficient admission budget, ordinary `Send` runs without false `LimitContextWindow` or summary dispatch.
  Historical media, hydrated references, and tool-result media receive the same treatment.
  Repeated candidates/reload retain one warning; text-only behavior and existing cost-risk/budget checks remain unchanged.
- [ ] Summary generation uses the active Turn's model in the first slice, with no tools.
  Its request also has bounded input and output.
  Summary attempts use normal model admission, deadline, hooks, and recorder guarantees.
- [ ] All summary attempts count toward the same model-attempt limit and cumulative budget as ordinary attempts.
  Their recorded purpose distinguishes summarization from the user's answer.
  Compaction cannot reset limits, hide charges, or consume the reserved final-answer attempt.
  Leaving an ordinary attempt available permits a final response; it does not guarantee completion if the model requests another tool.
- [ ] A runnable `Example*` configures protection and completes the long-loop fixture.
  The guide explains text-only estimates, unavailable media protection, summary loss, prior-history ownership, and accounting.

### UC-3: Interrupt and return to the same work

The developer interrupts during summary generation or after its application.
A worker reloads the Turn and continues with the original prior history.

- [ ] Reload preserves the original request, settled effects, summary attempts, and applied replacement.
  Continuing never reruns a completed file edit or command merely because its messages were summarized.
- [ ] An acknowledged summary result survives a crash before application.
  Recovery applies that result once without generating it again.
- [ ] After an ambiguous recorder acknowledgement, reload authoritative facts before any further dispatch.
  Do not invent a summary result, application, or new attempt to resolve that write.
- [ ] A crash after summary intent acknowledgement but before result acknowledgement follows existing model recovery on explicit `Continue`.
  Since summaries offer no tools, existing safe-repeat rules may admit a new summary attempt.
  Retain the interrupted attempt, its accounting evidence, and the new attempt's identity and admission checks.
  The fixture never repeats settled file edits or commands and never treats unknown usage as zero.
- [ ] Explicit cancellation ends the foreground Turn and summary work.
  An interruption preserves resumable work under existing rules.
  Applied summaries remain applied. Partial summary previews never replace context.
- [ ] Repeated or concurrent continuation commands retain existing identity and fencing guarantees.
  Readers without a compactor reconstruct an applied replacement without model work.

### UC-4: Explain a request that cannot continue

The summary fails, saves no context, or cannot leave room for the next request.
Dashi stops with the work already performed visible and an actionable explanation.

- [ ] Empty, failed, or non-shrinking summaries leave the previous model context unchanged.
  They do not erase evidence or create an applied-compaction marker.
  The failure remains recorded with any incurred usage and charge.
- [ ] Dashi makes at most one summary attempt for the same candidate request during one run.
  Failure stops further ordinary dispatch. An explicit continuation may make a new admitted attempt.
- [ ] For text-only candidates, stop before dispatch when protected input or an indivisible exchange exceeds allowance and no legal compaction can make it fit.
  Return `LimitContextWindow` for this case.
  Identify the cause and suggest a smaller input, a larger window, or between-Turn compaction where applicable.
- [ ] Context limitations, summary failure, budget refusal, cancellation, and uncertainty remain distinguishable in public outcomes.
  Provider overflow despite an estimate follows existing failure/recovery rules.
- [ ] Screen and line modes show compacting, applied, failed, and unavailable states.
  Applied status reports estimated savings separately from settled usage and cost.
  Reopen shows applied markers from recorded facts without presenting summaries as user prompts or final answers.

## Decisions and scope

- Compact only settled current-Turn work. This solves repeated reads and checks while preserving caller-owned history.
  Reject silent rewriting of `Continue.History` or a new foreground Turn for the same request.
- Record summary attempts and applied coverage. Recovery needs facts, and budgets need all charges.
  Reject an unrecorded middleware rewrite or a separate unbounded summarizer.
- Keep ordinary conversation history distinct from model context.
  Reject removing covered messages from `Session.History` or exposing summary work as conversation messages.
- Preserve instructions verbatim and summarize evidence at complete exchanges.
  Reject arbitrary truncation, silent provider-artifact stripping, and transcript repair.
- Use active context estimates only for text-only requests. Encoded media bytes do not establish provider context tokens.
  Keep conservative money-risk estimates and cost admission unchanged; reject an invented media formula.
- Ship proactive protection first. Provider overflow does not establish safe retry.
  Compact-and-retry after overflow, model fallback, custom summary models, manual active compaction, and tool retries are separate work.

## Success and risks

Success requires the keyless long-loop, crash, cancellation, no-fit, and budget scenarios above.
Independent acceptance must try the actual CLI flow and the public Go example.
Recovery/accounting, replay, provider-content, hooks, and between-Turn compaction checks must remain valid.
No paid probe is required to claim this fixture-backed behavior.

Summaries can omit useful details. Preserve source facts and document that limitation.
Provider estimates and private replay bundles can differ from text sizes.
Non-text media currently lacks active context protection; a provider can still reject its context.
The media-estimate amendment passed named re-review at `0811b0d`; root approved the checkpoint.
