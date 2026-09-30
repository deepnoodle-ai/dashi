# Run one coding request from a script

Status: Approved\
PRD PR: Not opened · Implementation PR: Not opened · Updated: 2026-09-30

## Problem and context

A developer pipes a multiline build log into Dashi, asks for a diagnosis, and parses the result in CI.
Today, `-p` ignores stdin. Without `-p`, line mode creates separate requests and can consume later lines as approval answers.
Its banners and notices share stdout. Print mode returns only an answer and collapses all other stops to exit code 1.
The script cannot reliably identify its recorded work, missing usage, or the action needed after interruption.

This completes O03/O04 and slice 8's reporting share of O06 in the [operational matrix](../research/cli-parity-operations.md).
[Claude Code](https://code.claude.com/docs/en/headless) combines piped data with a prompt and offers text, JSON, and streaming output.
[Pi](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/json.md) reserves stdout for JSONL and distinguishes progress from settled work.
Adopt those concrete workflows using Dashi's recorded Turn and Session authority.
Reuse the [accounting contract](spend-usage-accounting.md), composer command boundary, and preceding attachment, inspection, active-context, and task tracks.

## Users and concepts

The primary user writes local scripts or CI jobs. They need one request, a parseable result, and safe continuation.
**Invocation** is one CLI process. It admits at most one new user request.
**Result** reports the invocation and its recorded work. Child and scheduled notice Turns remain distinct from the original request.
**JSONL** is one JSON object per LF-terminated line. Progress is provisional. The final result contains the authoritative recorded snapshot.

## Use cases and acceptance

### UC-1: Send the whole build log

Run `cat build.log | dashi -p "Explain the failure" --output-format json`.

- [ ] Read nonterminal stdin to EOF before admission. Preserve Unicode, tabs, blank lines, trailing newlines, and slash-prefixed lines.
  The combined request creates one user-origin Turn, never one Turn per line or an approval/command stream.
- [ ] Place the exact `-p` text before exact stdin, separated by two newlines when both contain bytes.
  Preserve each part without trimming. Stdin alone is valid. Empty/whitespace-only combined input rejects before admission.
- [ ] Stdin is literal text: `/quit`, `yes`, `@file`, and attachment-like paths in it cause no local command, approval, or file read.
  Explicit `-p` attachment mentions and `--attach` retain the attachment track's validation and snapshot policy.
  Only parse that explicit prompt for mentions, before combining it with stdin.
- [ ] Limit stdin to 16 MiB. Read failure, invalid UTF-8, NUL, or overflow fails before model/tool dispatch.
  Never silently shorten or omit failed input. Signal cancellation during reading admits no request.
- [ ] Nonterminal stdin without `--plain` selects one-shot text output, including without `-p`.
  `--output-format text|json|jsonl` explicitly selects one-shot output with any terminal arrangement. It never reads terminal stdin.
  `-p` without the new flag keeps answer-only text output and now includes redirected stdin.
- [ ] `--plain` without `-p` intentionally retains the existing line/command/approval route.
  Reject `--plain` or `--inline` with any explicit output format.
  Fullscreen/inline interactive launch and terminal-input line mode otherwise retain their routes and controls.
- [ ] A slash-prefixed `-p` remains a command-only invocation when stdin contains no bytes.
  Help and quit aliases run locally. Other commands reject with the composer's actionable guidance. None calls a provider.
  Reject slash-prefixed `-p` plus nonempty stdin before dispatch, rather than silently interpreting either as a model request.
  Machine formats return a command/error result with no fabricated Turn identity.

### UC-2: Parse the result and its cost

Run the same request with `json`, then `jsonl`. Parse stdout independently from stderr.

- [ ] JSON emits exactly one result object. JSONL emits typed progress objects and exactly one final result.
  Every line parses separately, including multiline answers and Unicode separators. Neither mode emits ANSI, banners, or human notices to stdout.
  Text output stays answer-only. Loading, trust, attachment, compaction, tool, and task notices use printable stderr.
- [ ] Valid machine-mode invocations return a versioned result even for preflight failures, with unavailable identities absent.
  Malformed flags/unknown formats may leave stdout empty and report usage on stderr. Document that boundary.
- [ ] Results identify session, original request Turn, last answer Turn, affected Turn/task/call, and recorded revisions where available.
  Include public answer/structured output, actual stops with terminal/projected flags and limit detail, persistence uncertainty, pending identities, and required action.
  A completed parent cannot hide an unresolved child. Never invent a completed stop from process exit or streamed prose.
- [ ] Destination ID, API, and model come from each recorded execution, not current defaults.
  Distinguish original and later notice-Turn answers. Unknown legacy destination fields remain unknown.
  Endpoint presentation excludes credentials, userinfo, and query text.
- [ ] Report invocation Turn spend separately from cumulative session spend.
  Identify covered Turns. Totals cover their whole records, including prior facts on resume, rather than only newly billed work.
  Repeated reports are snapshots and must not be added as new charges.
  Count ordinary, between-Turn summary, active-context summary attempts, and scheduled notice work once under their recorded owners.
  Report parent, child, and inclusive amounts distinctly. Model and local-tool money stay separate by currency, without conversion.
  Parent budgets remain parent-only. Inclusive spend neither enforces a family cap nor grants a child budget.
- [ ] Preserve uncached input, cache reads/writes, output, hosted-request meters, estimated usage, unavailable cache metrics, and `Spend.UsageIncomplete`.
  Preserve `CostUnknown`, `CostPartial`, and unpriced-request evidence independently. Never label missing usage/cost as measured zero.
  Unreadable omitted records disclose their count and affected incomplete aggregates. Usage-only incompleteness does not qualify independently exact money.
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
  Corrupt records or another owner produce explicit errors rather than invented results.
  Only before continuing or admitting new work, validate its live destination, tools, resources, and immutable attachment evidence.
  Missing credentials or changed bindings then produce explicit errors. No destination fallback occurs.
- [ ] Fixture: report completed parent/child work with unavailable credentials, changed tool bindings, and inaccessible attachment storage.
  JSON and JSONL retain recorded IDs, answers, stops, destinations, and spend. No provider call, MCP process, attachment read, or new Turn occurs.
- [ ] With no payload, one-shot resume continues only work that existing recovery deems ready/interrupted.
  Already-completed work returns a read-only result without another Turn or call.
  Pending approval, questions, uncertain effects, limits, and terminal stops report their exact target and remedy.
  Resume never doubles attempt limits, raises budgets, settles effects, or grants approval implicitly.
- [ ] With a new payload, start one new user Turn only after previous work and child recovery permit admission.
  Do not automatically discard unresolved work to accept it. A failed/unknown admission preserves its identity and requires reconciliation before resubmission.
- [ ] Stdin never answers an approval. Existing unattended denial and explicit `--yes` rules apply to original parent and child calls.
  Formatting, resume, instructions, and attachment selection grant no authority. Project MCP startup retains its separate binding/trust policy.
- [ ] Wait for the task track's pending children and permitted scheduled notice work before successful exit.
  Resolution-required parent/child stops interrupt remaining children, preserve records, and identify the affected target.
  No child continues as a daemon. Reopen restores stable task links and delivers each recorded result notice once.
- [ ] A keyless interrupt/reopen fixture executes a write once, retains stable request/task identities and destination, and records each child notice once.
  Repeated resume/reporting neither repeats settled effects nor counts their spend again.

## Exit contract

The result carries the process exit code separately from each Turn's recorded stop.
Signal exit takes precedence, then output/persistence failure, then the affected recorded work.
For mixed recorded stops, choose execution failure before required interaction, then limit, refusal, and cancellation.
Waiting, uncertain effects, and interruption requiring continuation belong to required interaction when no invocation signal applies.
For equal rank, prefer the original parent, then notice/child targets in lexical Turn ID, task ID, and call ID order.
Report every affected stop, identity, and required action. The primary target never replaces or hides the others.

| Code | Observable result |
| --- | --- |
| 0 | Recorded invocation work completed, permitted child/notice work is quiet, and output was written. Local help/quit also succeeds. |
| 1 | Known execution/setup/I/O failure, unreadable required records, or unknown persistence/delivery outcome. |
| 2 | Invalid flags, input, incompatible modes, or unsupported command before execution. |
| 3 | Host interaction/reconciliation required: waiting approval/question/result or uncertain external effect. |
| 4 | Recorded limit, refusal, or cancellation prevents successful completion. No automatic remedy. |
| 130 / 143 | Invocation interrupted by SIGINT / SIGTERM. Preserve the actual recorded stops and any uncertainty. |

Completed work with a recorded denied call retains its completed stop. The report exposes the denial without inventing a waiting stop.
Fixtures pin every code, empty input, no stdout contamination, and interrupted reading.
A completed parent with failed and waiting children selects the failed child and code 1 while reporting both children.
A waiting parent with a failed child also selects the failed child. Equal-rank parent/notice/child stops follow the stable target order.
Repeated record-only reports select the same primary target and preserve all stops without starting work.

## Decisions, limits, and success

Use one versioned reporting contract across JSON and JSONL, with the same final result. Reject exporting internal structs as a public format.
Keep literal whole-stdin input and explicit line-mode compatibility. Reject autodetecting commands or approval answers inside payloads.
Reuse record-derived accounting/recovery and the task host's quiet/report boundary. Reject a second ledger, recovery store, or queue driver.
Keep machine formatting separate from model JSON-schema output. RPC, daemons, remote submission, new grant flags, automatic retry, and family budgets remain outside scope.

Success is a keyless shell transcript piping a multiline log into one recorded request and parsing both formats with `jq`.
It also proves stops/exit codes, accounting completeness, child quiet/result delivery, and interrupt/reopen without repeated effects.
Named independent PRD re-review returned ready with no remaining findings.
The [technical spec](../design/cli-automation.md) freezes framing, presentation fields, bounded writing, and preceding-track integration before code.
No paid qualification or product question blocks this draft. Preceding task/report and attachment APIs must settle before implementation.
