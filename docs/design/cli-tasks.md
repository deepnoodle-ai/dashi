# CLI task hosting and recovery

Status: Accepted · Author: Task track · Date: 2026-09-30\
Workflow: Spec, root checkpoint, then implementation. Named PRD re-review and root technical checkpoint approved.\
Requirements: [See and control delegated work](../prds/cli-tasks.md) · PRD PR: #148

## Ownership and narrow library seams

One CLI task host belongs to one open parent session. `delegate.Tasks` remains the sole child driver.
Engine remains the sole parent command/queue driver; UI events never run a Session directly.
Helpers live in `cmd/dashi/tasks.go`, `task_input.go`, `task_render.go`, and `approval_queue.go`.
Root schedules shared-file integration after session inspection. Keep Wonton, existing task tools, IDs, records, and spend contracts.

Add these opt-in seams to `delegate.Config` and `Tasks`:

```go
// Config additions; nil preserves current behavior.
PrepareStart func(context.Context, dashi.CallRef, dashi.Start) (dashi.Start, error)
BindServices func(context.Context, dashi.CallRef, dashi.Agent) (ChildServices, error)
NoticeReady func(dashi.TurnView) bool

type ChildServices struct {
    Client llm.Client
    ContentResolver dashi.ContentResolver
}
type TaskTarget struct { ID, TurnID string; Revision int64 }
func (*Tasks) Run(context.Context, TaskTarget, dashi.Command) (dashi.Outcome, error)
```

Run accepts only Continue, Settle, and Cancel with a nonempty CommandID.
PrepareStart runs outside `Tasks.mu` only for a new child, after resolving child tools and before launch/Start commit.
Give it an owned Start. Preserve CommandID, Parent, Origin, task prompt, child instructions, tool restrictions, limits, and empty caller History.
It may append input and pin settings, active-compaction policy, and behavior revision; reject changed protected fields.
Do not invoke it when a recorded child already exists.
BindServices runs for new/recovered children using the selected or recorded Agent; nil service fields inherit.
It cannot replace recorder, hooks, tools, observer, content limits, or context-ending policy.
Propagate setup/binding failures without dispatch or invented task success.

## Child input and immutable bindings

CLI PrepareStart loads/replays the parent Turn and verifies the original task call and derived ChildID.
Copy the parent's admitted generation settings with `NoInheritedSettings=true` and active-compaction policy.
Keep Explore/General instructions and attempt limits. Pin Provider/API/Model and CLI route fingerprint from that parent's recorded agent.
Verify the inherited tuple agrees; no current picker lookup or custom child catalog.

Append the parent's original Start attachment evidence and recorded instruction/source/catalog snapshot messages present at the task call.
Preserve roles, origins, and `llm.Message.Context` identity; copy no unrelated transcript or later composer draft.
Record this input in child Start. Child-aware resource hooks keep these initial snapshots and skip live rediscovery for seeding them.
Explicit skill/supporting-file tools retain their current validation/approval and record actual returned content.
Task prompt text never triggers file mentions.

Store child evidence beneath `PARENT/children/<sha256(ChildID)>/attachments/`.
Bind its logical session ID to ChildID, not its directory hash. Reuse the attachment track's private publication and bounded resolver helpers.
For each original parent ref, verify bytes through the parent session resolver, then publish a child-local snapshot before child Start.
Use deterministic `att_<32 hex>` IDs derived from parent ref ID/digest within this child namespace.
An existing publication is reused only after metadata, digest, size, and bytes agree; conflicts fail closed.
This makes crash-before-Start retries idempotent without an extra upload journal. Keep published orphan/uncertain content.
Child Start.Parent and copied evidence labels record provenance; application refs keep Source.Origin empty.

Bound copied files and aggregate evidence by the attachment limits, retain no source-path fallback, and authorize only this logical child session.
On reopen, use recorded child refs; never recopy changed parent/source bytes.
The inherited parent filerecorder still stores child Turns and fences them; no child session metadata journal is needed.

