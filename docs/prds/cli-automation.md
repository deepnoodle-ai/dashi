# Run one coding request from a script

Status: Approved\
PRD PR: #145 · Implementation PR: Not opened · Updated: 2026-09-30

## Problem and context

A developer pipes a failed build log into Dashi and logs its response in CI.
They need response content they can consume reliably, plus an exit status when Dashi fails or needs attention.
The build/test command determines build success. Dashi completion certifies neither the build nor its diagnosis.

Today, `-p` ignores stdin. Line mode can split redirected input into requests or consume it as approval answers.
Banners and notices share stdout. Existing print mode emits the last `Outcome.Answer` as text.
This proposal adds whole-input handling and response formatting around that existing public answer content.
It completes O03/O04 in the [operational matrix](../research/cli-parity-operations.md). It does not complete O06 usage reporting.
This replacement supersedes earlier slice 8 execution-report and JSON/JSONL usage requirements in the [parity PRD](cli-parity.md).

## Users and response formats

A script author sends one request and consumes assistant responses.
An **invocation** is one CLI process that admits at most one new user request.
A **response message** contains the public answer text of a parent request or permitted parent notice response.
Machine stdout contains response messages only. Exit status and printable stderr carry errors, attention, and recovery guidance.

JSON returns an ordered message list:

```json
{"messages":[{"content":"The test failed: expected 3, got 4."}]}
```

JSONL streams one complete message object per LF-terminated line:

```json
{"content":"The test failed: expected 3, got 4."}
```

| Field | Script use |
| --- | --- |
| `messages` | Iterate responses in invocation order, including the empty-response case. |
| `content` | Read or log the assistant's text without terminal formatting. |

Assistant role is implicit. These are text responses already presented by the CLI.
This slice adds no typed-output, multimodal export, introspection flag, task schema, ledger, or API.
Inspection, task details, accounting, and recovery state remain in their existing surfaces.

## Use cases and acceptance

### UC-1: Send the whole build log

Run `cat build.log | dashi -p "Explain the failure" --output-format json`.

- [ ] Read redirected stdin to EOF before admission. Preserve Unicode, tabs, blank lines, trailing newlines, and slash-prefixed lines.
  Create one user-origin request, never a command/approval stream or one request per line.
- [ ] Combine exact prompt text, two newlines, and exact stdin when both contain bytes. Never trim.
  Stdin alone is valid. Reject empty/whitespace-only input before admission, except no-payload resume.
- [ ] Treat stdin as literal text. `/quit`, `yes`, `@file`, and attachment-like paths trigger no local command, approval, or file read.
  Parse mentions only from explicit `-p`; retain `--attach` validation and snapshot policy before combining stdin.
- [ ] Limit stdin to 16 MiB. Read failure, invalid UTF-8, NUL, or overflow fails without dispatch or shortened input.
  Cancellation during reading admits no request.
- [ ] Redirected stdin without `--plain` selects one-shot text. `-p` retains answer-only text and now includes redirected stdin.
  Explicit `--output-format text|json|jsonl` selects one-shot with any terminal arrangement. Never read terminal stdin.
- [ ] `--plain` without `-p` preserves the line/command/approval route. Reject explicit format with `--plain` or `--inline`.
  Fullscreen/inline interactive launch and other terminal-input routes retain their existing controls.
- [ ] Slash-prefixed `-p` with empty stdin permits local help/quit aliases and rejects other commands with composer guidance, without a provider call.
  Reject slash-prefixed `-p` with nonempty stdin before dispatch. Machine command output has no assistant message.

### UC-2: Consume response content

- [ ] JSON contains only `messages` and each message's `content`. JSONL contains only response-message objects with `content`.
  Both escape controls and preserve literal text, including multiline answers and Unicode separators.
  Neither emits ANSI, banners, status objects, tool/task events, accounting, or recovery snapshots on stdout.
- [ ] New/resumed execution emits one nonempty public parent answer per stopped request/notice Turn reached in that invocation, exactly once in invocation order.
  Include permitted task-notice answers presented by the foreground parent. Exclude unrelated earlier answers and direct child replies.
  Use the existing answer projection verbatim. Do not separately emit intermediate tool-loop messages, summaries, reasoning, or raw provider/tool content.
