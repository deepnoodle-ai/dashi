# When these changes matter

The user journeys below reassess the seven original implementation proposals.
Tail is withdrawn; six remain in the implementation PRs. Passing tests establishes
API behavior, not whether an addition earns its place in the library.

The distinction that was missing: Swarm already handles most of these cases.
A Dashi addition must fix an observable defect, remove meaningful application
work, or support an explicit product behavior. It should not take credit for
recovery that the application already implements.

## Current recommendation

| Change | Sequence to inspect | What changes relative to Swarm today | Judgment |
| --- | --- | --- | --- |
| MCP conversion | [Ask the agent to inspect an MCP screenshot](r25-mcp-images.md#sequence-where-it-matters) | The next model request contains an image instead of JSON text. | Strongest case. Share the MCP module's conversion; an envelope fix in Swarm is the smaller alternative. |
| Record-only admission | [Stop accepted work before it starts](r01-save-queued-work.md#sequence-where-it-matters) | A direct call replaces the closed-pause convention. Cancellation already works without credentials. | Modest ergonomic benefit for a background-worker library. |
| Initial context | [Resume work in its original project](r06-initial-reminders.md#sequence-where-it-matters) | Saved reminders become Start data instead of first-call callback logic. They already survive restart in Swarm. | Useful ergonomic benefit; weigh it against the reader upgrade. |
| Strict history | [Continue after loading an unreadable saved record](r14-broken-conversation.md#sequence-where-it-matters) | The library directly returns an identifying error and no history. Swarm already refuses the request. | Small safety API, with modest code savings. |
| Saved failures | [Reload a provider rejection before its retry time](r03-handle-failure.md#sequence-where-it-matters) | The original category and delay remain available. | Useful data, conditional user benefit. Swarm still needs the corresponding retry/repair policy. |
| Summary validation | [Restart after a summary finishes](r16-summary-restart.md#sequence-where-it-matters) | More frozen-source/input checks move to Dashi. Finished-summary reuse already exists. | Defer adoption until a Swarm port proves useful code removal. |
| Tail | [Stop displaying results when the user stops the agent](r22-updates-after-finish.md#sequence-where-the-earlier-proposal-fails) | No change is needed: Follow already ends with execution. | Withdraw. The late-result panel was an invented requirement; remove Tail from #253. |

## What the assessment changes

The implementation index and examples remain available in
[remaining implementation candidates](implemented.md). The PRs contain tested
code with different adoption cases; the withdrawn idea remains documented.

For [#252](https://github.com/deepnoodle-ai/dashi/pull/252), admission and initial
context remove specific implementation conventions. Failure metadata needs an
application policy before it changes the user's experience.

For [#254](https://github.com/deepnoodle-ai/dashi/pull/254), strict history has a
small explicit-safety case. The summary helper does not replace Swarm's job
recovery, account checks, or activation transaction. Its value needs a real port.

For [#253](https://github.com/deepnoodle-ai/dashi/pull/253), the screenshot case
supports MCP conversion. Tail is removed from that PR and its requirements.
Stopping the agent already stops the normal watcher; the user expects no later
result. Retaining internal evidence does not require a new display subscriber.

## Evidence boundaries

Swarm's reviewed/current checkout is `a41b9a4f59617a4393e48172a97b01ca1cf76a91`.
The numbered sequences name user actions, application work, and external events.
Crash, damaged-record, and delayed-result sequences are scenarios supported by
source or fixtures, not reported production incidents. The 429 timer needs an
application policy. The proposed post-stop activity reader is withdrawn.

The proof for the screenshot case is the input type, not the model's eventual
answer quality. No adoption port or whole-adapter line reduction was measured.
Implementation/security reviews verified the code against its requirements;
this document reassesses whether those requirements earn a library API.