BindServices uses `routeClients.clientFor(recordedAgent)` to create a child-specific immutable client and the child-local resolver.
Never return the shared mutable foreground `turnClient`. Route fingerprint checks and credential trust apply before child preparation.
Changed/missing bindings stop the affected child; parent switches cannot redirect it.
Synchronize shared route/grant access; recovered project trust prompts carry explicit task attribution through the UI broker.
Hydrated complete child input reaches the preceding active-context/request guard, including copied media and resource snapshots.

## Exact child commands and fencing

Extend Tasks' existing worker ownership with a per-task command reservation.
Reject commands while that child has a worker, another command, or unresolved command acknowledgement; other tasks/parent stay usable.
Load/replay the exact target Turn and require it is the task's latest recorded Turn with matching Parent and expected Revision.
Known identical command retries return the recorded result before stale-revision rejection; conflicting command content fails normally.

Run the command through the child Runner on that frozen loaded Turn, sharing existing Tasks event collection and cancellation ownership.
Do not silently load a newer snapshot through Session.Run, allocate a new child Turn, or dispatch a child queue.
Recorder ExpectedSeq/owner rules fence any write after the target load; expose revision conflicts without speculative execution.
Record/replay the result into the existing task view, retaining recorded agent and restrictions.
Register command work under Tasks' cancel/done lifecycle so Stop/Close still govern it; do not create an independent child worker.

The CLI captures target revision/details when beginning each action and revalidates after operator confirmation.
Use `cli-task:` plus SHA-256 of canonical JSON `{turn, revision, action, call, payload}` as CommandID.
Payload contains the complete recorded command content, including settlement/approval facts, cancel reason, or exact limit raise; it excludes CommandID itself.
Pending approval responses use existing Pending builders and their bound deterministic IDs.
An unknown write retains that exact command and blocks other actions until reload confirms its matching digest, absence, or conflict.
Absence permits only the same command retry against its unchanged target; changed state requires a new inspection/action.
On process restart, derive state from records; no retained UI confirmation implies authority to retry.

| Action | Child behavior |
| --- | --- |
| `/task ID continue` | Continue ready/interrupted work. Pending approval opens its original attributed approval, records its answer, and requires all pending items settled. For `NextRaiseAttempts`, disclose checked doubling of the effective recorded cap; overflow rejects. Other limits retain their required remedy; no child monetary raise. |
| `/task ID done CALL_ID` | Require an already-started pending external/uncertain tool call. Display exact child Turn/tool/args and require operator affirmation of completion. Settle `tool.Succeeded("Operator verified this exact call completed; its output was not captured.")`; no tool execution, model settlement, or unexecuted approval bypass. Do not continue automatically. |
| `/task ID discard` | Cancel the stopped nonterminal child with recorded operator reason; uncertain effects may have run. Terminal/running/stale/wrong-call targets reject. |
| `/task-stop ID` | Existing live Stop, with cancellation confirmed from authoritative record; unresolved timeout/write remains visible. |

Continue with unresolved effects returns the existing not-ready/reconciliation result.
Refusal/failure requires the displayed external remedy before explicit continuation; terminal/deadline/accounting-unknown stops never revive.
Inspection never changes the target of parent Enter, `/done`, or `/discard`.
After Stop, reload even on apparent success: a synthetic task state or returned nil alone cannot prove cancellation was recorded.

## Progress, notice deferral, and queue ownership

CLI sets NoticeReady to `Stop.Terminal || Stop.Kind==completed || Stop.Kind==canceled`.
Nil retains existing Go consumers' stopped-Turn notice and callback behavior.
Under this policy, withheld nonterminal stops update task state/OnChild progress, without setting final `notified/reported` flags or consuming NoticeID.
Recheck on subsequent Run/reload; resolution/discard emits the one final notice.
Recover marks notices already present in parent records acknowledged and never rewrites/redelivers their historical content, even if later child settlement changes inspection.

For opted-in policy only, Wait returns a typed `AttentionError{Tasks []TaskView}` when no live/reporting work remains but withheld tasks require action.
RunUntilQuiet propagates it; do not hang unattended runs or report success with unresolved children.
Default Wait behavior stays unchanged. Child Start, partial progress, and stop states remain visible through OnChild/List and stable UI entries.