- [ ] Empty answers emit no message, including completed tool-only work. Never substitute an older nonempty answer or fabricate error/approval content.
  Available answer text on a partial, waiting, failed, or interrupted stop remains response content. Exit and stderr qualify unfinished work.
- [ ] JSON emits one object with `messages:[]` when no response exists and stdout is usable. JSONL emits no lines in that case.
  Malformed flags, unknown formats, `--help`, or unusable stdout may produce no machine document.
- [ ] JSONL delivers complete message objects when the parent response stops. It adds no token deltas, lifecycle markers, gap events, or final run envelope.
  A successful invocation yields the same ordered content in JSON and JSONL. Text retains last-answer-only behavior.
- [ ] Never silently drop content for a slow reader. Bound writing and cancellation; healthy output completes before exit.
  Broken/stalled/partial stdout fails nonzero, stops owned work, and prints saved-session/uncertainty guidance on stderr.
  Never retry effects or repeat a written prefix to reproduce output.
- [ ] Print sanitized errors, every affected attention target, and existing recovery guidance on stderr.
  Response delivery never overrides an execution failure or certifies the response's correctness.

CI logs content while retaining its build outcome:

```sh
build_status=0
go test ./... >build.log 2>&1 || build_status=$?
if [ "$build_status" -ne 0 ]; then
  dashi -p "Explain this test failure" --output-format json <build.log >response.json || true
  jq -r '.messages[].content' response.json || true
fi
exit "$build_status"
```

### UC-3: Interrupt and safely resume

- [ ] SIGINT/SIGTERM stop admission, cancel execution/readers, and close owned work through existing bounded shutdown.
  Deliver already-recorded selected response content when stdout remains usable. Hard termination cannot guarantee output or effect completion.
- [ ] Reopen validates workspace-scoped records under existing ownership rules before live setup.
  Completed or resolution-required work can return relevant saved parent content without credentials, tools, MCP startup, or attachment access.
  Required unreadable records and another owner fail explicitly. Never replace corrupt latest work with an earlier answer.
- [ ] Completed no-payload resume returns the relevant latest saved parent request/notice answer, not session history, without a new request or dispatch.
  Empty saved answer stays empty. Resolution-required work can return its available saved answer with nonzero exit and stderr guidance.
- [ ] No-payload continuation runs only existing ready/interrupted work. Its responses include newly reached parent request/notice answers, not earlier independent requests.
  Resume never doubles attempts, raises budgets, settles effects, discards work, or grants approval implicitly.
- [ ] New payload starts one request only after previous parent/child work permits admission. Its output excludes earlier saved replies.
  Preserve pending command identity after unknown acknowledgement and reconcile before resubmission.
  Revalidate destination, tools, resources, and immutable attachment evidence before live dispatch. No destination fallback.
- [ ] Stdin never supplies approval. Existing noninteractive denial, explicit `--yes`, and separate project MCP trust rules remain.
  Wait for permitted child/notice work to become quiet before success. Attention stops remaining children and identifies every affected target on stderr.
- [ ] Keyless reopen fixtures count one write effect and one notice. Repeated response-only reads perform no provider/tool/MCP/attachment activity.
  Recorded effect, ownership, and accounting authority remain intact without exporting them as response fields.

## Exit contract

The first invocation signal takes priority, then output/persistence failure, then preflight error or affected recorded work.
Recorded work ranks execution failure before required interaction, limit, refusal, and cancellation.
Equal ranks prefer original parent, then lexical notice/child Turn, task, and call IDs internally.
Stderr preserves all affected targets/remedies. A completed stop with a denied call remains completed.

| Code | Meaning |
| --- | --- |
| 0 | Invocation completed, child/notice work is quiet, and output succeeded. Local help/quit also succeeds. |
| 1 | Execution/setup/I/O failure, unreadable required records, or unknown persistence/delivery outcome. |
| 2 | Invalid flags/input, incompatible modes, or unsupported command before execution. |
| 3 | Approval/question/result/reconciliation or uncertain external effect needs attention. |
| 4 | Recorded limit, refusal, or cancellation prevents completion. No automatic remedy. |
| 130 / 143 | Invocation received SIGINT / SIGTERM. |

## Decisions

Use response-only JSON and complete-message JSONL. Reject an execution-report DTO and token/event stream that scripts must reconcile.
Keep whole-stdin compatibility, existing resume/effect authority, and bounded I/O. Update the CLI guide and prove the CI workflow with keyless fixtures.
