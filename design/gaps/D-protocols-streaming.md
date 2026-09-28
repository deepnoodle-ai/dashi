# D. Protocols and live turn streaming

Source review of Dashi main at 9d28264 on 2026-09-27; findings are directions,
not decisions.

**Names:** written before [decision 30](../decisions.md); read `Definition` as
`Agent`, `Engine`/`Bindings` as `Runner`, `Accept` as `Settle`, and a command's
`ID` as `CommandID`.

Dashi's record model is already the right source for both protocols. It has a
gap-free per-turn `Seq`, steps emitted only after the recorder acknowledges
them, a message ID reserved before the model call, a separate step kind for each
fact, and richer outcomes than ACP or nvoken (uncertain, not executed, detached,
persistence unknown). Its weakness is the delivery surface. `Engine.Stream`
covers one invocation for one reader, is used once, and has no cursor.
`Observer` silently drops committed steps as well as previews. Previews lack the
identity fields (offset, tool-call ID, execution epoch) that nvoken relies on.
Lifecycle, approval, and failure facts are not self-describing enough to project
live without host-side interpretation. An application that must serve a UI and
resume it after reconnect can build this on Dashi today, but it must rebuild
the log reader, gap handling, phase fold, and preview bookkeeping itself.

Sources: ACP v1 1.23.0 / v2 alpha.5 via `tmp/research/acpresearch/`
(`acp-v2-and-streaming.md` §2–§6); nvoken spec
`nvoken-cloud/docs/design/streaming-protocol-specification.md` (R1–R55,
@deea133); Dashi `main` @9d28264.

## Model comparison

