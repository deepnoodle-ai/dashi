# Whole-input requests and response-only output

Status: Approved · Author: Codex · Date: 2026-09-30\
Requirements: [Run one coding request from a script](../prds/cli-automation.md)

## Shape and ownership

Wrap existing parent answer presentation with whole-stdin input and response-only JSON/JSONL formatting.
Current print mode uses `Outcome.Reply`. The replayed answer is fixed at a terminal stop and can be empty after successful tool-only work.
A waiting Turn can have answer text beside its calls. Preserve that projection rather than reconstructing text from raw model events.
Current CLI options expose no task-output schema or multimodal answer export. This slice adds neither.

Use a small private response encoder, not direct encoding of Outcome, TurnView, Event, llm.Message, or task/model/tool facts.
The linked slice 8 requirements now cover response content and exits/stderr only. Usage/spend remains in existing inspection and task surfaces, outside automation completion.
Reuse generic input/descriptor/write helpers. Keep journal, Session, recorder fencing, Engine parent/queue ownership, and delegate.Tasks child ownership.
Add no root API, record format, ledger, recovery store, or queue driver.

## Input and preflight

Add `--output-format text|json|jsonl`, exact lowercase, default text, retaining whether the flag was explicit.
Check conflicts and qualify stdout before live environment/client/tool/MCP setup or admission.
Explicit format conflicts with `--plain`, `--inline`, and `models` listing. CLI `--help` remains human usage and starts nothing.

| Route | Behavior |
| --- | --- |
| `-p` or explicit format | One-shot. Read redirected stdin once and combine with optional prompt. |
| Redirected stdin without `--plain` | One-shot text, including stdin alone. |
| `--plain` without `-p` or format | Preserve line/command/approval input. |
| Other terminal routes | Preserve fullscreen/inline or line fallback. |

Never read terminal stdin or change terminal input modes, regardless of stdout arrangement.
Read at most `16 MiB + 1` in bounded chunks before admission. Reject overflow, NUL, invalid UTF-8, or read error with code 2.
Cancellation returns no partial payload and admits nothing. Own cancellable pipe duplicates; originals stay open.
Join nonempty parts as `prompt + "\n\n" + stdin` without trimming. Reject whitespace-only input except no-payload resume.
Parse explicit prompt/attachment flags through existing snapshot helpers before appending literal stdin.
Raw payload never enters command, attachment mention, or approval dispatch.

Slash-prefixed `-p` permits local help/quit only with empty stdin. Reject other commands and mixed command/data input without provider dispatch.
Machine command/error routes contain no assistant response. Print usage, guidance, and errors on printable stderr.
When stdout is qualified, JSON can emit `{"messages":[]}` and JSONL emits no line. Malformed parsing and `--help` may bypass machine formatting.

## Response selection and framing

The only fields are `messages` for ordered response iteration and `content` for the answer string.
JSON is `{"messages":[{"content":"..."}]}` plus LF. JSONL is `{"content":"..."}` plus LF per complete message.
Assistant role is implicit. Omit version/type, IDs, revisions, destinations, stops, actions, persistence, spend, errors, and warnings from stdout.
Do not infer a typed value by parsing answer text or expose previously unused structured-output blocks.

For new or continued execution, select nonempty Answer text from the parent request/notice Outcomes reached by this invocation.
Emit once per stopped parent Turn in parent-driver order. Include foreground task-notice answers, excluding direct child output and unrelated earlier requests.
A continued request's answer can include its own earlier retained content, as the existing Answer projection specifies.
Do not separately emit tool-loop intermediate messages, compaction summaries, reasoning, tool details, inputs, replay artifacts, or configuration.
An empty Answer emits no message. Never search older Turns to fill an empty response.
A noncompleted stop can supply useful response text; its nonzero exit and stderr qualify the work without a stop object.

JSON waits for settlement/shutdown and encodes selected answers in order. JSONL emits each acknowledged stopped-parent answer as a complete message object.
JSONL is not a token-preview protocol. No delta index, correction event, progress gap, or final run envelope is needed.
Successful EOF means selected content was delivered. Nonzero exit can accompany useful content and never promises completion of invocation work.
Text retains last-answer-only behavior. JSON escaping preserves exact answer text; terminal text/stderr use workspace.PrintableText.

## Relevant saved response and safe continuation

Use the inspection track's workspace-scoped loader and exclusive lock. Preserve one lease across record-only reading and permitted live open.
Require existing directories. Load through filerecorder and Turn.View without creating/repairing records or claiming a worker.
Use recorded origin/order/links and the existing foreground parent answer selection. Empty legacy origin remains user.

Completed no-payload resume selects the relevant latest saved parent request/notice Outcome for the latest ordinary request, excluding compaction and unrelated history.
Return its Answer once, or no message if empty. This is a saved-response read, not a transcript export or all earlier request answers.
Resolution-required work similarly returns available relevant saved content with attention on stderr and a nonzero exit.
If continuation is permitted, emit newly reached stopped-parent responses. Do not pre-emit the saved answer as another response before continuation.
A new request's output excludes prior responses; unfinished work blocks admission rather than being silently closed to accept it.

