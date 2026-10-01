# Runtime reminders

**Status:** Implemented, API experimental · **Date:** 2026-09-27

Dashi uses typed `llm.Reminder` content for application-supplied context. A
reminder has a stable name, a `contextual` or `operator` tier, and model-visible
content. It occupies its own user-role message in the provider-neutral record. A
tag in ordinary user text cannot create a typed reminder or raise authority.

## Lifetime and record

`Start.Input` and `AddMessages` accept `llm.ReminderMessage`. `BeforeModel` can
return one through `ModelDecision.AddMessages`. The full typed content is saved in
the turn record and reconstructed by `Project`, including after an external tool
wait or process restart. A `ContextItem.Snapshot` item supersedes older items
of the same origin and context-item name: `Turn.Context` returns the latest,
and the reminder instruction tells the model the latest of a name is current.
The model projection keeps the older copies in place and appends the new one,
so the prompt prefix stays cacheable ([prompt caching](prompt-cache.md)).
`NextCallOnly` keeps a recorded item until a later model call shows evidence of
dispatch (a provider response or rejection); after an ambiguous crash the resent
call carries it again, so it may reach the model twice rather than never.
Neither rule edits the saved facts.

This matches Nvoken's choice to keep application context in its authoritative
transcript. Nvoken also deduplicates unchanged named values and carries them
through session compaction; Dashi does not yet own sessions or compaction.

## Authority and preparation

Only an operator-origin `Start.Input` or `ContextItem` may carry an
operator-tier recorded reminder. Provider preparation renders a typed reminder
as `<system-reminder name="...">` text and records a `render_reminder`
`ContentChange`. First-party OpenAI Responses encodes an operator-tier reminder
in a developer message. Contextual reminders remain user messages. Anthropic
Messages, Gemini, and other routes use user messages for both tiers because
Dashi cannot preserve mid-history developer authority on those routes. The
recorded content change shows that reduction. Dashi always adds a system
instruction telling the model to follow the prepared role rather than infer
authority from the tag, so the system prompt, and the provider's prompt cache,
do not change as reminders come and go. Authorization of operator origin
and tool use remains an application policy decision.

Sender tags ([decision 43](decisions.md)): a reminder has an optional `from`
attribute that Dashi sets for agent messages, and rendering escapes
`<system-reminder` and `</system-reminder` in reminder content, so no
reminder can close its tag or forge another. Tool results and user text are
not escaped: a coding agent must see a file's bytes. Origin `ContextAgent`
carries exactly one contextual reminder, in the user role, with a `from`.

This adapts Dive's typed recorded reminders to Dashi's `ContextItem` projection
and provider-neutral preparation boundary. It leaves string-based
`SetSystemReminder` editing behind: changing a recorded reminder requires a new
context item, not mutation of earlier transcript text.
