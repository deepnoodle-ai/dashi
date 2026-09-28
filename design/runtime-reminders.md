# Runtime reminders

**Status:** Implemented, API experimental · **Date:** 2026-09-27

Kernel uses typed `llm.Reminder` content for application-supplied context. A
reminder has a stable name, a `contextual` or `operator` tier, and model-visible
content. It occupies its own user-role message in the provider-neutral record. A
tag in ordinary user text cannot create a typed reminder or raise authority.

## Lifetime and record

`Start.Input` and `Deliver` accept `llm.ReminderMessage`. `BeforeModel` can
return one through `ModelDecision.Deliver`. The full typed content is saved in
the turn record and reconstructed by `Project`, including after an external tool
wait or process restart. `ContextItem.Snapshot` replaces older items of the same
origin and context-item name in the model projection; `NextCallOnly` keeps a
recorded item for one model call. Neither rule edits the saved facts.

This matches Nvoken's choice to keep application context in its authoritative
transcript. Nvoken also deduplicates unchanged named values and carries them
through session compaction; Kernel does not yet own sessions or compaction.

## Authority and preparation

Only an operator-origin `Start.Input` or `ContextItem` may carry an
operator-tier recorded reminder. Provider preparation renders a typed reminder
as `<system-reminder name="...">` text and records a `render_reminder`
`ContentChange`. First-party OpenAI Responses encodes an operator-tier reminder
in a developer message. Contextual reminders remain user messages. Anthropic
Messages, Gemini, and other routes use user messages for both tiers because
Kernel cannot preserve mid-history developer authority on those routes. The
recorded content change shows that reduction. When a request carries a reminder,
Kernel adds a system instruction telling the model to follow the prepared
role rather than infer authority from the tag. Authorization of operator origin
and tool use remains an application policy decision.

This adapts Dive's typed recorded reminders to Kernel's `ContextItem` projection
and provider-neutral preparation boundary. It leaves string-based
`SetSystemReminder` editing behind: changing a recorded reminder requires a new
context item, not mutation of earlier transcript text.