Load children only where existing readiness, effect safety, quiet/notices, or attention requires them. Verify parent/original-call links with task-host logic.
Do not aggregate child/spend snapshots for response formatting. Completed/blocked reads require no live credentials, tools, MCP startup, or attachment resolver.
Required latest/child record failures return code 1 and sanitized guidance. Never replace corrupt latest work with an earlier readable reply.
Missing recorded child-notice delivery is unfinished work, reported on stderr or driven only through permitted live admission.

Only validated new input or ready/interrupted continuation enters live setup. Transfer the lease to Engine and recheck revisions and recovery/destination/tool/resource/attachment bindings.
Use plain dashi.Continue only when Next/readiness permits. Do not use Engine.Resume attempt doubling or retry heuristics.
Do not settle, discard, raise budgets, approve, or close unresolved work implicitly.
Unknown acknowledgement retains its pending Start/CommandID; reload establishes admission before repeat submission.
Encoding content neither changes persistence nor authorizes a remedy.

## Bounded writing and shutdown

One worker owns stdout. Serialize one selected message at a time; use bounded UTF-8 write chunks of at most 4 KiB.
JSON can traverse recorded selected responses after settlement without retaining another transcript or report.
JSONL callbacks mark acknowledged responses pending without copying raw events or blocking source recording.
Bound scheduling hints to 64 entries/256 KiB. Coalesce hints and rescan recorded parent order after overflow, without dropping, reordering, or duplicating content.
The writer consumes recorded text sequentially. It retains no unbounded queue of copied answers and emits no public gap objects.

Use cancellable pipe/qualified TTY writes with a 10-second inactivity deadline, renewed when bytes advance.
Regular files/null use checked writes. Refuse unsupported uninterruptible descriptors before dispatch.
TTY adapters duplicate descriptors and preserve configurable flags/terminal attributes. Never close originals or read terminal stdin.

Check encoding/write/flush failures. Never repeat a written prefix, append stdout error objects, or retry effects to reproduce output.
Output failure cancels owned execution and attempts existing bounded shutdown/settlement. Print saved-session/uncertainty guidance on stderr.
An unusable stream can end in a partial JSON frame; fail nonzero rather than claim delivery.

Register SIGINT/SIGTERM before reading. Remember the first signal, cancel execution/readers, and close the task host through its accepted shutdown path.
Keep writer lifetime separate so usable stdout can deliver selected recorded content after shutdown, without duplicating already-emitted messages.
Release the lease after workers join and adapters close. Unknown shutdown/persistence is an error on stderr. Hard termination guarantees no output.

Exit priority remains signal 130/143, output/persistence failure 1, then preflight 2 or affected work.
Internal ranks remain failed, attention, limited, refused, canceled, with respective codes 1, 3, 4, 4, 4.
Attention includes approval/question/result, uncertain effects, and interrupted work needing continuation without an invocation signal.
Equal ranks prefer original parent, then lexical notice/child Turn/task/call IDs. Stderr retains every affected target/remedy and actual stop.
Code 0 requires completed parent work, quiet children/notices, and successful output. Unrelated historical stops are not exit candidates.

## Task integration and proof

Keep Config.Session nil, NoticeReady, existing pending-notice scheduling, original-call approval, and task fencing.
Engine alone drives parent commands/notices/queue Turns. Automation adds no second RunUntilQuiet caller.
Quiet requires children/callbacks, pending notices, pending Start, and parent queue clear.
Scan attention/unknown effects before Tasks.Recover can dispatch. Recover only permitted ready/interrupted work and stop remaining live children on attention.
Explicit --yes retains original-call policy. Stdin, formatting, and stderr guidance grant no approval.

Keyless proof covers whole literal input, no-admission failures, response order/equality/empty cases, and exclusion of summaries/children/intermediate/internal content.
Exercise ordinary plus parent notice responses, partial stops with useful text, empty tool-only completion, and no unrelated history on new/continued/saved-response routes.
Pin every exit, mixed limit/refusal/cancellation ordering against opposing IDs, equal-rank ties, and all attention guidance on stderr.
Resume completed content with invalid live bindings/storage. Interrupt/reopen counts one effect and notice; repeated saved-response reads dispatch nothing.
Prove slow/stalled/broken pipes and inherited TTY output, canceled readers/writers, restored flags/termios, untouched terminal stdin, and joined workers.
The CI fixture retains failed test status when Dashi successfully emits a diagnosis.

## Decisions

Choose complete-message JSONL over token previews so scripts need no content-correction/event protocol. This delays each message until its parent Turn stops.
JSON waits for all selected responses; both formats consume existing public answer text. Reject the execution-report DTO and a salvage debug/introspection flag.
