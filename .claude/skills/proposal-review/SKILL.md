---
name: proposal-review
description: Turn a proposal or PRD into a review page for Curtis, with context and numbered questions that have options and comment boxes, published as a Claude Artifact. Use when Curtis wants to review a design doc, proposal, or PRD and give direction, or asks for the questions a document leaves open.
argument-hint: <path to proposal or topic>
---

# Proposal review

Build a review page from a proposal so Curtis can give direction quickly. The
page presents the context he needs, then numbered questions, each with a topic,
the question, options to pick from, and a comment box. A copy button compiles
every numbered answer with comments into plain text he pastes back to an agent.

## Steps

1. **Read the proposal.** `$ARGUMENTS` is a file path or a topic. If it is a
   topic, find the document under `design/` (check `design/decisions.md` for
   the decision it serves). Read the whole document and the decision-log entry
   it answers. Read related design notes only as far as needed to understand
   what is actually undecided.

2. **Find the real questions.** List the choices the document leaves open or
   makes tentatively. Keep only the ones whose answer changes what gets built.
   Aim for 4 to 8 questions. Skip anything already decided in
   `design/decisions.md` unless the proposal argues against it. Each question
   needs:
   - `topic`: two to four words naming the area (used as the eyebrow label).
   - `question`: one sentence, a real question mark at the end.
   - `facts`: two to four labeled rows that give the reader the state of
     play for this one question, so it can be scanned without reading the
     document. Each is `{label, text}` or `{label, items: [...]}`. Use short
     labels from this set where they fit: `Today` (what Kernel does now, with
     a file reference), `Proposed` (what the document suggests), `At stake`
     (what applications gain or lose, what it forecloses), `Evidence` (the
     number, incident, or Dive behavior that bears on it), `Depends on` (a
     prior question whose answer settles or moots this one).
   - `why`: optional one-sentence framing when the facts alone do not say
     what hangs on the answer. Prefer `facts`; use `why` sparingly.
   - `options`: two to five, each `{label, desc?, recommended?}`. Labels are
     short noun phrases; `desc` is one sentence on the tradeoff. Mark at most
     one option `recommended: true` and only when the proposal or you have a
     real preference. Do not add an "Other" option; the comment box covers it.
   - `multi: true` only when several options can hold at once.

   Order questions from most consequential to least. Where one answer makes a
   later question moot, say so in that question's `why`.

3. **Write the context.** Labeled rows, the same `{label, text}` or
   `{label, items}` shape as question facts, four to six of them: `Proposes`
   (what the document says to build, in two sentences), `Serves` (the
   decision-log entry and what it left open), `Constraint` (the requirement
   or evidence that most limits the answer), `Recommends` (the document's own
   preference, if it has one), and `Out of scope` when the document draws a
   line worth knowing. Backticks render as code. Write for someone who has
   not read the document today. Plain strings still work but read worse.

4. **Fill the template.** Copy `template.html` from this skill's directory to
   the scratchpad directory under a distinctive file name, for example
   `budgets-review.html`. Replace `__TITLE__` with a two-to-four-word name for
   the page (the proposal's subject, such as `Budgets Review`). Replace
   `__DATA__` with one JSON object:

   ```json
   {
     "id": "budgets-2026-09-27",
     "title": "Budgets Review",
     "source": "design/budgets.md",
     "date": "2026-09-27",
     "context": [
       {"label": "Proposes", "text": "..."},
       {"label": "Serves", "text": "Decision 7; left open where limits live."},
       {"label": "Constraint", "items": ["...", "..."]}
     ],
     "questions": [
       {
         "topic": "Where limits live",
         "question": "Should a budget be declared on the Definition, on Start, or both?",
         "facts": [
           {"label": "Today", "text": "No limit anywhere; `Policy.BeforeSend` is the only hook."},
           {"label": "Proposed", "text": "Declare on Definition, allow Start to narrow."},
           {"label": "At stake", "text": "Whether a limit is a property of the agent or of one run."}
         ],
         "options": [
           {"label": "Definition only", "desc": "One limit per agent; simplest to record."},
           {"label": "Start only", "desc": "Callers set a limit per run; nothing on the agent."},
           {"label": "Both, Start narrows", "desc": "Agent sets a ceiling; a run may lower it.", "recommended": true}
         ]
       }
     ]
   }
   ```

   `id` keys the browser-side draft, so keep it stable if you republish the
   same review. Do not edit the template's CSS or script; the styling is
   deliberately fixed so every review looks the same and takes no design time.
   Escape nothing by hand; the page escapes text itself.

5. **Publish.** Call the Artifact tool with the file path, `icon: "checklist"`,
   and a one-sentence `description` naming the proposal. To update an existing
   review, publish to the same file path (same session) or pass its `url`.

6. **Report.** Reply with the link and one line per question: its number and
   topic. Nothing else; the page carries the context.

## When Curtis pastes responses back

The pasted text lists each question as `N. Topic — Question`, then `Choice:`
and an optional `Comment:`. Treat a choice as a decision unless the comment
says otherwise; treat a comment as direction that outranks the chosen option.
Update the proposal document to reflect the decisions, record any settled
items in `design/decisions.md` under a dated heading, and list what remains
open. Do not ask the same question again on a later review page.