CLI sets Config.Session=nil and routes eligible OnTask results through a 128-entry in-memory pending-notice map keyed by existing NoticeID.
Each candidate retains the existing bounded notice text. If full, signal a rescan of recorded child Turns after space opens; never drop delivery or block a child callback.
This is retry state, not a journal: restart derives missing delivery from child and parent records.
Only one parent driver steers/enqueues notices and executes queue Turns; never do it on child observer goroutines.
Retain notices across enqueue failures until authoritative parent records establish delivery.
Session context/queue identity provides deduplication, including unknown queue-Start acknowledgement.
Before claiming quiet, check live children, attention states, pending notices, parent pending admission, and queue emptiness.
Pending Start, compaction, stopped parent work, or lost acknowledgement cannot be overtaken.

Render stable task rows/announcements by task/child Turn ID. Deltas remain provisional and never join the parent answer.
Label `TaskView.ModelCalls` as model attempts: it sums `TurnView.ModelAttempts`, not recorded dispatched calls.
Details retain recorded dispatch evidence, `NoDispatch`, and unresolved intents; interrupted dispatches cannot disappear behind the attempt count.
Inspection uses recorded child messages/metadata and the existing bounded viewer, never resolver reads.
Print progress/errors use stderr; stdout remains the existing answer contract.

## Approvals, lifecycle, and accounting

One cancellation-aware UI broker serializes parent/child approvals and questions.
Capture CurrentTurn/CurrentCall before UI presentation; each request retains original child Turn/call/args, task ID, scope, and response channel.
Before applying a response, revalidate that request and worker still exist. Cancellation removes only its request; stale answers are discarded.
Remembered permissions retain existing scopes. Task creation approval grants no child effects.
Controller foreground mode is derived from its actual worker/dialog owners, not restored unconditionally to running after a child prompt.

Recover once before unrelated admission, binding each original child. Ctrl-C interrupts only foreground work; explicit task-stop cancels a child.
Quit/new/session switch closes Tasks, waits for interrupted records, then releases tools/clients/session lock.
Shutdown timeout reports unresolved shutdown and admits no replacement session; no daemon promise.
Print waits through permitted notice Turns; AttentionError/parent resolution stops close remaining children and return non-success with exact action guidance.

`/status` and session `Inspection` retain own-session spend, including parent queue and summary Turns. Neither adds child reports.
`/tasks` and task details/report use TaskView.Spend and existing `delegate.Report` for separate own/children/inclusive amounts.
Across parent Turns, add each parent's own spend once and deduplicate children by recorded child Turn ID and Parent linkage.
Show parent/children/inclusive model money separately from tool money, preserving currency and completeness.
Live values are lower bounds. Explore/General attempt limits are not dollar caps.
Disclose any existing configured parent cap as parent-only. The CLI has no `--budget`; add no flag, child Budget, or family cap.
Automation formats response content only; spend scopes remain in the existing inspection and task-report surfaces.

## Alternatives, release, and proof

CLI-owned child runners duplicate delegation recovery. Shared parent-store access widens authority; source-path forwarding changes evidence.
Immediate unresolved notices consume final identity too early. The opt-in seams retain the library's current defaults at the cost of extra host wiring and copied disk content.

Rebase/publish after session inspection under root scheduling. Existing Go callers/records need no migration; update delegation, recovery, and CLI guides/examples.
Keyless fixtures cover child copies around Start crashes, immutable routes/resources, targeted command retries/fencing, concurrent/stale approvals, waiting/limited/uncertain resolution, and one final notice with one write execution.
Also prove historical acknowledged notice preservation, enqueue failure/unknown queue Start, truthful quiet/error exits, spend once after reopen, special-file/symlink/cross-session rejection, and effective media sizing.
Pin the PRD's USD fixture: own 3.50, children 5.50, inclusive 9; status remains 3.50 after reopen and repeated child callbacks.
Capture both Wonton modes at 80×24 and plain/print states. Run formatting, vet, build, and root/CLI race tests; required implementation, acceptance, security, and taste reviews remain gates.
No product question remains. Root approved these API and ownership seams; code/publication waits for the scheduled dependency checkpoint.
