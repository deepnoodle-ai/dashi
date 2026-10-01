# See and control delegated work

Status: Approved\
PRD PR: #148 · Implementation PR: Not opened · Updated: 2026-09-30

## Problem and context

A developer delegates investigation while fixing a bug. They need progress, results, and attributable approvals.
The CLI has no task host, inspection, or recovery; another terminal loses the request's recorded relationship and evidence.

[`delegate.Tasks`](../../delegate/tasks.go) supplies the task tools, stable IDs, records, progress, cancellation, and recovery.
Its [guide](../guides/delegation.md) defines recorded notices and separate parent/child spend.
This track completes [O08 / slice 9](../research/cli-parity-operations.md) by hosting that foundation in the CLI.

[Claude Code](https://code.claude.com/docs/en/sub-agents) surfaces a background child's approval in the main session and identifies the child.
[Pi #552](https://github.com/earendil-works/pi/issues/552) requests observable, abortable, persisted tasks; it is a closed request, not shipped-feature evidence.
[OpenHands #4537](https://github.com/OpenHands/software-agent-sdk/issues/4537), open on September 30, reports delegation freezing the parent UI.
These support responsive controls, not a claimed Dashi defect.

## Users and concepts

Interactive users control delegated coding work; print users need results before exit.
**Task** is focused work started by a recorded `task` call, with a stable ID.
**Child** performs it in a separate recorded Turn with its own limits.
**Task notice** is contextual result delivery, without user authority.
**Parent spend** covers parent model calls; **child spend** covers child Turns; **inclusive spend** adds both once.

## Use cases and acceptance

### UC-1: Delegate a focused investigation and keep working

Ask “Have the explorer locate the parser and its tests while you investigate this failing request.”
The agent starts a task and gets its ID; the developer sees the same task while foreground work continues.

- [ ] Offer the existing Explore and General task tools to the parent. Explore offers only tools with read-only annotations; unknown annotations are excluded.
  General uses the existing parent tool restrictions. Neither child receives task tools or creates grandchildren.
- [ ] Starting work shows stable ID, subagent, description, parent Turn/call, and working state only once the child record establishes it.
  A failed or uncertain child admission shows its recorded state and correction; it cannot claim a second child started successfully.
- [ ] The child's recorded Start contains its focused prompt, the original parent request's selected attachment evidence, instruction snapshot, and skill catalog snapshot.
  It does not receive the parent's conversation transcript, later draft selections, or attachment-looking text as new file reads.
  The task details identify supplied evidence and resources without opening files.
- [ ] Before child Start, copy the exact verified parent attachment bytes into that child's private immutable session storage.
  New child-scoped references preserve the digest and recorded parent provenance, including after the source changes or disappears.
  Each resolver retains strict same-session authorization; the child receives no general access to its parent's store.
  Unavailable evidence stops explicitly rather than silently omitting it.
- [ ] Child destination and controls are fixed from the admitted parent/task selection, with the child's existing task-specific instructions and attempt limits.
  A later parent model switch cannot redirect a running or recovered child.
  Missing credentials, changed destination binding, or missing tools reports the affected task; no fallback widens access.
- [ ] Parent tool-root, MCP approval, and supporting-file policy still apply to child calls.
  Resource changes cannot silently replace the child's initial recorded instruction/catalog snapshot during continuation.
  Explicit later skill loads retain existing policy and record the content they actually return.

### UC-2: See progress and inspect the result

Use `/tasks` while work runs, then `/task ID` to inspect that task's evidence, answer, and stop reason.

- [ ] `/tasks` works during foreground work and when idle, without model dispatch.
  With no tasks, show “No tasks in this session.” Unknown IDs and invalid arguments preserve the draft and list usable controls.
- [ ] Each row shows ID, description, subagent, actual state, model attempts, tool-call count, and child spend completeness.
  Model attempts retain the existing task count; details distinguish recorded dispatch evidence, no-dispatch attempts, and unresolved intents.
  Distinguish live progress from recorded facts, and approval pending from ordinary work.
  Listing/scrolling/input stay responsive when a child blocks on a model, tool, or approval.
- [ ] `/task ID` shows parent link, current/last child Turn, supplied evidence/resources, destination, limits, stop reason, answer, and recorded tool results.
  Disclose truncation; the transcript viewer inspects records without resolving bytes or repeating effects.
  Running tasks show progress, not answers; all stopped states retain their actual reason.
  For stopped work, show the exact child Turn/call details and permitted task-targeted resolution commands.
- [ ] Screen and inline modes show compact task progress without mixing child deltas into the parent's answer.
  Plain mode shows equivalent named task updates. Preserve draft, dialog focus, transcript position, and existing reasoning-visibility controls.
- [ ] Each child Turn's result reaches the parent through the existing notice identity.
  Reading `/task` or `task_status` does not mark the notice delivered.
  Reopen or repeated callbacks do not append a duplicate completion announcement or duplicate recorded notice; historical display can restore the original entry.
  Show unresolved/nonterminal stops as task progress; hold final result delivery until resolution or discard.
  Never rewrite or redeliver an already recorded notice; inspection still shows the child's current recorded result.
  This is an opt-in CLI host policy; existing Go consumers retain their notice behavior.
- [ ] If the parent is idle, a notice uses the existing scheduled queue-Turn route, not a fabricated user prompt.
  Delivery waits behind existing pending admission, compaction, or stopped work requiring resolution.
  Show undelivered results and their blocked delivery state. Starting delivery preserves the independent composer draft.

### UC-3: Decide a child's call and stop one task

A General child asks to edit a file. Inspect its task ID and exact call before answering.
Stop an unnecessary investigation with `/task-stop ID` while other work continues.

- [ ] Every approval identifies the task, child Turn, original child call, tool, exact arguments, and the existing preview/scope.
  A queued prompt retains those identities. Allow-once applies only to that original call; existing remembered grants retain their documented scope and lifetime.
  Approving task creation does not approve the child's later effects.
- [ ] Parallel parent/child approvals and questions serialize through the actual UI in both modes.
  Cancellation removes the matching prompt, and stale answers cannot authorize another child or call.
  Closing one prompt restores the true foreground/idle state; a child approval cannot invent a running parent.
- [ ] `/task-stop ID` operates during foreground work or idle and cancels only that task through existing `Tasks.Stop` semantics.
  Report cancellation after the record confirms it. A timeout or ambiguous write reports unresolved stopping rather than completed cancellation.
  Already-ended and unknown tasks produce clear harmless results; they do not restart work.
- [ ] Stopping a task removes its outstanding approval without consuming the next prompt's answer.
  Completed effects stay recorded; stop does not promise rollback or proof that an uncertain effect never ran.
- [ ] Ctrl-C interrupts the foreground parent while children continue, with a visible count and stop instruction.
  Quit, `/new`, or session switch closes the current task host and settles/interrupts children before releasing its resources.
  Reopen may resume those interrupted tasks; none continues as a daemon after process exit.

### UC-4: Reopen safely and understand the cost

Quit with a task running, reopen the session, inspect its progress, and receive its result once.

- [ ] Recover existing child sessions from parent call records before admitting unrelated new work.
  IDs, destinations, evidence, restrictions, settled model results, tool outcomes, and spend survive reopen.
  Missing/corrupt records identify the affected task and preserve uncertainty instead of inventing success or a new child.
- [ ] Existing recovery resumes only unfinished/interrupted child Turns; canceled and ended Turns do not restart.
  Waiting/uncertain effects remain visible under existing reconciliation rules. Recovery does not replay a settled write/test merely to rebuild progress.
- [ ] `/task ID continue` targets only that stopped child Turn using existing recorded continuation and fencing.
  Resume ready/interrupted work; for a model-attempt limit, disclose and record the existing doubled attempt-cap continuation.
  Other limits report their required remedy; no implicit budget raise or revived terminal Turn.
  A waiting approval presents its original bound approval; unanswered external/uncertain effects block continuation rather than retrying them.
- [ ] `/task ID done CALL_ID` shows the exact child Turn, original tool, and arguments, then requires affirmation that the operator verified this call finished.
  Record the existing host-supplied settlement with output explicitly unavailable; invent no captured output and execute no tool.
  It applies only to an already-started waiting/uncertain call, never an unexecuted approval or a model attempt.
  Show `/task ID continue` once all required settlements make the child ready.
- [ ] `/task ID discard` records cancellation of that stopped child Turn, acknowledging that its uncertain effects may have run.
  Live cancellation remains `/task-stop ID`. Completed/terminal, wrong-call, stale-state, and concurrent-owner actions reject without dispatch.
  Each action reloads and fences its displayed target; ambiguous command writes require reconciliation before another action.
  Parent `/done`, `/discard`, and empty Enter always keep their parent target; inspection creates no hidden target selection.
- [ ] Reopen an uncertain child write, inspect the original call, mark that exact call done, and continue to its result.
  Count one write execution and one recorded result notice; repeat recovery/actions cannot add another effect or notice.
  Separate fixtures exercise waiting approval/result resolution, model-attempt-limited continuation, unsupported limit remedies, discard, and wrong/stale IDs.
- [ ] Fault fixtures cover crashes around child start, tool settlement, notice enqueue, and notice recording.
  Repeated recovery yields one child for the original parent call, one recorded completion notice per child Turn, and no repeated settled effects.
- [ ] `/status` retains own-session spend, including parent queue and summary Turns, and excludes child totals.
  `/tasks` and task details/report supply separate parent, child, and inclusive amounts through existing `delegate.Report` facts.
  Inclusive totals add parent own spend and each recorded child Turn once, including after resume.
  Running totals are lower bounds. Preserve unpriced-call, accounting-unknown, and usage-incomplete indicators without mislabeling exact money.
- [ ] Fixture: parent ordinary/queue/summary model spend is USD 2/1/0.50; two distinct child Turns cost USD 4/1.50.
  `/status` shows own USD 3.50; task reporting shows own/children/inclusive USD 3.50/5.50/9.
  Reopen and repeated child callbacks retain those amounts. Tool money stays separate.
- [ ] Display child attempt/turn limits and whether it has a monetary cap.
  Existing Explore/General defaults bound attempts, not dollars; this slice adds no child monetary cap.
  Existing configured parent caps disclose that they exclude child spend; never call them family/session caps.
  The CLI has no `--budget` flag. Add no budget flag or child/family cap, and infer none from parent remaining money or reports.
- [ ] Print mode waits for pending children and permitted notice queue work before successful exit, using the existing quiet/wait contract.
  A parent or child stop that requires resolution interrupts remaining children before exit, reports the exact action, and preserves records for reopen.
  Existing non-interactive approval/`--yes` behavior applies to each original child call; child progress/diagnostics add no unsolicited stdout output.

## Decisions and limits

- Start existing built-in tasks through recorded parent calls; no manual spawn protocol.
- Supply admitted evidence/resources without unrelated history; no transcript fork.
- Keep original call approval and one session task host; no mutable child routing or notice journal.
- Preserve per-Turn budgets and child limits; no family ledger, reservation, or child-cost policy flags.
- Immediate inspection dispatches no model. Rich messaging, custom catalogs, grandchildren, and daemons are outside scope.
- Resolve stopped children through explicit task IDs and original calls, using existing Continue/Settle/Cancel commands. A narrow host resolution/reporting seam is permitted; a new messaging protocol is not.

## Success and risks

Keyless acceptance proves parallel work, approval, stop, resolution, and reopen with stable evidence, effects, spend, and one result delivery.
Wonton recordings cover both modes at 80×24 and empty/loading/error/approval states.
Required implementation, acceptance, security, and taste reviews preserve preceding CLI workflows.

Children share the workspace and can conflict; delegation grants no isolation or rollback.
Inherited runner fields are insufficient by themselves: CLI destination revision, child-scoped evidence storage, resource snapshots, and approval UI require explicit integration.
The technical spec must settle those bindings, task resolution/reporting, and single-driver queue ownership before implementation.
Named independent re-review returned ready. No product question remains; the [technical spec](../design/cli-tasks.md) requires root checkpoint before implementation.
