# CLI attachment snapshots

Status: Accepted · Author: Attachment track · Date: 2026-09-30\
Workflow: Spec checkpoint accepted by root, then implementation. Named PRD re-review returned ready.\
Requirements: [Supply screenshots, PDFs, and selected files](../prds/cli-attachments.md) · [PRD PR #145](https://github.com/deepnoodle-ai/dashi/pull/145)\
Dependency: Active-context requirements [#144](https://github.com/deepnoodle-ai/dashi/pull/144). Its implementation is underway.

## Shape and ownership

The composer owns editable text and numbered attachment entries; Engine owns pending submission, Start identity, FIFO, and acknowledgement.
Session-private storage owns immutable bytes. Turn records remain the authority for submitted input.
The runner's existing resolver verification and active-context guard own model admission.
Do not create an upload service, attachment queue, transcript journal, or additional tool permission.

Private helpers live in `cmd/dashi/attachments.go` and `attachment_input.go`; storage lives in `cmd/dashi/session/attachments.go`.
Root schedules shared-file integration after active-context implementation: screen/inline, Controller, Engine, options/main, and transcript rendering.
Keep Wonton input ownership. Reuse the command inventory for `/attach`, `/attachments`, and `/detach`; add repeatable print-only `--attach`.
No clipboard extraction, thumbnail rendering, remote fetch, media conversion, or automatic retention command enters this slice.

## Selection and bounded reads

Add a small reusable split to `content/mention` while preserving `Expand` behavior:

```go
type Match struct { Start, End int; Path string }
func Scan(input string) []Match // explicit @mentions only; byte spans; no I/O
type File struct {
    Block content.Block
    Attachment Attachment
    CanonicalPath string
    LineSuffix string
}
func ReadFile(path string, opt Options) (File, error)
```

`Scan` uses the existing quote/escape grammar; spans exclude surrounding punctuation and include a line suffix.
`ReadFile` reuses existing path resolution, bounded readers, classification, labels, and defaults.
Resolve a literal filename first; only its absence permits the existing `:12`/`:12-15` suffix fallback.
Legacy `Expand` continues reporting skipped files as warnings and accepting existing bare image/PDF paths.
The CLI uses explicit scans and typed selection errors, never the legacy warning-to-omission behavior.

During selection, canonicalize the resolved target to deduplicate ready entries before opening it again.
Allow explicit symlinks and files outside the workspace; display the resolved location and outside-workspace notice.
Open without blocking on special files, then validate the opened descriptor is regular before reading.
Use a platform helper for nonblocking open where needed; do not rely only on a pre-open Stat.
Sniff and validate from this one bounded descriptor read, rather than reopening for a header or extension check.
Read at most the applicable limit plus one byte from that descriptor. Cancellation checks bound the selection loop; source changes during reading yield the bytes actually read, then freeze them.
Validate UTF-8/no NUL for text and detectable MIME signature/extension mismatches for known media.
Never execute path syntax or terminal-control characters.

Per-file limits stay image 3.75 MiB, PDF 10 MiB, text 256 KiB.
Across ready entries require `sum(4*ceil(size/3)+256) <= 32 MiB`, using checked arithmetic on actual bytes.
Selection is serial outside rendering, so temporary bytes remain within one request plus one bounded file.
Failures retain numbered error entries with cause and correction; errors do not count as ready content or permit dispatch.
Plain and print errors use stderr and never expose file contents.

## Draft transitions and UI

Each entry has a stable draft-local number, generation, requested/resolved path, state, optional immutable bytes/block, and associated mention spans.
Numbers do not shift when an entry is detached. New selection of the same canonical ready path reuses its bytes and entry.
Loading completions return through the UI event loop and apply only to the current entry generation; removal or session change cannot resurrect a stale result.

On fresh interactive Enter, scan explicit mentions and stage only new selections; preserve the exact draft and show preview without Controller prompt dispatch.
A second Enter sends only after all entries are ready and no new mention requires staging.
Track spans against the current draft revision. Rescan grammar on edits without reading files; map unchanged mentions to entries and detach disappeared associations.
Successful outbound text uses mention's labels and preserves line suffixes; the displayed raw draft remains editable before submission.

`/detach N` removes the entry and its associated mention tokens from the current draft, applying span deletions from right to left.
For a failed mention this includes its suffix; retain every other byte, punctuation, and successful entry.
Removing a ready mention also removes that association, so next Enter cannot silently reattach it.
Command-only `/attach` entries have no source span. `/detach` removes their snapshot without changing prose.
Edited spans are resolved by the latest lexical scan, never by stale offsets or unrestricted string replacement.

Recognize a drop only from a main-composer insertion/paste event whose entire inserted segment parses as existing supported paths.
Stage that selection and substitute removable labels for the segment. Mixed prose remains untouched.
If a runtime cannot identify an insertion segment, offer explicit `/attach` or `@file`; never infer a drop from the whole ordinary draft.
Current Wonton input exposes no identifiable insertion segment, so both terminal modes use that explicit selection fallback.
Completion, recall, editor return, dialogs, model/tool output, and rendering never read file contents.
Both Wonton modes show loading/ready/error numbers and removal instructions; plain input provides the same commands and two-step mention review.
Recall stores raw user text only, without snapshot authority.

While foreground work runs, an attachment mutation or attachment-bearing submission preserves the draft and asks for a fresh idle submission.
Lexical detection may identify mentions without reads; ordinary attachment-free text steering remains unchanged.
Selected entries count as unsaved draft state in exit/session-switch controls. Clear them only after the user accepts disclosed draft loss.

## Storage and resolver contract

Use `SESSION/attachments/att_<32 lowercase hex>/` with `blob` and `metadata.json`.
Allocate random IDs per snapshot, never paths or digests as access authority.
Metadata contains version, display basename, MIME type, digest, and size; source paths remain draft-only.
Record a bounded attachment label in the submitted text for inspection, including name/type/size, so images need no new core record field.
Document titles use display basenames. Inspection uses recorded labels, never opens blobs or source files.

Under the existing session lock, write a private same-parent temporary directory, 0600 files, and 0700 directories.
Write and sync both files, close them, rename to the final unused ID, then sync the parent directory before allowing Start recording.
Publication failure stops admission and retains the ready draft bytes for correction; never record a partially published reference.
If publication succeeds, reuse that immutable ID for this submission's retry. Do not republish by rereading the source.
Session metadata saving does not establish or revoke submission.

Bind a resolver to each Engine-opened session and wire it into that session's `Runner.ContentResolver`.
`Resolve(ctx, turnID, ref)` requires a valid session Turn ID matching the bound session and a strictly parsed attachment ID.
Prior-Turn references remain usable under later Turns in that same session; no creation-Turn-only authorization.
Open only the fixed session attachment directory through a descriptor-relative, no-follow storage helper using `os.Root` and platform no-follow opens.
Reject symlinks in the store path, ID directory, metadata, or blob at open time; a check-then-open path sequence is insufficient.
Require regular files, version-1 metadata bounded to 16 KiB, matching metadata/ref digest and size, supported MIME metadata, and `size <= 10 MiB`.
Read at most `ref.Size+1` bytes with context checks. The runner performs final size/digest verification; never cache source bytes across requests or fall back to a path/URL.
Keep default runner reference limits, including aggregate 64 MiB across historical uses; typed failures remain resumable through normal Continue.

Remove only this operation's unpublished temporary directory on failure/cancellation.
Published snapshots, including unreferenced or uncertain ones, are never opportunistically deleted in this slice.
`Store.Discard` must preserve a session with published attachment directories or an unresolved submission even when session metadata is absent.
This favors recovery over disk reclamation; future retention can use authoritative complete Turn scans.

## Submission, pending work, and admission

Construct one immutable `submittedInput` containing draft revision, ordered reference-bearing `llm.Message`, display text, and published snapshot IDs.
Flags precede prompt mentions; deduplicate canonical paths and preserve first-selection order.
Print mode stages directly, rejects any failure before dispatch, and calls the same Engine input route without an Enter review.

Add `Engine.PromptInput(ctx, input)` beside the existing text wrapper; it clones messages into `pendingPrompt.start.Input`.
Generate CommandID once using the existing pending Start path. Never flatten media into `pendingLine.text` or an inbox steer.
If another pending Start prevents admission, return retry-after-pending and preserve this draft; attachment inputs do not enter `later` or Session's volatile queue.
Existing attachment-free text FIFO behavior remains intact.

Publish blobs before entering this pending Start; preserve full reference input while between-Turn compaction runs.
Drive a submission receipt through the UI event loop using submission ID and draft revision, distinguishing pre-record failure, pending/unknown, and recorded.
Do not clear the composer at OnSubmit. Freeze the submitted draft during admission; acknowledge it only from the matching ordinary `StepStarted` or reconciled loaded Start.
Clear its entries once on acknowledgement, even when later model execution fails. Never clear an independently edited/new draft.
Known pre-record failure releases the frozen draft unchanged. Summary Start acknowledgement does not consume it.
Unknown writes retain the pending identity and snapshots; existing `reconcilePrompt` loads and compares the full reference-bearing input before allowing another Start.
Established absence permits retry under existing command identity rules. Conflicts/unreadable records keep admission blocked.
Continue operates on recorded references, not a composer snapshot; it cannot resend or consume an independent draft.

Pass the actual reference-bearing input into all existing Session admission and compaction paths.
The preceding active-context implementation hydrates and prepares the complete request before estimating its size; attachments must reach that guard and cannot be represented only by preview labels.
Extend between-Turn compaction's rough tail estimator to count reference bytes from declared size conservatively, without resolving content or changing coverage digests.
Provider preparation remains authoritative; known incompatibility checks are early diagnostics, not a replacement for it.
An oversized protected original attachment request can stop at the existing context limit; do not silently remove evidence or summarize the objective away.

## Alternatives, release, and proof

Inline record bytes avoid blob storage but duplicate evidence through every checkpoint.
Live-path resolution uses less disk but changes recovery input. Provider uploads reduce repeated transfer but add expiry and credential-bound identities.
Local immutable references preserve Dashi's existing recovery contract at the cost of disk retention, repeated resolution, and a small mention API addition.

Ship in the root dependency chain after active context; existing text-only records require no migration.
Update attachment and CLI guides for explicit selection, private storage, outside-workspace evidence, detach, print flags, and unknown admission.
No implementation or publication starts before root's checkpoint; no product decision remains open.

Keyless fixtures assert exact dispatched bytes/order; two UI modes exercise preview, loading removal, failed `/attach` and failed mention removal, Unicode/quoted spans, duplicate paths, recalled text, and running-work refusal.
Fault fixtures cover known pre-record failure, acknowledged Start followed by failure, and unknown write with present/absent/conflicting records; count Starts and model/tool calls.
Reopen after source edit/deletion and continue across Turns/model changes; reject cross-session refs, traversal, all storage symlinks, corruption, growing files, and special files.
Verify effective attachment bytes trigger context protection, later plain prompts add no new blocks, and Continue preserves an independent draft.
Run formatting, vet, build, race tests in root/CLI modules and actual Wonton terminal recordings; complete implementation, acceptance, security, and taste reviews.
