# Command-risk outage confirmation

Status: Superseded. A failed command check now needs approval, with no
confirmation request and no pause.\
Author: Codex · Date: 2026-10-07\
Workflow: Spec, then build after independent PRD review.\
Reviewers: independent PRD reviewer and security implementation reviewer.\
PRD: [command-risk-outages.md](../prds/command-risk-outages.md)

## Context

`toolcheck.ask` currently treats a server or non-timeout network error as
an outage and immediately starts a shared pause. The CLI also infers a
pause from `IsOutage`. A payload-specific failure therefore disables
checking across turns. Repeating that payload cannot establish provider
availability independently of untrusted command text.

## Proposal

Keep confirmation in `decide/toolcheck`, at the command policy boundary.
Use the existing part splitting, response validation, client resolution,
and pause table. Keep the prompt-injection path's policy and call options.
No public configuration, root engine change, or new client state is needed.

For `CommandRisk.Check`:

1. Check cancellation before client resolution, pause lookup, and requests.
   Keep whole-command size rejection and existing setup-error behavior.
2. Create one context bounded by `Timeout` and the caller's deadline.
   Send the command parts with `decide.WithCallMaxRetries(0)`. At most
   eight parts run, with the existing four-part concurrency limit.
3. On a valid whole-command answer, return the existing verdict. A timeout,
   rate limit, rejected request, or invalid answer remains suspect.
   Consider every part's error before confirmation. Any suspect failure,
   including an SDK attempt timeout while the whole-check context is live,
   vetoes confirmation. Retain every original cause and observed usage.
4. Authentication and setup failures are direct outages. They do not depend
   on command text and need no confirmation request.
5. For a server or non-timeout network error, send one request with
   `state = {"command": "true"}` and the existing `severe` question.
   Use the same client, model, context, and validation. Disable SDK retries.
   The text is constant and never includes a command, part, or tool result.
6. If that request also returns a server, non-timeout network, or
   authentication failure, confirm an outage. Recheck the context before
   starting the existing one-minute pause. Return its `PausedError`,
   including the exact end time and original failure as its cause.
7. Otherwise return the original failure as suspect. A valid benign answer
   proves availability only. It never passes or flags the original command.
   A benign-request timeout or cancellation takes precedence and never
   confirms an outage. Return a context error when the check context ends.

Do not reset the deadline, sleep, or retry the whole multipart command.
A slow first request may leave no time for confirmation. That command then
requires approval instead of opening a shared pause. Maximum provider calls
are eight original parts plus one benign request, including with a client
configured for more SDK retries. A one-part command uses at most two.

### Error and presentation contract

Use a private error wrapper for an unconfirmed command failure. It unwraps
to the original error. `IsOutage` recognizes the wrapper before classifying
the underlying error. `CauseOf` still returns the original cause. Existing
raw Decide errors retain their classification for prompt-injection checks.
Any context cancellation or deadline in a command result takes precedence
over outage classification.

The command hook holds this suspect error, with a reason naming the cause.
The CLI's current `suspect` and `holds` decisions then apply. A confirmed
command outage returns `PausedError` even on the initiating check, so the
CLI uses the library's actual end rather than inventing another minute.
Keep one `OnCheck` callback and one checked/unchecked command count per
logical check. A benign answer is never recorded as a checked command.
Add `Verdict.Requests` for logical Decide calls initiated, including the
benign request. This does not count SDK retries or prove HTTP dispatch or
receipt. Skipped, unread, and setup failures count zero. Keep observed
response usage even on errors. The CLI uses these observations directly,
instead of inferring requests from error types. A concurrent confirmation
returns the existing pause identity and still unwraps its own original
failure.

### Concurrent and delegated work

The existing pause key stays `(resolved client, check kind)`. Network calls
and callbacks run outside its mutex. Do not serialize command checks or
add a probe cache. Each in-flight check completes its own policy decision.
New checks observe any confirmed pause. Independent clients and content
checks remain separate. The CLI's shared runtime and existing child hooks
need no new delegation state.

## Security considerations

The agent, command text, and tool results are untrusted. The configured
provider and credentials are host-controlled. A repeated payload bug,
including a failing part of a large command, cannot confirm an outage while
the constant request succeeds. A non-outage probe failure stays suspect.
Approvals remain the host's decision. The check only holds or advises.

A command may overload the service or its connection enough to break the
constant request too. Confirmation then starts the accepted fail-open
pause. This change does not eliminate that service-wide tradeoff or make
the classifier a sandbox. Cancellation immediately before pause creation
must be checked under the existing policy path, not inferred from a
provider error. No new untrusted text is transmitted by the probe.

## Alternatives and tradeoffs

- Same-command retry: minimal, but a persistent payload failure still opens
  the unchecked window. It does not solve the underlying security problem.
- Two different failing commands: avoids a probe, but attacker-controlled
  text can shape both. It needs hashes, expiration, and concurrent state.
- Never pause on server or network errors: strongest approval protection,
  but changes the accepted outage behavior and stops unattended work.

The benign request adds one small provider request after a qualifying
failure. Removing hidden command retries can require approval after a
transient failure that the SDK previously recovered. Both costs keep the
request bound and policy clear. No paid requests are needed to qualify it.

## Validation and rollout

Use fake Decide servers and transports. Cover a single payload and large
multipart payload that always fail while `true` succeeds. Neither may
pause, including after repeats. Cover a server or network failure followed
by each possible benign outcome: valid answer, auth/server/network error,
rate limit, invalid answer, timeout, and cancellation. Check request counts
with SDK retries configured above zero. Check pause expiry and exact end.

Exercise library holds, CLI default/bypass/unattended modes, and delegated
commands. Keep existing prompt-injection tests green. Use barriers for
parallel checks and cancellation during confirmation. Run focused tests
with `-race`, then `make check`. Require independent implementation and
security reviews. Update the CLI guide, toolcheck comments, shared Decide
design exception, and Unreleased changelog. Ship as a focused bug fix.