| Concern | ACP (v1 → v2 draft) | nvoken streaming protocol | Dashi today |
|---|---|---|---|
| Lifecycle | v1: `session/prompt` RPC is the turn. v2: prompt returns `{messageId}` on insertion; `state_update` running / requires_action / idle(+stopReason); session-scoped, no turn ID | `TurnChange.status`: queued, running, waiting, budget_hold → completed, incomplete, failed, cancelled; server-computed `terminal` flag; `current` marks latest revision | Steps `turn_started`, `turn_continued`, `approval_accepted`, … `turn_stopped{Stop}` (turn.go:118-134). No running/queued signal; phase is implicit in the step sequence. `Outcome` exists only at the end of `Apply`/`Stream` |
| Update kinds | message/thought/user chunks and upserts; `tool_call_update`, `tool_call_content_chunk`, terminal, plan, usage, commands, config, info | Durable `transcript.update{messages[], turn_changes[]}`; ephemeral `message.delta{text,thinking,tool_arguments}`, `stream.resync`, `connection.closing` | `Event{Step xor Delta xor Outcome}` (policy.go:73-83); `llm.Delta` kinds text, reasoning, tool_arguments, content, usage (llm/llm.go:231-245) |
| Identity & ordering | `messageId`, `toolCallId`; per-ID receipt order; cross-ID placement by first sight | Message `sequence` + change `(turn_id, revision)`, two gap-free counters per log; changes never precede their messages (R17) | One gap-free `Seq` per turn over all facts (recorder.go:85); `AttemptID`/`MessageID` minted before send (engine.go:564-565); tool call ID from provider |
| Resumption | v1 `load` = full replay, `resume` = none; v2 `replayFrom{start}` only; #2208 in-memory per-connection cursor | Opaque scope-bound cursor = exact position; "repeats nothing, skips nothing"; any number of readers; Turn and Conversation scopes | None in the API. The recorder holds the steps; `Seq` could act as a cursor, but there is no read-after/follow surface, and `Stream` is used once (engine.go:30-40) |
| Approvals | `session/request_permission` RPC bound to one connection; v2 `title`/`subject`; not re-issued | Approval is a host tool: Turn `waiting`, and the current change carries `arguments` and `deadline_at` (R24) | `tool_decision{defer}` then `turn_stopped{waiting}`; `Accept{Approval}` bound to args digest + policy revision (engine.go:239-248). No title, options, or deadline |
| Cancel | Notification; flush, then `idle{cancelled}`; client cancels pending permissions | Separate cancel op; terminal `cancelled` change; disconnect never cancels (R45) | `Cancel` command or breaking the range → `turn_stopped{canceled}`; late `model_settled`/`tool_settled` may follow the stop (state.go:130-136) |
| Terminal | Stop reasons end_turn, max_tokens, max_turn_requests, refusal, cancelled; **no failed** | `terminal: true` change is the only end signal (R22); Turn route closes `settled` | `StopKind` completed, waiting, limited, refused, canceled, failed, uncertain (turn.go:246-254). Only completed and canceled are final (state.go:603-606), and nothing on the stop says which |
| Usage | `usage_update{used,size,cost}`; per-turn usage ambiguous (#1860) | Summed disjoint buckets on the current terminal change | Per-attempt `Result.Usage` in `model_settled`, disjoint buckets (llm/usage.go:39); summed only by `Replay` (state.go:115-120); provisional `DeltaUsage` |
| Errors | JSON-RPC error; v2 custom `_…` stop | Pre-stream HTTP errors only; `TurnFailure{code (closed enum), message, details}` | `Stop.Reason` and `ModelFact.Error` are free strings, often `err.Error()` (engine.go:614-620); `Persistence` + `WriteError` on `Outcome` |

## ACP patterns

| # | Pattern | Status | Dashi evidence | Reference | Direction |
|---|---|---|---|---|---|
| 1 | Acceptance means the input was inserted (recorded) | Covered | `turn_started` committed before any work; steps are yielded only after ack (engine.go:158, 333-344) | v2 prompt-lifecycle §2 (acp-v2 §2.3) | State it as the contract: the first `Stream` event is the receipt |
| 2 | Lifecycle kept separate from content (`state_update`) | Partial | Only `turn_stopped` is explicit; running/resumed are implicit | acp-v2 §2.3, §4.1(2) | Add a folded `Turn.Phase()` (Running, AwaitingHuman, AwaitingExternal, Stopped{Final}) |
| 3 | `requires_action` while blocked, with a reason | Partial | `turn_stopped{waiting}` + `Pending` IDs (tools_loop.go:253); the reason is a string | acp-v2 §2.3 | Carry the wait kind and subjects on the stop or phase |
| 4 | Agent-minted IDs, stable across replay | Partial | `MessageID` recorded (turn.go:199); no IDs for input messages, delivered context, or thought blocks | acp-v2 §2.3, §6.3 identity rules | Derive IDs as a pure function of `(TurnID, Seq, index)` and document it |
| 5 | Whole-entity replace heals lost previews | Covered | `model_settled` carries the full `llm.Result` | acp-v2 §2.9(2) | Projection emits a whole upsert at commit |
| 6 | Tool call visible before the permission request | Covered | `tool_decision` step carries `Call` before the stop (tools_loop.go:152) | acp-issues #851 | — |
| 7 | Honest in-progress tool status | Covered | `tool_started` committed before execution (tools_loop.go:172, 216) | acp README "deeper dives" | — |
| 8 | Permission `title`/`description`/`subject`/options (allow/reject once/always) | Missing | `ToolDecision{Kind,Revision,Reason,Rewrite}` (policy.go:35-40); `ApprovalFact.Approve bool` | acp-v2 §2.5 | A `WaitInfo` on defer/wait steps; "always" stays host policy |
| 9 | Approval bound to recorded arguments; unknown outcome ≠ approve | Ahead | Args digest + policy revision checked (engine.go:243-245) | acp-v2 §2.5 "What this does not fix" | Keep; recommend it in the adapter |
| 10 | Cancel: flush, then a single terminal signal | Deliberately different | Late facts are allowed after `turn_stopped{canceled}` (state.go:130-136) | acp-v2 §2.3 Cancellation | Projection must treat post-stop settles as tool updates, not a re-opened turn |
| 11 | Failure is a stop reason | Ahead (kind) / Partial (code) | `StopFailed`, `StopUncertain` exist, but reason is free text | acp-v2 §4.2(6) | Typed reason codes (see gap 5) |
| 12 | Resume = replay, then state sync, then re-issue pending approvals | Partial | `Replay(t)` yields `TurnView.Pending`/`Stop` (state.go:66); no replay-from-Seq API or approval payload to re-issue | acp-v2 §2.6, §6.3 replay rules | Provide `StepsAfter(seq)` + phase snapshot |
| 13 | Tool content streaming / terminal snapshot + chunk | Missing | `Executor.Execute(ctx, Call) (Outcome, error)` has no progress sink (tool/tool.go:312-315) | acp-v2 §2.4 `tool_call_content_chunk`, `terminal_*` | Optional progress sink carrying `(CallID, index)` as a preview class |
| 14 | Open enums, preserve unknown variants | Partial | Replay rejects unknown step kinds (state.go:537), although turn.md says to retain them | acp-v2 §4.3(4) | Retain-but-block unknown kinds, as design/turn.md says |
| 15 | Plans, commands, config, session info | Deliberately different | Out of turn scope | acp-v2 §2.4 | Session or host layer |
| 16 | Tool argument streaming | Ahead (ACP has none) / Partial | `DeltaToolArguments` exists, but carries no call ID | acp-v2 §4.2(6) | See gap 3 |
| 17 | Detached / background work | Ahead | `tool.StateDetached` + `Handle` (tool/tool.go:241) | acp-issues #1847 | Map to `in_progress` + `_meta` |

## Streaming protocol mapping

Status legend: **OK** = derivable losslessly from Dashi; **Host** = Dashi has
no concept, so the host must supply it (acceptable); **Gap** = no source, or not
reliably available.

| nvoken frame / field | Dashi source | Status |
|---|---|---|
| Log position, `cursor` | `Step.Seq` (turn scope). No conversation-scope counter | OK (turn) / Host (conversation) |
| `transcript.update` delivery, exactly once, in order | `Stream` yields acked steps in `Seq` order; `Observer` may drop them (observer.go:23-26) | Gap for multi-reader or observer delivery |
| `has_more`, replay page | No read-after-`Seq` API; `Recorder` has only `Commit` (recorder.go:19-21) | Gap (host builds it) |
| `ConversationMessage.id` for user and tool rows | `StartFact.Input`/`ContextItem`/`tool_settled` have no IDs | OK only through a documented derived ID |
| Assistant message `id` = preview `message_id` (R26) | `ModelFact.MessageID` on requested/settled + `Event.MessageID` | OK |
| `role`, `content`, `tool_use`, `tool_result`, `reminder` blocks | `Project` and step payloads (project.go:13) | OK. Per-step projection helper missing |
| `redacted` / no stored reasoning (R19, R29) | Reasoning blocks are kept in `Result` for provider replay | Host must redact in projection |
| `sequence`, `created_at` | `Seq`, `Step.At` | OK |
| `user_key`, `agent_id` | `StartOrigin`/`ContextItem.Origin` only | Host |
| `TurnChange.status` queued | — | Host |
| running | Inferred from `turn_started`/`turn_continued`/accept steps | Gap: implicit only |
| waiting | `turn_stopped{waiting}` | OK |
| budget_hold | `StopRefused` from `BeforeSend` (engine.go:557-559), or a host recorder rejection | Partial |
| completed / incomplete / failed / cancelled | completed; limited → incomplete; failed; canceled. **uncertain** has no nvoken status | OK, with a mapping choice for uncertain |
| `terminal`, `current` | Not on the stop. Final = completed or canceled (state.go:603-606); canceled still accepts late facts | Gap: consumer must hard-code (violates R21) |
| `stop_reason` enum | `Stop.Reason` strings "model_attempts", "deadline", "output", "context", "provider_pauses" (engine.go:408-685) | Partial: uncontracted strings |
| `error{code,message}` | `Stop.Reason` = free `err.Error()` | Gap: no code |
| `through_message_sequence` | Implicit: one `Seq` counter | OK (simpler) |
| `tool_calls[]` id, name, status | `TurnView.Tools` (state.go:25-31): requested/allowed→pending, started/waiting→running, settled→completed/failed, not_executed→cancelled, uncertain→failed | OK via fold |
| `mode` | `tool.Definition.Kind` (external vs local); no mcp/callback | Partial |
| `arguments`, `deadline_at` while settleable | `Call.Args` yes; per-wait deadline no (only `Limits.Deadline`) | Partial |
| `usage` summed, `provenance` | `Replay` sums `modelUsages`; `ModelFact` provider/API/model | OK |
| `structured_output`, `final_answer_message_id` | `output_recorded` step; last `model_settled.MessageID` when completed | OK |
| `message.delta.attempt` (execution epoch) | `AttemptID` is a model call, not a lease epoch; `Owner` is not recorded on steps or events | Gap / Host |
| `content_index` | `Delta.Index` (provider block index, same caveat as nvoken) | OK |
| `offset` | — (host can count only if it sees every delta) | Gap |
| `kind` text / thinking / tool_arguments | `DeltaText`/`DeltaReasoning`/`DeltaToolArguments` | OK |
| `tool_call_id`, `name` on tool_arguments (R27) | Only Anthropic emits a block-start `DeltaContent` (providers/anthropic/messages/stream.go:151); OpenAI and Gemini send only `Index` | Gap |
| `emitted_at` | — | Gap (trivial) |
| `stream.resync` | No gap signal; observer drop is silent (observer.go:23-26) | Gap |
| Preview voiding on a failed call | `model_settled{Error}` or turn stop | OK (implicit slot close) |
| `connection.closing{settled}` | `Outcome` event ends `Stream`; no end on break (engine.go:30-34) | Host |

## Top gaps

**1. No resumable, multi-reader view of a turn.** `Engine.Stream` is one reader,
one invocation, and one use. A turn that waits and resumes through `Accept`
produces a new stream on each call, and a reconnecting UI has no "steps after
`Seq` N, then follow" operation. nvoken's core guarantee (exact cursor, replay
then live, any number of readers, disconnect never cancels) has to be rebuilt by
every host. Worse, breaking the `Stream` range *cancels the turn*
(engine.go:30-34). A UI transport that ties the turn to its reader therefore
violates R45. Direction: document `Seq` as the turn-scoped cursor, add an
optional recorder read (`StepsAfter(turnID, seq)`) and a post-commit wake hook,
and steer UI serving toward "run detached, follow by cursor", not toward ranging
`Stream` from a request handler.

