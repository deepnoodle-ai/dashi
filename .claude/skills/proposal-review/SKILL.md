---
name: proposal-review
description: Turn a proposal or PRD into a review page for Curtis, with context and numbered questions that have options and comment boxes, published as a Claude Artifact. Use when Curtis wants to review a design doc, proposal, or PRD and give direction, or asks for the questions a document leaves open.
argument-hint: <path to proposal or topic>
---

# Proposal review

Build a review page from a proposal so Curtis can give direction quickly. The
page presents the context he needs, then numbered questions, each with a topic,
the question, options to pick from, and a comment box. Every card also has two
fixed flags, "Question doesn't make sense" and "Needs its own analysis", which
the template adds; do not put them in `options`. A copy button compiles
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

4. **Fill the template.** Write the review data as one JSON object to a
   file in the scratchpad directory, for example `budgets-review.json`, then
   build the page with this skill's fill script:

   ```sh
   python3 .claude/skills/proposal-review/fill.py \
     "$SCRATCH/budgets-review.json" "$SCRATCH/budgets-review.html"
   ```

   `$SCRATCH` stands for the scratchpad directory. Never paste text into
   `template.html` by hand. The script checks the data,
   HTML-escapes the title, and writes the JSON so that proposal text such as
   `</script>` cannot close the data block or inject markup. `title` in the
   data becomes the page's `<title>`: a two-to-four-word name such as
   `Budgets Review`. The data looks like this:

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
   Escape nothing yourself; `fill.py` makes the data safe to embed and the
   page escapes text again when it renders. `test_fill.py` in this directory
   pins that behavior.

5. **Publish.** Call the Artifact tool with the file path, `icon: "checklist"`,
   and a one-sentence `description` naming the proposal. To update an existing
   review, publish to the same file path (same session) or pass its `url`.

6. **Report.** Reply with the link and one line per question: its number and
   topic. Nothing else; the page carries the context.

## When Curtis pastes responses back

The pasted text lists each question as `N. Topic — Question`, then an optional
`Flag:` line, `Choice:`, and an optional `Comment:`. Handle flags first:

- **`question does not make sense`**: ignore any choice on that question. Use
  the comment to find what was wrong (a false premise, a missing option, the
  wrong framing, a question already settled), fix the proposal where the
  confusion came from, and rewrite or drop the question.
- **`needs dedicated analysis`**: leave the question open. Brief an agent to
  write a focused analysis in `design/` covering what the comment asks for,
  or add a section to the proposal if it is small. Then build a new review
  page for that question alone.

Otherwise treat a choice as a decision unless the comment says otherwise; a
comment is direction that outranks the chosen option. Update the proposal to
reflect the decisions, record settled items in `design/decisions.md` under a
dated heading, and list what remains open, including flagged questions and
what happens to each. Do not ask a settled question again on a later page.
