# See and control delegated work

Status: Draft\
PRD PR: Not opened · Implementation PR: Not opened · Updated: 2026-09-30

## Problem and context

A developer asks for parallel investigation while fixing a bug. They need to know which task is working, what it found, and whether it needs approval.
The CLI currently wires no task host, task inspection, or lifecycle recovery.
Delegating through another terminal loses the parent request's evidence, approval context, and recorded relationship.

[`delegate.Tasks`](../../delegate/tasks.go) already supplies background `task`, `task_status`, and `task_stop` tools, stable task IDs, child records, event callbacks, cancellation, and recovery.
Its [guide](../guides/delegation.md) defines record-backed completion notices and separate parent/child spend.
This track completes [O08 / slice 9](../research/cli-parity-operations.md) by hosting that foundation in the CLI.

[Claude Code](https://code.claude.com/docs/en/sub-agents) surfaces a background child's approval in the main session and identifies the child.
[Pi request #552](https://github.com/earendil-works/pi/issues/552) asks for observable, abortable delegation with persisted results; it is closed, not evidence of a shipped core feature.
[OpenHands complaint #4537](https://github.com/OpenHands/software-agent-sdk/issues/4537), open when checked on September 30, reports a parent lock held during delegation that freezes its conversation UI.
These sources support visible controls and responsiveness; they do not establish a defect in Dashi's existing library.

## Users and concepts

The primary user works in an interactive coding session; print users need task results before the process exits.
**Task** is focused work started by the agent's recorded `task` call, performed in a child session with a stable ID.
**Child** is the agent doing that task, with its own Turn, tools, model calls, limits, and recorded effects.
**Task notice** is the recorded contextual message that tells the parent a child Turn ended; it grants no user authority.
**Parent spend** covers the parent Turn's own model calls. **Child spend** covers child Turns. **Inclusive spend** adds both once.

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
- [ ] Each row shows ID, description, subagent, actual state, model/tool-call counts, and child spend completeness.
  Distinguish live progress from recorded facts, and approval pending from ordinary work.
  Listing/scrolling/input stay responsive when a child blocks on a model, tool, or approval.
- [ ] `/task ID` shows parent link, current/last child Turn, supplied evidence/resources, destination, limits, stop reason, answer, and recorded tool results.
  Bound and disclose displayed truncation; the existing transcript viewer can inspect recorded detail without resolving attachments or repeating effects.
  Running tasks show progress rather than a fabricated answer; failed, limited, waiting, uncertain, refused, interrupted, and canceled states retain their actual reason.
- [ ] Screen and inline modes show compact task progress without mixing child deltas into the parent's answer.
  Plain mode shows equivalent named task updates. Preserve draft, dialog focus, transcript position, and existing reasoning-visibility controls.
- [ ] Each child Turn's result reaches the parent through the existing notice identity.
  Reading `/task` or `task_status` does not mark the notice delivered.
  Reopen or repeated callbacks do not append a duplicate completion announcement or duplicate recorded notice; historical display can restore the original entry.
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
- [ ] Fault fixtures cover crashes around child start, tool settlement, notice enqueue, and notice recording.
  Repeated recovery yields one child for the original parent call, one recorded completion notice per child Turn, and no repeated settled effects.
- [ ] Task details and session inspection distinguish parent, child, and inclusive model spend, with tool money separate.
  Inclusive totals count each recorded child Turn once, including after resume, and include scheduled parent queue-Turn spend in session totals.
  Running totals are lower bounds. Preserve unpriced-call, accounting-unknown, and usage-incomplete indicators without mislabeling exact money.
- [ ] Display child attempt/turn limits and whether it has a monetary cap.
  Existing Explore/General defaults bound attempts, not dollars; this slice adds no child monetary cap.
  Parent limits and budgets remain parent-only. Any `--budget` help, startup, and task detail must explicitly exclude child spend; never call it a family/session cap.
  No child budget is inferred from parent remaining money or inclusive reports.
- [ ] Print mode waits for pending children and permitted notice queue work before successful exit, using the existing quiet/wait contract.
  A parent stop that requires resolution interrupts remaining children before exit and preserves records for reopen.
  Existing non-interactive approval/`--yes` behavior applies to each original child call; child progress/diagnostics add no unsolicited stdout output.

## Decisions and limits

- Host existing task tools and built-in subagents; start through an actual parent agent tool call. Reject a new manual spawn protocol without a recorded parent call.
- Supply admitted request evidence and resource snapshots, not a transcript fork. This makes screenshot/document tasks useful without filling each child with unrelated history.
- Keep original call approval and one session task host. Reject global mutable child destination binding, display-only approval identity, and independent task notice journals.
- Preserve separate per-Turn budgets and existing child limits. Family budget reservation, child-cost policy flags, and a family ledger require another contract.
- Keep task controls immediate and read-only inspection free of model dispatch. Rich messaging, `task_send`, message-parent tools, custom agent catalogs, grandchildren, and tasks outliving the process are outside this slice.

## Success and risks

Keyless acceptance completes a parallel investigation, approval, targeted stop, and reopen with stable evidence and exactly one result delivery.
Counting fixtures prove no repeated settled effects and accurate spend after recovery. Actual Wonton recordings cover both modes at 80×24, including empty/loading/error/approval states.
Implementation, acceptance, security, and taste reviews must pass; preserve composer, attachment, MCP, provider, context, and session-inspection behavior.

Children share the workspace and can conflict; delegation grants no isolation or rollback.
Inherited runner fields are insufficient by themselves: CLI destination revision, child-scoped evidence storage, resource snapshots, and approval UI require explicit integration.
The technical spec must settle those bindings and single-driver queue ownership before implementation.
No product question blocks the draft. Stop for named independent PRD review.
