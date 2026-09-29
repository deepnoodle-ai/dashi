# Session context identity and presence

Status: Accepted  Author: Codex  Date: 2026-09-29  
Workflow: spec, checkpoint, then build. The orchestrator checks direction before implementation.

## Context and goals

The [PRD](../prds/session-context-dedup.md) requires one rule for generic SeedContext items and skill catalogs. Project currently emits only ContextItem.Message. Origin and Name disappear unless encoded incidentally in reminder content. Session concatenates these messages, and compaction stores retained messages directly. The solution preserves slot identity alongside the projected message. It introduces no session store or lifetime setting.

## Message metadata

Add `llm.Message.Context *llm.MessageContext` with JSON key `context,omitempty`. `MessageContext` contains only `Origin string` and `Name string`. It describes a context slot, not an authority grant or a delivery ID. Origin uses the normalized ContextItem origin. Empty ContextItem origin becomes `user`. Name preserves its exact value.

`Message.Clone` independently copies this pointer. Input clones, request snapshots, and CompactionFact.Keep already clone messages and must retain the new field. JSON round trips preserve it. Nil metadata is valid and means unknown context identity. Non-nil metadata requires nonempty Origin and Name. Core validates that identified context uses an existing ContextOrigin and the role/content restrictions of that origin. The llm layer validates structural shape without importing dashi. Names retain ContextItem's existing rules.

Project attaches metadata to each named context delivery after cloning its message. It overwrites any caller-supplied metadata with the recorded ContextItem's normalized Origin and Name. Unnamed deliveries have no slot identity. Started input and caller history preserve explicit metadata but never infer it from text. Keep ContextItem.Message and TurnView.Context as their recorded values.

Providers encode Role and Content as before. Metadata never enters wire bodies, reminder tags, system text, tools, or cache keys. Original Input snapshots retain it. Verify the major provider codecs and generic request paths do not serialize whole messages directly.

## Shared delivery rule

SeedContext remains the shared helper. skill.Bundle builds the same snapshot, calls SeedContext, and sets the returned hook Name to `skills-catalog`. Empty bundle behavior stays unchanged.

For every proposed request:

1. Scan Input.Messages backward for the item's normalized Origin and Name.
2. On the first match, compare the complete message value after removing Context metadata from both sides. Use canonical JSON meaning, as existing core comparisons do. Compare Role, Content, and Source. Do not compare the ContextItem ID, Snapshot, or NextCallOnly fields.
3. Equal values return no delivery. A different value or no matching slot delivers the cloned configured item.

Do not consult Turn.Context as an alternate presence source. It can disagree with the actual supplied history. SeedContext still clears NextCallOnly and requires Name. Changed values append. Old messages retain order and content. Existing duplicates stay in place.

## Delivery IDs

IDs remain identities of individual deliveries. They must not block a restored A after B within one turn. Preserve a configured nonempty ID when that ID is unused in the current turn. If it was used, derive a fresh ID from the configured ID, Turn.ID, and Turn.Revision. Check for collisions and append a numeric suffix if necessary. An empty configured ID remains empty, as currently permitted.

Make recorded ID membership available privately to SeedContext through TurnView. Replay rebuilds it from recorded items. Clone that private membership map when a hook receives its view. No new exported method is necessary. This leaves arbitrary Deliver and steer duplicate-ID rules unchanged. The catalog's digest remains in its ID even when a suffix distinguishes another delivery. No IDs appear in projected identity metadata.

## Compatibility and compaction

Old ordinary records already contain ContextItem Origin and Name. Updated projection annotates them without rewriting records. Old CompactionFact.Keep messages lack identity. Preserve them unchanged and deliver one identified copy. Later turns suppress further copies while that copy remains. Never recover identity through text matching or discarded pre-compaction history.

Updated compactors that retain messages through Clone or JSON preserve identity. A compactor that reconstructs only text loses identity and receives a new delivery. A summary mentioning context never counts as its retained message. If a replacement retains B but drops the later A, a configured A appends again.

Keep `historyFingerprintDomain` and fingerprintMessage's explicit field list unchanged. Context metadata is descriptive and adds no model-visible content. Excluding it preserves history and compaction digests for old records. Standalone `compaction.Record` digests also exclude Context. Its token estimates exclude Context because providers do not send it. Both keep all established Message fields. Metadata receives independent validation. Changes to model-visible values still change digests. Older readers ignore unknown JSON fields, retain established model content, and continue their old per-turn delivery behavior.

## Alternatives and tradeoffs

Reminder-only scanning is smaller but cannot cover plain or multi-block seeds and separate origins. Exact text matching can confuse ordinary user input with a context delivery. Session-only state duplicates turn facts and cannot identify what compaction retained. Explicit message metadata adds a public field and a linear backward scan, but works through existing projection and persistence paths. It can produce one temporary duplicate after old compaction. This is safer than silently withholding context.

## Verification and rollout

Core and skill tests cover unchanged turns, reopened handles, Continue, different origins, changed IDs, A-to-B-to-A, and retained/dropped compaction values. Add a serialized old-compaction fixture and pointer-ownership tests. Pin provider bodies before and after metadata. Pin unchanged history fingerprints and continuation validation. Run root tests and race tests, including prompt-cache and compaction suites. Update hooks, skills, and compaction guides. Commit a scripted public Example showing one seed and one catalog across successive requests. Ship without a flag or record migration.