**2. The observer silently drops committed steps and never reports gaps.**
`emitter.send` drops any event when its 64-slot buffer is full
(observer.go:19-26). This contradicts design/agent-loop.md:99-100 ("an observer
receives isolated committed events and bounded, lossy previews"). Nothing tells
the consumer that it lost data, so it cannot emit `stream.resync` or refetch.
Direction: never drop a committed step (or split a lossless post-commit hook
from a lossy preview hook), and emit a `Gap` signal when previews are dropped.
This is the three-class split (committed, preview, signal) from acp-v2 §6.1.

**3. Preview identity is incomplete.** Deltas carry `TurnID`, `AttemptID`, and
`MessageID`, but not a byte `offset`, not the tool-call ID or name on
`tool_arguments` (except Anthropic's block-start content), not an emission time,
and not an execution epoch that voids previews from a dead worker. The host
cannot detect lost or repeated fragments, and cannot attach argument previews to
the right tool card for OpenAI or Gemini. This is nvoken's known "discrepancy 1"
in a new place. Direction: have the provider adapters emit a normalized
block-start event with call ID and name, add `Offset` to deltas, and carry an
owner or epoch on events.

**4. Lifecycle is not self-describing.** Dashi has no running signal, no
`terminal`/final flag, and no folded phase. Waiting, limited, failed, refused,
and uncertain are resting stops that `Continue` or `Accept` can reopen. Canceled
accepts late settles after its stop. A consumer must hard-code which stops are
final, which is exactly what nvoken R21 forbids, and must infer "running again"
from step kinds. Direction: add `Stop.Final` (or `Turn.Phase()`), and name the
resuming steps explicitly so projections can synthesize `state_update{running}`
or a `running` change without guessing.

**5. Approval, wait, and failure payloads are not display-ready or
machine-coded.** A deferred decision has only `Reason`. `tool.Wait` has `Reason`
and `Prompt`. Neither has a title, options, subject, or per-wait deadline, so
ACP `request_permission` and nvoken `deadline_at` have no source. `Stop.Reason`
and `ModelFact.Error` are free text, often `err.Error()`, so neither
`TurnFailure.code` nor ACP stop reasons can be mapped reliably. Direction: add a
small `WaitInfo{Kind, Title, Description, Options, Deadline}` on defer and wait
steps, and give stop reasons a typed code with open-enum rules, keeping the free
text as the message.

## Already strong

- **One counter for every fact.** A single gap-free `Seq` orders content and
  lifecycle together. The messages-before-changes rule and
  `through_message_sequence` that nvoken needs for two counters come for free.
- **Emit after acknowledgement.** Steps reach consumers only after
  `Recorder.Commit` succeeds. That matches both ACP's "acceptance means
  insertion" and nvoken's "durable frames come from the log".
- **The preview-to-record handoff is built in.** `MessageID` is reserved in
  `model_requested` and repeated in `model_settled`, which is the nvoken R26
  contract. Each model call is its own recorded attempt, so there is no
  stable-vs-new-ID ambiguity on retry.
- **Richer outcomes than either source:** `uncertain`, `not_executed`,
  `detached`, `waiting`, `limited`, and a failed stop (ACP has none), plus
  `Persistence` as a separate state (acknowledged, rejected, unknown).
- **Approval binding:** an approval is checked against the argument digest and
  policy revision. ACP explicitly leaves this to implementers, and nvoken does
  it only through host-tool IDs.
- **Idempotent commands** (`CommandID` + digest) fix ACP v2's
  lost-prompt-response ambiguity and match nvoken's idempotency key.
- **Deltas never change what is saved.** `llm.Stream` and `Send` persist
  identical results (nvoken R42). The iterator's backpressure suits in-process
  callers. It is the wrong coupling for a network UI, where nvoken disconnects a
  slow reader instead of stalling execution (see gap 1).
