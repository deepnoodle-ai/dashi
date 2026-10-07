# Keep command-specific failures from disabling command checks

Status: Approved\
PRD PR: #231 · Implementation PR: pending · Updated: 2026-10-07

## Problem

A command can trigger a Decide server error or dropped connection. Today,
that one failure pauses `CommandRisk` for a minute. During the pause,
`--dangerously-skip-permissions` runs every shell command unchecked,
including parallel and delegated commands. [Issue #208][issue] and the
[CLI guide][guide] document this gap.

## Context

`toolcheck` already holds commands when their checks time out, exceed the
input limit, or receive a rejected request or answer. Provider failures
currently bypass that protection. Checks share a pause by Decide client
and check kind. The CLI bounds command checks to three seconds and uses
one approval prompt per call.

## Users and job

Application developers and CLI users need command checks that preserve
approval during an isolated failure, while tolerating a confirmed outage.

## Use cases

### UC-1: One command breaks its check

A command receives a server or connection error. `CommandRisk` asks the
same provider about fixed benign text to check availability. The provider
answers that request. The original command remains unchecked and suspect.

Acceptance:
- [ ] The library hook holds the original command for approval.
- [ ] Bypass mode asks once and cannot remember this approval.
- [ ] `dashi run --yes` refuses the command and records its check failure.
- [ ] Repeating the failing command never starts a shared pause while the
      benign request succeeds. Parallel and delegated commands still check.
- [ ] A non-outage failure of the benign request also leaves the original
      command suspect. Its answer never becomes the command's verdict.

### UC-2: The provider cannot answer independently of command text

The command request and the benign request both fail with an outage.

Acceptance:
- [ ] The command check pauses for one minute, shared only by the resolved
      client and command-check kind. The CLI shows the actual pause end.
- [ ] Existing outage behavior remains: bypass mode proceeds unchecked,
      the library records a warning, and checks resume after the pause.
- [ ] Authentication and setup failures keep their direct outage behavior.
- [ ] Prompt-injection failures cannot confirm or pause command checks.

### UC-3: The user answers first or the deadline ends

Acceptance:
- [ ] All original parts and the benign request share one check deadline.
      The CLI still adds at most three seconds, apart from scheduling.
- [ ] Cancellation prevents further requests and never starts a pause.
- [ ] A timeout during either request leaves the command suspect. The
      default prompt still opens immediately and accepts the user's answer.
- [ ] A command check sends at most eight original parts plus one benign
      request. Command requests cannot multiply through SDK retries.

## Requirements

- R-1: Put outage confirmation in `toolcheck`, so the hook and hosts that
  call `CommandRisk.Check` use one policy. Keep existing zero values.
- R-2: Preserve `errors.Is` and `errors.As` for the original failure.
  `IsOutage` must return false for an unconfirmed command failure.
  `CauseOf` must still name the original cause, such as `server error`.
- R-3: Keep thresholds, whole-command checking, approval ownership, and
  prompt-injection behavior unchanged. Emit one callback per command check.
- R-4: Document the confirmation rule, request bound, deadline, and
  remaining fail-open tradeoff. Verify with keyless regression tests.

## Decisions

- Use fixed benign text, rather than retrying the same command as #208
  proposes. A persistent payload bug can fail twice. Two attacker-shaped
  commands can also fail. Neither proves an independent outage.
- Disable SDK retries for command parts and the benign request. One layer
  owns confirmation and bounds requests. Content-check retries stay as now.
- Keep the accepted one-minute fail-open behavior after confirmation.
  Replacing outage policy or adding a sandbox is outside this issue.

## Risks and success

A command can cause a service-wide failure that also breaks the benign
request. The check then pauses. This remains the availability tradeoff of
the existing fail-open policy. Success means an isolated payload failure
cannot open the shared unchecked window, without weakening approval,
cancellation, deadline, or delegated-work guarantees.

[issue]: https://github.com/deepnoodle-ai/dashi/issues/208
[guide]: ../guides/cli-decide.md#when-decide-cannot-answer
