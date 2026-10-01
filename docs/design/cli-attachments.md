# CLI attachment snapshots

Status: Accepted · Date: 2026-09-30\
Requirements: [Supply screenshots, PDFs, and selected files](../prds/cli-attachments.md) · [PRD PR #145](https://github.com/deepnoodle-ai/dashi/pull/145)\
Dependency: Active-context [#147](https://github.com/deepnoodle-ai/dashi/pull/147) is merged. Wonton v0.3.0 is the baseline; drops require a reviewed public official Wonton hook commit.

## Shape and ownership

The composer owns editable text and numbered attachment entries; Engine owns pending submission, Start identity, FIFO, and acknowledgement.
Session-private storage owns immutable bytes. Turn records remain the authority for submitted input.
The runner's existing resolver verification and active-context guard own model admission.
Do not create an upload service, attachment queue, transcript journal, or additional tool permission.

Composer helpers: `cmd/dashi/attachments.go`, `attachment_input.go`; storage: `cmd/dashi/session/attachments.go`.
Root coordinates shared screen/inline, Controller, Engine, and entry-point integration.
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

Fresh mention Enter stages new selections, preserves the draft, and previews without prompt dispatch.
A second Enter sends only ready entries with no unstaged mention.
Track spans against the current draft revision. Rescan grammar on edits without reading files; map unchanged mentions to entries and detach disappeared associations.
Successful outbound text uses mention's labels and preserves line suffixes; the displayed raw draft remains editable before submission.

`/detach N` removes the entry and its associated mention tokens from the current draft, applying span deletions from right to left.
For a failed mention this includes its suffix; retain every other byte, punctuation, and successful entry.
Removing a ready mention also removes that association, so next Enter cannot silently reattach it.
Command-only `/attach` entries have no source span. `/detach` removes their snapshot without changing prose.
Edited spans are resolved by the latest lexical scan, never by stale offsets or unrestricted string replacement.

### Saved-file drops

Wonton v0.3.0 consumes paste before `OnKey`; `OnChange` also sees recall/completion. A separate, reviewed commit in the official Wonton repository must add:

```go
func (f *InputFieldView) OnPaste(fn PasteHandler) *InputFieldView
func (i *InputView) OnPaste(fn PasteHandler) *InputView
```

Wire existing `tui.PasteInfo`/`PasteHandler` through shared input state. A nil hook preserves existing behavior.
Call once before insertion with decoded text in `PasteInfo.Content`; preserve decoder normalization.
Wonton privately retains the pre-edit value and native insertion range, including actual offsets behind paste placeholders. Export no cursor/range API.
`PasteAccept` inserts normally; `PasteModified` inserts returned text at that range; `PasteReject` changes nothing.
Rejection and empty modification consume the event without changing draft/cursor or bubbling. `OnChange` fires only for changed values.
Typing, recall, completion, editor return, and binding updates never invoke the hook.

Install only on Dashi's main composer. Check `len(PasteInfo.Content)` before path parsing: recognition is limited to 64 KiB of UTF-8 content.
For a larger segment, return `PasteAccept` unchanged without classification, selection, or file/provider reads.
Show one fixed notice that it was inserted as text and directs the user to undo it, drop a smaller batch, or use `/attach PATH`.
This is an attachment-recognition bound, not a global decoder or ordinary-paste limit; no oversized segment is claimed as a recognized drop.
Within the byte bound, parse the whole segment without I/O as local absolute, `~/`, or `file://` paths.
Decode quotes/escapes and surrounding whitespace, never shell syntax. Mixed prose uses `PasteAccept` unchanged without a selection error.
Keep at most 64 decoded candidate paths. If more tokens exist, validate the remaining bounded segment without allocating more paths.
Only a wholly path-only run can produce a count error; prose anywhere in the segment preserves ordinary paste.
Before synchronization or reservation, reject a recognized run with more than 64 candidate paths or `len(existingEntries)+len(candidatePaths) > 64`.
Count spellings before filesystem deduplication and all existing loading/error/ready entries; canonical aliases do not waive this conservative preflight.
Return `PasteModified, ""` with one fixed notice directing `/attachments`, `/detach N`, or a smaller batch.
Leave draft/cursor, entries, IDs, and generation unchanged. Make no filesystem or provider call; do not reserve a partial run.
For accepted idle, unfrozen runs, synchronize the existing draft, stage with `stageAttachments(paths, true)`, and return `PasteModified, ""`.
Like `/attach`, entries have no source spans. Footer labels hold selections; detach never rewrites prose.
Reserve loading entries before returning; the existing serial worker resolves/reads them. Invalid paths remain numbered errors.
Reuse bounds, canonical deduplication, generation guards, immutable snapshots, and outside-workspace disclosure.
Enter cannot overtake staging or bypass readiness/errors. Ready snapshots send normally; only fresh mentions retain the two-Enter rule.

During work or frozen admission, consume recognized path runs without selection/reads, preserve draft/cursor, and request idle resubmission.
Dialogs retain ordinary paste. Completion, recall, editor return, model/tool output, and rendering remain read-free.
Both Wonton modes share the hook. Unbracketed typing lacks provenance; `/attach` and `@file` remain the fallback, including plain mode.
Paths must exist on Dashi's host; SSH transfers no desktop bytes. Clipboard image extraction remains separate.
Recall stores raw prose without snapshot authority.

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
After publication, save the non-authoritative header under its lease with the next-turn destination/model, permitting crash reopen.
A header alone never lists completed work or authorizes acknowledgement; recorded Start facts do.

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
Active-context admission hydrates/prepares complete requests; preview labels cannot replace attachments. Preserve its media-estimate-unavailable warning and context-sizing exception.
Preserve monetary admission and provider preparation; that exception never proves tokenizer fit.
Extend between-Turn compaction's rough tail estimator to count reference bytes from declared size conservatively, without resolving content or changing coverage digests.
Provider preparation remains authoritative; known incompatibility checks are early diagnostics, not a replacement for it.
An oversized protected original attachment request can stop at the existing context limit; do not silently remove evidence or summarize the objective away.

## Alternatives, release, and proof

Inline bytes duplicate checkpoints; live paths change recovery input; provider uploads add expiry/credential identities.
Local immutable references preserve recovery but require disk retention and repeated resolution. For drops, consume identified paste into entries.
Rejected: whole-value diffs confuse paste with recall/completion; injecting `@file` tokens changes the draft unnecessarily.

Ship in the root dependency chain after active context; existing text-only records require no migration.
Update attachment and CLI guides for explicit selection, private storage, outside-workspace evidence, detach, print flags, and unknown admission.
Drop integration requires a reviewed public hook commit in the official Wonton repository.
Require native input proof in fullscreen/inline before Dashi integration; then pin its exact SHA through a Go pseudo-version.
The upstream PR may remain pending; no merge, release, or tag is required. Wonton v0.3.0 lacks this hook.

Keyless fixtures assert exact bytes/order and both UI modes: loading/removal, errors, Unicode/quoted spans, duplicates, recall, and running-work refusal.
Drop proof covers insertion inside a Unicode draft, escaped screenshots, multiple paths, local URLs, invalid files, source deletion, and paste+Enter batching.
Boundaries cover 64/65 paths, exactly 64 KiB/one byte over (including Unicode), 63 existing entries plus two paths, and a full 64-entry draft.
Assert atomic count rejection preserves draft/cursor/IDs/generation with no reservation, file read, or provider call; include missing files and duplicate spellings.
Oversized path text and oversized multiline mixed prose use native paste unchanged with no attachment recognition or reads.
Within the byte bound, a 65-path prefix followed by prose remains ordinary paste, with no partial selection or count notice.
Drive the actual InputField with bracketed and unbracketed input; verify no reads from mixed prose, history, completion, editor return, dialogs, or frozen drafts.
Qualify a saved-file desktop drop in named terminals for fullscreen/inline; record transport and fallback. Clipboard-byte support remains unclaimed.
Fault fixtures cover known pre-record failure, acknowledged Start followed by failure, and unknown write with present/absent/conflicting records; count Starts and model/tool calls.
Reopen after source edit/deletion and continue across Turns/model changes; reject cross-session refs, traversal, all storage symlinks, corruption, growing files, and special files.
Verify effective attachment bytes trigger context protection, later plain prompts add no new blocks, and Continue preserves an independent draft.
Run formatting, vet, build, race tests in root/CLI modules and actual Wonton terminal recordings; complete implementation, acceptance, security, and taste reviews.
