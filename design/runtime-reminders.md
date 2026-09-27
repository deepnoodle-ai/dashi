# Runtime reminders

**Status:** Implemented, API experimental · **Date:** 2026-09-27

Kernel uses typed `llm.Reminder` content for application-supplied context. A
reminder has a stable name, a `contextual` or `operator` tier, model-visible
content, and optional JSON `Details` for the application. It occupies its own
user-role message in the provider-neutral record. A tag in ordinary user text
is only text; it cannot create a typed reminder or raise authority.

## Lifetime and record

| Source | Lifetime | Turn record |
| --- | --- | --- |
| `Start.Input` or `Deliver` with `llm.ReminderMessage` | Projected history; `ContextItem.Snapshot` can replace a same-origin, same-name item, and `NextCallOnly` limits it to one model call. | Full typed content and details. |
| `Bindings.ModelOnlyReminders` | Every model call in one `Apply` or `Stream` invocation, including tool iterations. | Name, tier, and digest on each model intent; no content or details. |
| `ModelDecision.ModelOnlyReminders` | The model call whose `BeforeModel` decision returned it. | The same limited evidence. |

The host must resupply model-only reminders on a later `Continue` or `Accept`
invocation. They are appended to projected messages immediately before the
model request and never enter `Project(Turn)`. A model intent is still recorded
before dispatch; an unresolved intent must be reconciled rather than rebuilt
from its digest. The request snapshot held during preparation contains the
original input, so applications should handle that snapshot as model input.
The model can quote reminder content in its answer; the no-content rule applies
to Kernel's reminder and intent facts, not to model output.

## Authority and preparation

Only an operator-origin `Start.Input` or `ContextItem` may carry an
operator-tier recorded reminder. Model-only reminders come from trusted host
bindings or policy. Provider preparation renders a typed reminder as
`<system-reminder name="...">` text and records a `render_reminder`
`ContentChange`. `Details` never enter the wire body. First-party OpenAI
Responses encodes an operator-tier reminder in a developer message.
Contextual reminders remain user messages. Anthropic Messages, Gemini, and
other routes use user messages for both tiers because Kernel cannot preserve
mid-history developer authority on those routes. The recorded content change
shows that reduction. The engine's fixed system instruction tells the model to follow
the prepared role rather than infer authority from the tag. Authorization of
operator origin and tool use remains an application policy decision.

This adapts Dive's typed reminders and model-only runtime context while using
Kernel's existing `ContextItem` projection, explicit model intents, and
provider-neutral preparation boundary. It leaves Dive's legacy string-based
`SetSystemReminder` editing behind: changing a recorded reminder requires a
new context item, not mutation of earlier transcript text.
