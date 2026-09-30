# Supply screenshots, PDFs, and selected files

Status: Draft\
PRD PR: Not opened · Implementation PR: Not opened · Updated: 2026-09-30

## Problem and context

A developer needs to show the agent a broken screen or supply requirements from a PDF or selected source file.
The CLI currently sends text prompts. It offers no attachment selection, removable preview, or immutable storage for reopening that request.
Reading a repository file through a tool does not provide the same workflow as selecting a Desktop screenshot.

[`content/mention`](../../content/mention/mention.go) already reads bounded text, PNG/JPEG/GIF/WebP images, and PDFs from user-selected paths.
[`content.Ref`](../guides/attachments.md) already records immutable digest/size identities and verifies resolved bytes before each model request.
The missing slice is CLI composition, session-scoped storage, and resolver wiring for [O01/O02](../research/cli-parity-operations.md).

[Dive](https://github.com/deepnoodle-ai/dive/tree/da15e916148fcc93f248c08648d22dfab1a4b73a/experimental/cmd/dive) turns conservative terminal file drops into removable labels.
[Pi](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/usage.md) supports file mentions and dropped images.
[Claude Code](https://code.claude.com/docs/en/interactive-mode) makes selected images visible as numbered chips.
These examples support direct evidence in a coding request. Dashi must additionally retain the exact submitted bytes for recovery.

## Users and concepts

The primary user codes in Dashi's interactive terminal. Script users need the same explicit file selection and failure guarantees.

**Attachment entry** means one numbered draft selection in loading, ready, or error state; an error entry has no usable snapshot.
**Selected attachment** means a ready entry's bounded snapshot, with a visible name, type, and size.
**Attachment preview** means these entries before submission. It supports removal without sending a request.
**Submitted attachment** means the snapshot referenced by a recorded request and retained in that session's storage.
Changing the source path never changes that snapshot.

## Use cases and acceptance

### UC-1: Show the agent the right evidence

Run `/attach ~/Desktop/broken.png`, then `/attach "requirements v2.pdf"`.
Inspect `/attachments`, remove a mistaken selection with `/detach 2`, and send “Fix the screen shown here.”

- [ ] `/attach PATH` stages one regular file without starting a Turn or model call.
  `/attachments` lists entry numbers, states, filenames, source locations, and available type/size or error details.
  `/detach NUMBER` removes that entry, including a failed selection. Invalid arguments preserve the draft and entries.
- [ ] Screen and inline modes show removable attachment labels beside the draft.
  Plain mode prints equivalent numbered metadata and removal instructions.
  All modes retain the request text while files load, inspection runs, or a selected file is removed.
- [ ] Removing a label removes its selected snapshot from the outbound request.
  Removing the last attachment preserves an ordinary text draft.
  Duplicate selection of the same canonical path retains one snapshot and identifies that existing selection.
- [ ] Selection reads bytes once within limits and reports loading, ready, or failure.
  A changed source is not reread on send. Detach and attach again explicitly selects its current bytes.
- [ ] A failed `/attach missing.pdf` creates a visible numbered error entry and blocks dispatch.
  Detaching it permits the remaining request and successful selections to submit; no evidence is silently omitted.
- [ ] Relative paths resolve from the selected `--workspace`, even when Dashi launches elsewhere.
  Absolute paths, `~/`, quoted spaces, shell-style escapes, and local `file://` paths use mention's path rules without shell execution.
  Show when a selected file resolves outside the workspace, including through a symlink.
- [ ] Explicitly selected outside-workspace files remain usable, including Desktop screenshots.
  Selection grants no read root, shell access, write permission, or future model-selected file access.
  File contents remain supplied evidence, not workspace instructions or approval grants.
  Filenames, metadata, and warnings render as printable text, without terminal-control execution.

### UC-2: Mention or drop a file while composing

Write “Use @src/theme.go and @spec.pdf”, or drop a screenshot path into the composer.
Review the selected evidence before submitting the request.

- [ ] A fresh `@file` submission first stages its snapshot, displays the attachment preview, and preserves the request without starting work.
  The notice explains that Enter sends the prepared request after review.
  Edits or removals preserve the remaining selections and recheck new mentions before sending.
- [ ] A failed mention creates a numbered error entry linked to its source span in the draft.
  Detaching it removes its associated failed mention spans, including path/line suffix, while preserving all other request text and successful entries.
  The next Enter sends the remaining request without reselecting the removed mention; only newly entered mentions can select it again.
- [ ] Recognize a terminal drop only when the inserted segment consists entirely of existing, supported file paths.
  Accept quoted/escaped path runs and local file URLs.
  Mixed prose, logs, stack traces, and ordinary path references remain text and cause no automatic file reads.
- [ ] Path completion inserts text only. History browsing and external-editor return perform no file reads.
  Recalled mentions require fresh staging. Recalled labels never recreate a previous attachment or imply its bytes remain selected.
- [ ] `@file:12` preserves the line suffix and stages the file, without claiming to restrict its content to that line.
  Preserve prose, Unicode, newlines, and attachment order. Repeated references attach a file once.
- [ ] An attachment-only request can be submitted intentionally after preview.
  Empty input with no prepared attachment request retains ordinary resume behavior.
- [ ] Approval/question dialog answers, model output, tool output, and resource instructions never trigger selection.
  During active work, attachment mutation or a new attachment submission preserves the draft and requests resubmission after the Turn ends.
  Ordinary text steering keeps its existing behavior. This slice adds no attachment queue or media steering.

### UC-3: Send a request from a script

Run `dashi -p "Compare these requirements" --attach spec.pdf --attach src/theme.go`.

- [ ] Repeatable `--attach PATH` preserves flag order and rejects empty paths.
  It uses the same snapshot, validation, storage, and reference path as interactive selection.
  It requires print mode in this slice.
- [ ] Print prompts can also use explicit `@file` mentions.
  Flags and mentions deduplicate canonical paths and retain deterministic content order.
- [ ] Unattended mode validates and stages directly, without an Enter review step or permission dialog.
  Selection notices/errors use stderr. Attachment handling adds no stdout notices.
- [ ] A failed selected file or storage operation exits nonzero before a model dispatch.
  Do not send only the remaining prompt or silently omit required evidence.
  Existing recorded stop and exit behavior governs later provider/reference failures.

### UC-4: Return to the exact submitted evidence

Submit a screenshot and PDF, edit or delete their source files, then reopen the session and continue the investigation.

- [ ] Before a request can record its attachment references, persist the bounded selected snapshots durably in that session's private storage.
  The Turn stores `content.Ref` identities with digest and size, not source paths as resolver instructions or repeated inline bytes.
- [ ] Acknowledged recorded submission consumes the submitted draft and attachment entries once, even if later execution fails.
  A later plain prompt adds no new attachment blocks; earlier references remain in recorded history.
  Failure known to precede recording preserves the request and entries for correction.
- [ ] An unknown recording outcome preserves the pending submission and blocks resubmission until reloading its record establishes whether it was recorded.
  If recorded, consume the submitted draft once; if established absent, retain it for resubmission under existing recovery rules.
  Continue uses recorded references, never resubmits or consumes an independent draft, and retains snapshots whose recording outcome is unknown.
- [ ] The session-bound resolver authorizes references from earlier Turns in the same session.
  It rejects another session's reference, traversal, unexpected files, and storage symlinks.
  It never follows the original source path or falls back to a URL.
- [ ] Every model request uses the runner's existing reference size/digest verification.
  Reopen and continuation send the original selected bytes even when the source changed or disappeared.
  Provider changes retain those bytes and recheck actual content support.
- [ ] Missing, revoked, corrupt, or oversized stored content produces the existing typed reference failure without dropping the attachment.
  Continue performs normal verification again; it never repeats a settled effect merely to reload content.
- [ ] Recorded transcript inspection shows submitted attachment names/types/sizes without resolving bytes or reading source files.
  Draft previews, input history, and session metadata never claim that a selection was submitted.
- [ ] Exiting, switching sessions, or `/new` identifies memory-only draft selection that would be lost under existing exit controls.
  Unsent drafts do not become recorded requests. Preserve already submitted blobs across later startup/save failures.

## Limits, failure, and decisions

Reuse mention's defaults: image 3.75 MiB, PDF 10 MiB, text 256 KiB, total estimated encoded attachments 32 MiB per request.
Bound reads independently of reported file size. Reject directories, special files, empty files, invalid text, unsupported types, and detectable type mismatches.
Show the selected file, cause, and applicable limit or correction. A failed explicit selection blocks submission until corrected or removed.
Known provider content incompatibility reports the affected attachment before dispatch. Unknown capability remains explicit; provider preparation remains authoritative.

- Use existing mentions, references, and resolver verification. Reject a second upload or transcript-repair protocol.
- Snapshot at visible selection and retain submitted bytes. Reject silently rereading a changed source at send or reopen.
- Keep completion and history navigation free of file reads. Reject automatic attachment of path-looking prose.
- Support local text, supported images, and PDFs first. Clipboard image extraction, rendered thumbnails, remote downloads, audio/video, document conversion, and media generation remain separate workflows.

## Success and risks

Keyless acceptance completes screenshot-based and document-based requests, removes mistaken evidence, and reopens against modified source files.
Counting fixtures prove no model dispatch on selection failure and no repeated settled effects during recovery.
Cover both Wonton UI modes and plain/print entry, including loading, empty, error, removal, and storage corruption states.
Preserve composer editing/history, workspace policy, Turn recovery, provider selection, and compaction checks.
Required implementation, acceptance, security, and taste reviews must pass before delivery.

Stored snapshots consume disk and contain user-selected evidence. Keep them private and session-scoped; retention/deletion commands remain outside this slice.
The technical spec must settle atomic blob publication, reference authorization, draft/input integration, and bounded cleanup without deleting submitted content.
Named independent PRD review precedes that spec and implementation. No remaining product question blocks this draft.
