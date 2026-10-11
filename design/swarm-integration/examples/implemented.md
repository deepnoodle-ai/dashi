# Seven implemented improvements

Three implementation PRs contain tested code built in separate worktrees.
They are open and awaiting merge. Their adoption cases differ in strength.
[Read the exact user actions and system events, and the revised recommendations](when-it-matters.md)
before treating implementation readiness as a reason to merge.
The examples below use implemented APIs and run without provider keys.

| Case | What changes for the application developer | API and runnable example | PR |
| --- | --- | --- | --- |
| [R01: queued work](r01-save-queued-work.md) | Save a request, then cancel or run it later, without forcing a pause or contacting the model. | `Runner.Admit`; [ExampleRunner_Admit](https://github.com/deepnoodle-ai/dashi/blob/76caabc7afc902599f6b9434ba03235dcceb3850/run_admit_example_test.go) | [#252](https://github.com/deepnoodle-ai/dashi/pull/252) |
| [R03: provider failure](r03-handle-failure.md) | Reload original failure metadata. The application still needs a retry/repair policy. | `TurnView.Failure`; [ExampleFailure and ExampleFailure_unknownSend](https://github.com/deepnoodle-ai/dashi/blob/76caabc7afc902599f6b9434ba03235dcceb3850/run_admit_example_test.go) | [#252](https://github.com/deepnoodle-ai/dashi/pull/252) |
| [R06: initial reminders](r06-initial-reminders.md) | Save a reminder with accepted input and deliver it once after restart, without an initialization hook. | `Start.Context`; [ExampleStart_context](https://github.com/deepnoodle-ai/dashi/blob/76caabc7afc902599f6b9434ba03235dcceb3850/run_admit_example_test.go) | [#252](https://github.com/deepnoodle-ai/dashi/pull/252) |
| [R14: broken history](r14-broken-conversation.md) | Refuse unreadable supplied records and mismatched summaries before another request. | `BuildHistoryStrict`; [ExampleBuildHistoryStrict](https://github.com/deepnoodle-ai/dashi/blob/63234506782ef4b8ae444d89744663f7f2a04d6b/history_strict_test.go) | [#254](https://github.com/deepnoodle-ai/dashi/pull/254) |
| [R16: saved summary](r16-summary-restart.md) | Centralize additional validation. Swarm already reuses finished summaries; useful code removal remains unproven. | `compaction.ValidateReplacement`; [ExampleValidateReplacement](https://github.com/deepnoodle-ai/dashi/blob/63234506782ef4b8ae444d89744663f7f2a04d6b/compaction/replacement_example_test.go) | [#254](https://github.com/deepnoodle-ai/dashi/pull/254) |
| [R22: late results](r22-updates-after-finish.md) | Supply a post-stop iterator for a future separate reader. No current Swarm consumer was identified. | `feed.Tail`; [ExampleTail](https://github.com/deepnoodle-ai/dashi/blob/41714a89b2723c8c077a53c7c91d786912dd05a7/feed/tail_example_test.go) | [#253](https://github.com/deepnoodle-ai/dashi/pull/253) |
| [R25: MCP media](r25-mcp-images.md) | Convert typed or saved MCP responses into images, audio, and text while retaining failure state and exact JSON numbers. | `mcp.ConvertResult`, `mcp.ConvertResultJSON`; [three conversion examples](https://github.com/deepnoodle-ai/dashi/blob/41714a89b2723c8c077a53c7c91d786912dd05a7/mcp/result_example_test.go) | [#253](https://github.com/deepnoodle-ai/dashi/pull/253) |

## What the code reviews changed

The history review caught empty source messages that the helper accepted but
ordinary Dashi history rejected. Validation now refuses them. It also compares
initial context directly, including user context whose lifetime has expired.

The turn-input review caught initial instructions missing from summary requests,
raw provider errors saved during summary replacement preparation, and returned
failure pointers sharing memory with records. All three have regression tests.
Preparation errors now preserve safe metadata without exposing provider text.

The MCP reviews reproduced a decoder panic from a null interaction entry and
SDK automation that repeated Toolset calls before conversion. Malformed
interaction envelopes now return safe errors. Toolset refuses unsupported input
requests after one call and keeps the connection usable. Unusable completed
output is settled as a failed result; completed tool execution is not repeated.

## Verification and limits

Every implementation passed `make check` and focused race tests. Independent
reviewers ran the build, tests, public examples, and additional adversarial probes.
The combined tree passed all four modules and devtools, race-tested examples,
and an offline workflow spanning admission, initial context, cancellation, late
MCP settlement, Tail, strict history, and frozen-summary comparison.

The application still owns scheduling, permissions, accounts, durable tool
execution, and summary activation transactions. Initial-context writers require
upgraded execution readers and saved-command consumers; #252 documents the order.
These tests qualify local mechanics, not SQL durability or paid providers.
Swarm has not yet adopted these changes, so actual adapter deletion remains unmeasured.
