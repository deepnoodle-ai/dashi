# Adversarial review decisions

[Main review](../README.md) · [Before-and-after examples](../examples/README.md)

On 2026-10-11, three independent reviewers challenged separate batches covering
all 32 recommendations. They inspected Dashi/Swarm source and the after sketches.
Their questions were: does this solve a real application problem, preserve the
existing guarantees, remove work rather than hide it, and beat smaller alternatives?

The notes below capture findings and their disposition. They are not approval to
implement the proposed APIs. After refinements, the same reviewers checked their
batches again; follow-up outcomes are recorded in each note.

| Batch | Scope | Review notes |
| --- | --- | --- |
| Execution and history | R01–R08, R14–R18 | [Findings and decisions](execution-history.md) |
| Accounting and tools | R09–R13, R23–R26 | [Findings and decisions](accounting-tools.md) |
| Observation and embedding | R19–R22, R27–R32 | [Findings and decisions](observation-embedding.md) |

The broad driver, destination registry, batch-result API, summary coordinator,
estimate API and voice controller are deferred. Existing CallRef/Pending.Result,
history suffix construction, provider defaults and feed support should be tried
first. Release packaging stays outside the runtime CLI.

The strongest library additions remain record-only admission, finishing a saved
limit without calling it cancellation, retained failure facts, strict history,
per-call cost classification/correction, typed funding refusal, standalone MCP
conversion and explicit CLI control. Progress subscriptions need a complete
adapter comparison before their size is justified.

Fourteen offline demos pass using current APIs and local fixtures. They prove
bounded mechanics, including preview-gap recovery, saved input before dispatch,
and model cost versus rounded account postings. They do not prove the proposed
APIs, SQL transactions, actual process crashes, live CLI control or voice playback.
