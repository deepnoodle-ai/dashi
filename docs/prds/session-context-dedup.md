# Keep repeated session context to one current copy

Status: Shipped in #117\
PRD PR: #114  Implementation PR: #117  Updated: 2026-09-29

## Problem

Library developers give agents persistent context with `SeedContext` and `skill.Bundle`. Each Session turn delivers another copy while earlier copies remain visible. The audit for [#104](https://github.com/deepnoodle-ai/dashi/issues/104) reproduced one, two, then three copies of both items. A fresh Session handle repeats the defect. Long sessions pay for redundant content and reach compaction sooner.

## Context and users

The primary user builds a durable agent conversation. They want current context in every request without tracking deliveries themselves. `ModelPlan.Input` contains the proposed history, but `Turn.Context` only sees the current turn. Projection preserves message content while losing the ContextItem origin and name for generic messages. Compaction stores kept messages and replaces the rest with a summary. Existing snapshots append their successors to preserve prompt prefixes.

## Concepts

- **Named context:** a ContextItem slot identified by Origin and Name. Empty Origin means ContextUser.
- **Visible context:** a delivered item whose message remains in the proposed model input, including kept compaction messages.
- **Current value:** the last visible delivery for that slot. Earlier values remain historical context.

These definitions apply to seeds and skill catalogs. They introduce no session storage or caller-managed delivery state.

## Use cases and acceptance

### UC-1: Repeat unchanged context

A developer configures `SeedContext(item)` and `dashi.JoinHooks(bundle)`, then calls `Session.Send` three times.

- [ ] Each ordinary request contains one seed and one catalog when neither changes.
- [ ] The result holds for plain-text seeds, typed reminders, and valid multi-block seeds.
- [ ] The result holds after JSON record storage and a fresh Session or Runner handle.
- [ ] Resuming a turn or making several model calls does not add unchanged copies.

### UC-2: Change context and change it back

A developer rebuilds the seed hook or skill bundle with a changed value, then restores the earlier value.

- [ ] Requests append values A, B, then A in that order. The first A does not suppress the restored A.
- [ ] The rule applies across turns and across Continue calls within one turn.
- [ ] A new delivery ID with unchanged content does not force another copy.
- [ ] Earlier messages keep their positions and content. Origin and Name distinguish independent slots.

### UC-3: Compact and continue

A developer uses Session compaction. Its replacement keeps all, some, or none of the context messages.

- [ ] A retained current value prevents another delivery.
- [ ] A removed value receives a new delivery before the next ordinary model request.
- [ ] If only an older value survives, the configured current value appends after it.
- [ ] A summary mentioning the context does not count as its retained message.
- [ ] Reopening after compaction produces the same behavior.
- [ ] Old compaction records without identity receive one new identified delivery. Their retained unidentified copies remain in place.

## Requirements

- R-1: Both helpers use one presence rule over the actual proposed messages, including the current turn's projected deliveries.
- R-2: Suppress delivery only when the latest visible item in the slot has the same complete message value.
- R-3: Preserve context identity in projected messages and kept compaction messages without changing their provider-facing content or role.
- R-4: Ordinary caller-authored messages cannot suppress a seed merely because their text matches it.
- R-5: Keep SeedContext's named-item requirement and NextCallOnly clearing. Keep empty Bundle.Hooks behavior and hook names.
- R-6: Document visibility, changed values, and compaction. Commit a runnable Example that demonstrates unchanged successive Session turns.

## Decisions

- Preserve identity alongside projected messages. Generic ContextItem messages need the same rule as reminders. Reject reminder-only or text matching.
- Compare full message values, excluding delivery identity. IDs identify deliveries. Reject ID-only matching, which can duplicate unchanged values or suppress restored values.
- Compare the latest retained value. A-to-B-to-A must restore A. Reject a search for any historical matching value.
- Append changed values and retain old messages. This follows the prompt-prefix policy. Reject retroactive replacement of messages already sent.
- Read projected input on each call. It reflects compaction and survives handle replacement. Reject in-memory flags and a new session store.

## Scope and risks

Do not deduplicate arbitrary history, alter caller-authored Deliver semantics, or interpret summary text. Do not add manual compaction or change compaction thresholds. Message identity metadata must survive cloning and JSON storage while remaining absent from provider wire content. Existing ordinary turn records gain identity during projection without migration. Old compaction Keep messages cannot prove identity and can temporarily duplicate a new delivery. The tech spec will define the metadata representation and delivery-ID handling before implementation.

## Success

Regression tests cover these acceptance criteria in core and skill packages. Existing prompt-cache, recovery, and compaction tests pass. Reviewers run the committed Example and can observe one unchanged copy per ordinary request.
