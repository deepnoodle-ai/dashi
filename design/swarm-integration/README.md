# What Dashi could take off Swarm's plate

Dashi should make it easy to build an agent that runs in the background, survives
restarts, remembers its conversation, and stays within a budget. The application
supplies storage, decides who can spend money, and runs its approved tools.

Swarm already gets important parts of this from Dashi. The remaining problem is
that Swarm must understand too many details of Dashi's saved execution records
to connect those parts. The best additions would remove that bookkeeping.

The 32 ideas in this review vary in strength. The main recommendations are below;
the linked notes preserve the evidence, smaller ideas, and limitations. Proposed
APIs still need design and testing. [Three adversarial review batches](reviews/README.md)
challenged every idea; their decisions and refinements are included.

[See the before-and-after code examples](examples/README.md). Each recommendation
has a concrete case. Fourteen offline Go demos reproduce current behavior; after
snippets for proposed APIs are labeled explicitly.

## 1. Make agent costs easy to save in the application's database

Imagine two agents sharing the last dollar in an account. Both check the balance;
only one can reserve the money. Later, a request fails halfway through and the
provider's final charge arrives after the agent has stopped.

Swarm handles these cases, but its database adapter has to inspect Dashi's records
to calculate money changes. Its single-request generation path repeats some of
the same cost rules. Later billing corrections live outside Dashi, so Swarm also
has to reconcile the model-cost view with its account postings.

**Dashi should explain changes in each model call's cost evidence, including
old/new classified totals and finality.** Swarm applies its own posting, rounding
and reservation rules and saves them with the execution record in one transaction. Support correcting an estimated
charge when better billing information arrives. Clearly report when work is
waiting for money or has reached an application spending limit.

This removes duplicated model-cost interpretation; account rules, posted charges
and database transactions stay in Swarm. [Evidence and details: R09–R13](accounting.md).

## 2. Make saved conversations easy to continue and shorten

An application developer wants to load a conversation, continue it after a
restart, and summarize older messages when it grows too long.

Dashi has history-building and summarization APIs. Swarm still assembles its own
conversation layer because it needs its own IDs and must refuse to continue with
broken history. It also writes a 331-line adapter to save a summarization job,
run it, check the result, and replace the old history safely.

**Start with IDs, explicit history policy and pure summary validation.** Session
already protects pending tools/approvals; the policy gap is safely resumable stops.
Today's history builder accepts a suffix beginning at a validated applied summary.
Use that before inventing a lazy-loader API. Keep job recovery in the application
until a complete example proves a coordinator helps.

Swarm still owns job identity, activation transactions and the summary account.
A shorter snippet is not evidence those responsibilities disappeared.
[Evidence and details: R05, R14–R18](history.md).

## 3. Make a background agent straightforward to run and display

An application needs simple answers: Is the agent working? Waiting for a tool?
Waiting for money? Finished? Does it need attention? A browser disconnect should
leave the background agent running.

Swarm's adapter does considerable work to answer these questions. It chooses
and saves Dashi commands, resumes saved work, prevents loops that make no
progress, watches new records, and combines them with whether a worker is alive.
A saved record alone cannot tell Dashi whether the process is still running.

**Dashi should provide clearer outcomes and a reusable progress subscription.**
The application supplies worker status; Dashi combines it with saved state.
Provide a way to save a Turn without starting it and to finish work because a
limit was reached without calling it a user cancellation.

Defer the command driver until its complete journal/planner adapters show net
simplification. Swarm keeps ownership, account policy, scheduling and shutdown.
[Running: R01–R04, R06–R08](execution.md).
[Displaying progress: R19–R22](observation.md).

## 4. Fix two direct integration problems

**MCP results should preserve images and audio.** Swarm recognizes screenshot
arrays, but its MCP client returns a whole result object. The current bridge
turns that object into JSON text. Dashi has reusable conversion; expose it with validation.
Select it from the admitted MCP tool format and preserve both journal and MCP
error state. Keep tool execution with Swarm.
[Tools: R23–R26](tools.md).

**Programs should be able to control the coding CLI directly.** Swarm sends
terminal keystrokes, escapes `@` text, and clears the editor with control-key
sequences. Add commands to submit text, interrupt, and report readiness, plus
machine-readable capabilities. This has a particularly clear code-removal case.
[CLI and voice: R27–R31](peripherals.md).

## What I would do first

Start with the shared billing calculations and MCP converter. Both have clear
Swarm code to replace. Then try the focused history/admission helpers and progress updates.
Use the resulting smaller adapter to decide how much command-driving help is
worth adding. Use existing CallRef/Pending.Result and provider defaults first. Defer the
registry, batch-result API, broad estimates and voice controller until real
examples establish a benefit.

Try three existing features in Swarm separately: `Agent.ActiveCompaction` can
summarize an expanding Turn; `Runner.CacheKey` can identify a conversation for
provider caching; `feed.Hub` can deliver output while it is being generated.
These need integration testing and do not all reduce code. ActiveCompaction
currently requires a Client even for queued admission; qualify that interaction
before adopting it with credential-free cancellation.

**R32 — show the complete application integration.** Ship one small runnable
database example covering restart, tool waits, shared budgets, summarization,
and a reader disconnecting. Show where the application's transaction belongs.
Test failed writes, repeated commands/results, lost acknowledgments, and late
billing. Document how applications detect unsupported saved-record formats.
Judge each addition by the actual Swarm code it removes and the concepts an
adopter must learn. Keep approvals, sandboxes, tool execution, team scheduling,
and business account policy in Swarm.

## Evidence and verification

Reviewed 2026-10-10: Dashi `ea65bb432ae12893cfe79e2d9bdcbbe1ea8724ed` and Swarm
`a41b9a4f59617a4393e48172a97b01ca1cf76a91` (Dashi v0.6.0). The requested sibling
path was absent; Swarm is at `/Users/curtis/git/deepnoodle/swarm`. Source links in
the notes pin these versions.

Swarm uses Dashi for agents, conversation summaries, single text requests,
realtime voice, and the coding CLI. Its five production runtime-adapter files
total 2,291 lines, including comments and blanks. Some are necessary application
code; no removal percentage or speed improvement has been measured.

Focused tests passed in Swarm's runtime, CLI control, voice, and MCP packages,
and Dashi's root, session, compaction, feed, and realtime packages. Before opening
the review PR, Dashi's full `make check` also passed across all four released
modules and development tools. The example tests use local fixtures and scripted
models; they demonstrate mechanics, not SQL durability or a live CLI/voice flow.
No production library API changed or paid model request ran.
