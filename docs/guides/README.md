# Guides

Task-oriented guides for building with Dashi. To build an agent, start with
[Running turns](running-turns.md).

## Guides

- [Running turns](running-turns.md): running an agent turn, reading the
  result, prior conversation, structured output, and streaming.
- [Packages](packages.md): the foundation packages, dependency direction, and
  media and speech client status.
- [Model calls](model-calls.md): preparing, sending, and streaming one model
  call, with validation, replay, tools, and cost.
- [Tools](tools.md): defining typed tools, returning outcomes, reporting
  progress, and adding provider-hosted tools.
- [Hooks](hooks.md): checking or changing model calls, tool calls, and final
  answers.
- [Approving tool calls](approvals.md): asking a person inline, or deferring
  calls and answering them later.
- [Recovering turns](recovering-turns.md): IDs, recorders, crash recovery,
  turn states, and limits.
- [Live updates](live-updates.md): serving a turn to browsers and observing a
  run in process.
- [Steering](steering.md): messages from people while a turn runs.
- [Skills](skills.md): loading skills from folders, giving the catalog to a
  model, and starting a skill by name.
- [Turn budgets](budgets.md): setting caps, accounting, raises, and host credit
  reservations.

## Reference

- [Agent turns and recovery](../agent-turns.md): the detailed contract for
  recorders, pending results, approvals, and stops.
- [Turn loop review against Dive](../agent-loop-review.md): what is proven and
  what remains before the loop API stabilizes.
- [Live tests](../dev/live-tests.md): paid live test commands and dated
  qualification results.
- [Go style guide](../dev/go-style.md): conventions for contributors.
- [Design notes](../../design/): decisions, proposals, and terminology.
