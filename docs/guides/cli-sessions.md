# Find and inspect saved sessions

Use `/sessions retry handler` to find an earlier investigation in the selected
workspace. The query matches literal, case-insensitive text in titles, IDs,
recorded user requests, steering, and ordinary assistant answers. Spaces stay
part of the query. Tools, instructions, internal summaries, and attached file
bytes are excluded. Search never resolves attachments or calls a model.

Omit the query to list saved sessions. Each result shows its ID, title, update
time, last request, and recorded stop. A text match names its Turn. Cards use
one recorded snapshot. Unreadable records leave explicit gaps. Search warns
when text matches may be missing and shows at most 50 results; refine the query
for more.

In fullscreen and inline modes, use Up/Down to select a session. Page Up/Down
scroll the selected card. Enter reopens it; Escape cancels. Cancellation keeps
the current session and attachment draft. Reopening retains existing draft-loss
warnings, session locks, and recovery checks. In line mode, the command prints
cards and IDs. Exit and run `dashi --resume ID` to reopen a listed session.

Use `/rename rate-limit investigation` while idle to name the current saved
session. Titles contain 1–160 printable Unicode characters on one line.
Surrounding whitespace is removed. Rename needs this process's existing lock
and a saved first request. It changes only the title and update time. Subsequent
requests keep the name. Failed writes retain the previous title.

## Recorded status and recovery

`/status` inspects the current session. `/status ID` inspects another saved
session in this workspace without switching. Status works during a Turn and
never steers, continues, dispatches, acquires a session lock, or grants approval.

During an approval choice or an empty answer field, type `/status [ID]` and
press Enter. From a nonempty own-words answer or denial reason, press Ctrl-S
to open a separate status field without replacing the draft.
The original dialog stays pending. Page Up/Down scroll the report; Escape
cancels inspection and returns to that dialog. Its choices and drafts remain.
An incomplete or unknown slash command cannot answer an approval. Free-text
answers reserve only `/status`; other answer text keeps its normal meaning.

The report separates recorded stop from this process's live activity. Another
session always has unknown worker liveness, even with an interrupted record.
Its guidance names that session and requires reopening it before action.
Acquiring its lock and passing recovery checks remain necessary. A busy reopen
leaves the current session and preferences intact.

For the current owned idle session, guidance uses existing continuation and
limit controls. An uncertain external effect names its exact call. Verify that
effect before `/done CALL_ID`; a mismatched ID records nothing. `/discard` does
not prove an external command ended. A damaged latest record shows unavailable
status; an older Turn never substitutes for it.

Recorded destination/API/model remain distinct from saved next-Turn preferences
and this invocation's next selection. A matching recorded binding can identify
its configured endpoint. Unknown bindings remain unavailable. Endpoint display
removes credentials, userinfo, query text, and fragments. Continuation retains
the recorded binding; missing credentials or changed definitions never cause a
fallback to another provider.

## Accounting

Latest-Turn and session totals stay separate. Session figures include this
session's own summary Turns once and exclude child report totals. Model and
tool money stay separate by currency. No currency conversion occurs. Model-call counts use recorded dispatch evidence,
not an unrecorded host counter. An interrupted call without provider response
evidence can have estimated usage and an unknown charge without a confirmed
dispatched call. Unresolved model intents remain separate.

The report keeps usage uncertainty separate from money uncertainty. Estimated
usage, missing cache-write reporting, token mismatches, partial charges, and
unknown charges remain visible. Omitted unreadable Turns make readable amounts
lower bounds. An unavailable index cannot establish zero spend.

Context occupancy describes only the latest model request's recorded usage and
recorded window. It never uses accumulated session tokens or today's catalog.
Incomplete or estimated usage has no exact percentage. Concurrent work can
advance after a displayed snapshot.

These commands require interactive or `--plain` line input. A command-only
`-p` invocation supports `/help` and quit aliases; other slash commands receive
an actionable rejection before provider dispatch.
