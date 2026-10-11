# Continue only validated conversation history

Status: Draft\
PRD PR: Pending · Implementation PR: Pending · Updated: 2026-10-11

## Problem and context

Application developers store Dashi Turns and decide when agents can continue.
`BuildHistory` can omit unreadable Turns or honor a summary whose source hash differs.
[Swarm converts those warnings into recovery errors](../../design/swarm-integration/examples/r14-broken-conversation.md).
That policy is easy for another application to miss.

Swarm also saves summary jobs, then validates and activates their finished Turns.
[The reviewed summary workflow](../../design/swarm-integration/examples/r16-summary-restart.md) calls for a pure helper that removes repeated validation and replacement assembly.
`Compactor.Start` already freezes the source hash and replacement metadata.
`TurnView.ReplacementHistory` already builds the replacement messages.
The helper should reuse both contracts.

The primary user owns application storage and execution policy.
Their job is to continue a saved conversation safely, including after a crash between summary completion and activation.

## Concepts

- **Strict history:** model history built only when all supplied Turn records pass integrity checks.
- **Replacement:** validated summary history plus its summary Turn identity and source hash, ready for application-owned activation.
- **Frozen source:** the history and compaction Start saved by the application before it runs the summary Turn.

## Use cases and acceptance

### UC-1: Refuse broken supplied history

The developer calls `dashi.BuildHistoryStrict(turns)` before admitting the next request.

- [ ] Nil or empty input returns nil history and no error.
- [ ] Valid input produces the same messages as `BuildHistory` without integrity warnings.
- [ ] An unreadable Turn or unbuildable ordinary Turn returns nil history and an error identifying its Turn ID.
- [ ] Every supplied record is checked, including records hidden by a later successful summary.
- [ ] A completed summary whose hash differs from its supplied covered history returns nil history and identifies the summary Turn.
  Earlier mismatches cannot disappear behind a later summary.
- [ ] Recorded notices unrelated to construction do not cause strict construction to fail.
  Failed or empty summaries still replace nothing.
- [ ] A suffix starting at an applied summary remains supported.
  The documentation states that omitted covered records cannot be checked.
- [ ] Existing valid compaction records without hashes remain readable.
  The documentation states that they carry no source hash proof.
- [ ] Construction leaves every supplied record unchanged.
  The default tolerant builder retains its existing behavior.

### UC-2: Validate a finished summary after reload

The application reloads its saved job, frozen source, and bound summary Turn.
It calls `compaction.ValidateReplacement(start, source, summaryTurn)`.

- [ ] The helper checks a nonempty source hash against the frozen source and compares the recorded summary metadata with the frozen Start.
- [ ] It rejects invalid records, another origin, changed retained messages or placement, changed framing, changed coverage, and changed summary input.
- [ ] Only a completed Turn with a nonempty extracted summary returns a replacement.
  Failed, limited, interrupted, or empty results return an identifying error.
- [ ] The replacement carries usable history, summary text, summary Turn ID, source hash, and source message count.
  It survives JSON storage and owns its message content.
- [ ] Repeated validation after reload returns equal values without model calls, tool execution, storage writes, or input mutation.
- [ ] A keyless example shows the application retaining old history until its activation commits.
  A failed required summary leaves the waiting request unadmitted.

## Decisions and scope

- Add `BuildHistoryStrict` beside `BuildHistory`.
  Reject changing tolerant defaults or asking callers to classify every warning.
- Validate supplied records before selecting summary history.
  Reject a wrapper that misses hidden corruption or assumes omitted records were checked.
- Return a concrete value from `compaction.ValidateReplacement`.
  Reject a coordinator, Store interface, scheduler, and lazy history loader.
- The application owns durable job creation, execution, account, deadline, source revision, activation transaction, and unknown-write recovery.
  It also chooses required versus optional summarization and verifies the job's summary Turn binding.

## Success, risk, and delivery

Success lets applications replace warning conversion and repeated summary validation while preserving their storage and execution authority.
The helper proves record consistency, not summary quality, current source revision, or successful activation.
Strict construction reads all supplied records. Trusted summary suffixes remain the application's way to avoid covered-history reads.

Ship keyless public `Example*` tests, adversarial and compatibility tests, guide updates, and an Unreleased changelog entry.
Run focused race tests and `make check`. Independent review must exercise the examples before implementation publication.
No paid request or Swarm change belongs to this track.
