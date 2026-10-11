# Before-and-after examples for the Swarm review

[Main review](../README.md)

Each page contains a concrete situation, unmodified excerpts from the reviewed
source snapshot, an after snippet, the observable result, and the work it would
remove. Before excerpts omit surrounding definitions; they are not complete
programs. Source links pin Swarm `a41b9a4` and Dashi `ea65bb4`.

**Proposed API sketches are design examples. Those APIs do not exist yet.**
Application storage methods illustrate the responsibilities that remain with
Swarm. Shorter caller code alone does not prove a better abstraction. The pages
also identify existing APIs the application can adopt today.

Start with [database cost updates](r09-database-cost-updates.md),
[summary recovery](r16-summary-restart.md),
[progress subscriptions](r20-progress-subscription.md),
[MCP images](r25-mcp-images.md), and [CLI commands](r27-cli-commands.md).

## Run the offline demos

From the repository root:

```sh
go test -v ./design/swarm-integration/examples/demo
```

The `Example*` functions use current Dashi APIs, a scripted model, and local
fixtures. They assert their printed output. They cover saving/canceling queued
work, duplicate commands after a new Runner starts, unreadable history, worker
status, preview gap followed by a final reply, input saved before dispatch,
raw versus posted cost, finished-call cost correction, existing Pending.Result, rounding, reader
disconnect, late results, summarization within a Turn, and the MCP array/object mismatch.

The MCP after-demo is a local envelope-unwrapping prototype, limited to its
text/image fixture. It does not implement the proposed public MCP converter.
No proposed API is mocked into existence. The demos do not qualify SQL
transactions, real process crashes, paid providers, a live CLI, or audio devices.

## All recommendations

| Recommendation | Example | After code / decision |
| --- | --- | --- |
| R01 | [Cancel a request before it starts](r01-save-queued-work.md) | Proposed |
| R02 | [Display a limit as a limit](r02-finish-at-limit.md) | Proposed |
| R03 | [Know what to do after a provider failure](r03-handle-failure.md) | Proposed |
| R04 | [Resume a command saved before a crash](r04-advance-saved-work.md) | Deferred experiment |
| R05 | [Use conversation helpers with application IDs](r05-application-turn-ids.md) | Proposed |
| R06 | [Supply a reminder once without an initialization hook](r06-initial-reminders.md) | Proposed |
| R07 | [Look up one configured model consistently](r07-configured-models.md) | Existing; registry deferred |
| R08 | [Drop only a setting the model rejected](r08-unsupported-setting.md) | Proposed |
| R09 | [Save model records and money together](r09-database-cost-updates.md) | Proposed |
| R10 | [Handle two agents competing for the last dollar](r10-shared-budget.md) | Proposed |
| R11 | [Replace an estimated charge with final billing](r11-final-billing.md) | Proposed |
| R12 | [Reuse billing rules for a Writing desk request](r12-single-request-billing.md) | Proposed |
| R13 | [Round costs consistently and explain request estimates](r13-money-units.md) | Existing; broad estimate deferred |
| R14 | [Refuse a conversation with unreadable history](r14-broken-conversation.md) | Proposed |
| R15 | [Read only history needed after an applied summary](r15-load-less-history.md) | Existing API |
| R16 | [Recover a conversation summary after a restart](r16-summary-restart.md) | Pure helper; coordinator deferred |
| R17 | [Keep a tool-heavy Turn within its context window](r17-growing-turn.md) | Existing API |
| R18 | [Use one cache identity across a conversation](r18-conversation-cache-key.md) | Existing API |
| R19 | [Show a running worker as running](r19-running-status.md) | Proposed |
| R20 | [Connect a browser to an independently running agent](r20-progress-subscription.md) | Proposed |
| R21 | [Deliver text while the model is generating it](r21-live-output.md) | Existing API |
| R22 | [Keep following later results for accounting](r22-updates-after-finish.md) | Proposed |
| R23 | [Return a result to the right Turn and call](r23-tool-call-identity.md) | Existing API |
| R24 | [Submit results without repeating command construction](r24-several-tool-results.md) | Existing; batch API deferred |
| R25 | [Give the model an MCP screenshot as an image](r25-mcp-images.md) | Proposed |
| R26 | [Acknowledge input saved into the transcript](r26-delivered-input.md) | Proposed |
| R27 | [Submit literal text to the coding CLI](r27-cli-commands.md) | Proposed |
| R28 | [Check CLI support before sending commands](r28-cli-capabilities.md) | Proposed with R27 |
| R29 | [Package a verifiable CLI for remote machines](r29-cli-packaging.md) | Proposed release tooling |
| R30 | [Wait to speak until the user and playback are ready](r30-voice-response.md) | Existing; controller deferred |
| R31 | [Share provider defaults and define voice metadata](r31-voice-options.md) | Existing defaults; metadata proposed |
| R32 | [Try the integration without paying a provider](r32-offline-adoption.md) | Existing API |

[Read the adversarial reviews and decisions](../reviews/README.md). All 32 ideas
were reviewed for correctness, alternatives and actual simplification. Deferred
ideas remain here so the reasoning and code comparison are reviewable.

## How to judge a proposal

Check that the after code preserves the before code's responsibilities. In
particular, account updates and records share a transaction, repeated commands
cannot repeat effects, and a disconnected reader cannot stop a worker. The
application still owns scheduling, access, approvals, sandboxes, and accounts.

These snippets are design comparisons, not measured deletion counts. Implement
one proposal against Swarm, remove the corresponding adapter code, and test its
failure cases before deciding the abstraction succeeded.
