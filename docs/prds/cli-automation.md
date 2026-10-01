# Run one coding request from a script

Status: Approved\
PRD PR: #149 · Implementation PR: Not opened · Updated: 2026-09-30

## Problem and context

A developer asks Dashi to explain a failed build log in CI.
CI needs a fixed run report: did Dashi finish, what did it answer, and does it need attention?
The build/test command determines build success. Dashi completion does not certify the build or its diagnosis.
Today, `-p` ignores stdin. Without `-p`, line mode creates separate requests and can consume later lines as approval answers.
Its banners and notices share stdout. Print mode returns only an answer and collapses all other stops to exit code 1.
The script cannot reliably identify its recorded work, missing usage, or the action needed after interruption.

This completes O03/O04 and slice 8's reporting share of O06 in the [operational matrix](../research/cli-parity-operations.md).
[Claude Code](https://code.claude.com/docs/en/headless) combines piped data and prompts. [Pi](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/json.md) separates JSONL progress from settled work.
Use those workflows with Dashi's recorded authority.
Reuse the [accounting contract](spend-usage-accounting.md), composer command boundary, and preceding attachment, inspection, active-context, and task tracks.

## Users and concepts

Scripts/CI send one request, log its answer, and detect unfinished Dashi work.
**Invocation** is one CLI process. It admits at most one new user request.
**Result** is a fixed CLI run report. Child/notice Turns retain separate identities.
**JSONL** is one JSON object per LF-terminated line. Progress is provisional. The final result contains the authoritative recorded snapshot.

## Use cases and acceptance

### UC-1: Send the whole build log

Run `cat build.log | dashi -p "Explain the failure" --output-format json`.

- [ ] Read nonterminal stdin to EOF before admission. Preserve Unicode, tabs, blank lines, trailing newlines, and slash-prefixed lines.
  Create one user-origin Turn, never a command/approval stream or one Turn per line.
- [ ] Combine exact `-p` text, two newlines, and exact stdin when both contain bytes. Do not trim.
  Stdin alone is valid. Reject empty/whitespace-only combined input before admission.
- [ ] Stdin is literal text: `/quit`, `yes`, `@file`, and attachment-like paths in it cause no local command, approval, or file read.
  Explicit `-p` attachment mentions and `--attach` retain the attachment track's validation and snapshot policy.
  Only parse that explicit prompt for mentions, before combining it with stdin.
- [ ] Limit stdin to 16 MiB. Read failure, invalid UTF-8, NUL, or overflow fails before model/tool dispatch.
  Never silently shorten or omit failed input. Signal cancellation during reading admits no request.
- [ ] Nonterminal stdin without `--plain` selects one-shot text, with or without `-p`.
  Explicit `--output-format text|json|jsonl` selects one-shot with any terminal arrangement, never reading terminal stdin.
  `-p` keeps answer-only text output and now includes redirected stdin.
- [ ] `--plain` without `-p` intentionally retains the existing line/command/approval route.
  Reject `--plain` or `--inline` with any explicit output format.
  Fullscreen/inline interactive launch and terminal-input line mode otherwise retain their routes and controls.
- [ ] A slash-prefixed `-p` remains a command-only invocation when stdin contains no bytes.
  Help and quit aliases run locally. Other commands reject with the composer's actionable guidance. None calls a provider.
  Reject slash-prefixed `-p` plus nonempty stdin before dispatch, rather than silently interpreting either as a model request.
  Machine formats return a command/error result with no fabricated Turn identity.

### UC-2: Parse the result and its cost

`--output-format json|jsonl` selects CLI report framing, not a task-specific diagnosis schema.
Optional `structured_output` passes through an already-recorded typed model answer. This CLI slice adds no task-schema selection.

Trimmed proposed reports omit snapshots, totals, persistence, warnings, and stop/action details.
Dashi completed its diagnosis of the failed build:

```json
{"version":1,"type":"result","mode":"execution","exit_code":0,"session_id":"s1","request_turn_id":"t1","answer_turn_id":"t1","answer":"The test failed: expected 3, got 4.","quiet":true,"affected":[]}
```

Dashi waits for approval:

```json
{"version":1,"type":"result","mode":"execution","exit_code":3,"session_id":"s1","request_turn_id":"t1","answer":"","quiet":false,"affected":[{"turn_id":"t1","call_id":"c1","primary":true,"stop":{"kind":"waiting"},"action":{"kind":"approve"}}]}
```

| Field | Script consumer |
| --- | --- |
| `exit_code` | Branch on Dashi completion/attention. Preserve the build's exit status. |
| `answer` | Log the textual diagnosis. It does not certify test results. |
| `session_id` | Save the recorded session identity for inspection or permitted resume. |
| `affected[].primary`, `affected[].action.kind` | Identify the primary target/remedy. Inspect all affected targets. Never auto-approve. |
| `totals.invocation.inclusive.model_cost`, `totals.invocation.inclusive.tool_cost` | Optional cost logging by currency with completeness flags. Repeated reports are snapshots. |

CI keeps its existing test outcome while logging Dashi's answer:

```sh
build_status=0
go test ./... >build.log 2>&1 || build_status=$?
if [ "$build_status" -ne 0 ]; then
  dashi -p "Explain this test failure" --output-format json <build.log >diagnosis.json || true
  jq -r '.answer // empty' diagnosis.json || true
fi
exit "$build_status"
```

Acceptance checks both formats and stdout/stderr separation:

- [ ] JSON emits exactly one result object. JSONL emits typed progress objects and exactly one final result.
  Lines parse separately, including multiline answers/Unicode separators. Keep ANSI, banners, and human notices off stdout.
  Text remains answer-only. All notices use printable stderr.
- [ ] Valid machine-mode invocations return a versioned result even for preflight failures, with unavailable identities absent.
  Malformed flags/unknown formats may leave stdout empty and report usage on stderr. Document that boundary.
- [ ] Results identify session, original request Turn, last answer Turn, affected Turn/task/call, and recorded revisions where available.
  Include public answer/structured output, actual stops with terminal/projected flags and limit detail, persistence uncertainty, pending identities, and required action.
  A completed parent cannot hide an unresolved child. Never invent a completed stop from process exit or streamed prose.
- [ ] Destination ID, API, and model come from each recorded execution, not current defaults.
  Distinguish original and later notice-Turn answers. Unknown legacy destination fields remain unknown.
  Endpoint presentation excludes credentials, userinfo, and query text.
- [ ] Report invocation Turn spend separately from cumulative session spend.
  Count identified whole Turns once, including resumed facts, both summary kinds, and notices. Repeated snapshots create no charges.
  `/status` retains own-session spend. Task reporting supplies child/inclusive scopes. Automation reports distinct invocation and cumulative own/child/inclusive scopes from verified records.
  Separate model/tool money by currency, without conversion.
  Parent budgets remain parent-only. Inclusive spend neither enforces a family cap nor grants a child budget.
- [ ] Preserve uncached input, cache reads/writes, output, hosted-request meters, estimated usage, unavailable cache metrics, and `Spend.UsageIncomplete`.
  Preserve `CostUnknown`, `CostPartial`, and unpriced-request evidence independently. Never label missing usage/cost as measured zero.
  Unreadable omitted records disclose their count and affected incomplete aggregates. Usage-only incompleteness does not qualify independently exact money.
  Fixture: prior/current parent Turns USD 1/2 and verified child Turn USD 4 yield `/status` own USD 3.
  Task/automation invocation parent/child/inclusive are USD 2/4/6. Automation cumulative own/child/inclusive are USD 3/4/7.
  Duplicate references/reports count the child once. Separate tool money is zero with complete records.
- [ ] Progress identifies its recorded target and local order. Mark provisional text/accounting explicitly and reconcile it with the final snapshot.
  Export only permitted presentation fields, never raw Turn/model-request dumps, hidden reasoning/replay artifacts, credentials, environment values, or attachment bytes.
  Public reasoning summaries require the existing explicit visibility setting. Internal compaction summaries do not become answers.
  Credential-bearing diagnostics are redacted. JSON framing escapes controls without executing them on a terminal.
- [ ] Slow readers receive bounded progress without unbounded memory. A healthy writer flushes the final result before exit.
  Broken stdout fails nonzero, interrupts owned work, attempts recorded settlement, and reports the known saved session on stderr.
  It cannot claim final delivery or retry effects to reproduce output.

### UC-3: Interrupt and continue the recorded request

Interrupt a tool-using invocation, then run `dashi --resume SESSION_ID --output-format json` with no payload.

- [ ] SIGINT and SIGTERM stop admission, propagate cancellation, settle owned parent/child work, and release resources.
  Emit the latest recorded result when stdout remains usable. Hard termination cannot promise a final result or effect completion.
- [ ] Reopen first loads and validates the workspace-scoped records under existing ownership rules.
  Reporting completed or resolution-required work uses those records only. It requires no current credential, tool binding, MCP startup, or attachment access.
  Reject corrupt or foreign-owned records explicitly.
  Only before continuing or admitting new work, validate its live destination, tools, resources, and immutable attachment evidence.
  Reject missing credentials or changed bindings without destination fallback.
- [ ] Fixture: report completed parent/child work with unavailable credentials, changed tool bindings, and inaccessible attachment storage.
  JSON and JSONL retain recorded IDs, answers, stops, destinations, and spend. No provider call, MCP process, attachment read, or new Turn occurs.
- [ ] With no payload, one-shot resume continues only work that existing recovery deems ready/interrupted.
  Already-completed work returns a read-only result without another Turn or call.
  Pending approval, questions, uncertain effects, limits, and terminal stops report their exact target and remedy.
  Resume never doubles attempt limits, raises budgets, settles effects, or grants approval implicitly.
- [ ] Admit one new user Turn only when previous work and child recovery permit. Never discard unresolved work automatically.
  Failed/unknown admission retains its identity and requires reconciliation before resubmission.
- [ ] Stdin never answers an approval. Existing unattended denial and explicit `--yes` rules apply to original parent and child calls.
  Formatting, resume, instructions, and attachment selection grant no authority. Project MCP startup retains its separate binding/trust policy.
- [ ] Wait for the task track's pending children and permitted scheduled notice work before successful exit.
  Resolution-required parent/child stops interrupt remaining children, preserve records, and identify the affected target.
  No child continues as a daemon. Reopen restores stable task links and delivers each recorded result notice once.
- [ ] A keyless interrupt/reopen fixture executes a write once, retains stable request/task identities and destination, and records each child notice once.
  Repeated resume/reporting neither repeats settled effects nor counts their spend again.

## Exit contract

Process exit codes remain separate from recorded Turn stops.
Signal exit takes precedence, then output/persistence failure, then the affected recorded work.
For mixed recorded stops, choose execution failure before required interaction, then limit, refusal, and cancellation.
Waiting, uncertain effects, and interruption requiring continuation belong to required interaction when no invocation signal applies.
For equal rank, prefer the original parent, then notice/child targets in lexical Turn ID, task ID, and call ID order.
Report every affected stop/identity/action. The primary target never hides another.

| Code | Observable result |
| --- | --- |
| 0 | Recorded invocation work completed, permitted child/notice work is quiet, and output was written. Local help/quit also succeeds. |
| 1 | Known execution/setup/I/O failure, unreadable required records, or unknown persistence/delivery outcome. |
| 2 | Invalid flags, input, incompatible modes, or unsupported command before execution. |
| 3 | Host interaction/reconciliation required: waiting approval/question/result or uncertain external effect. |
| 4 | Recorded limit, refusal, or cancellation prevents successful completion. No automatic remedy. |
| 130 / 143 | Invocation interrupted by SIGINT / SIGTERM. Preserve the actual recorded stops and any uncertainty. |

Completed work retains its completed stop. Expose recorded denials without inventing a waiting stop.
Fixtures pin every code, empty input, no stdout contamination, and interrupted reading.
A completed parent with failed and waiting children selects the failed child and code 1 while reporting both children.
A waiting parent with a failed child also selects the failed child. Equal-rank parent/notice/child stops follow the stable target order.
Mixed fixture: a canceled original parent and children `a-canceled`, `b-refused`, and `z-limited` select `z-limited`, despite target ordering.
Without `z-limited`, `b-refused` wins. Both results keep code 4 and every affected stop/action.
Repeated record-only reports select the same primary target and preserve all stops without starting work.

## Decisions, limits, and success

Use one versioned CLI report, literal whole stdin, explicit line-mode compatibility, and existing accounting/recovery/task authority.
Do not export internal structs, infer commands/approvals from payloads, or add a second ledger, recovery store, or queue driver.
RPC, daemons, remote submission, new grant flags, automatic retry, and family budgets remain outside scope.

Keyless fixtures prove framing, literal input, Dashi stops/exits, accounting, child quiet/notices, and interrupt/reopen without repeated effects.
The CI fixture retains the test command's failed status when Dashi successfully diagnoses it.
[Technical spec](../design/cli-automation.md) fixes reporting and integration. Named CI/spend clarity review returned ready. Root accepted it and the technical checkpoint.
Base: attachment PRD #145, including merged inspection/task PRDs #146/#148. Rebase onto the reviewed task feature before shared CLI integration and final gates. Prerequisite API, writer qualification, and implementation authorization remain gates.
