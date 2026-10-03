# Selective compaction: needle-in-history results

The eval in `decide/selective/eval` implements the comparison in
[design/decide/selective-compaction.md](../../design/decide/selective-compaction.md#eval-part-of-v1).
Run it from `decide/`:

```sh
go run ./selective/eval              # keyless: plumbing checks only
go run ./selective/eval -live        # Claude (ANTHROPIC_API_KEY) and Jev (TYPESAFE_API_KEY)
go run ./selective/eval -live -vague # the latest request is "Keep going."
go run ./selective/eval -long-turn   # a large file read in the last turn
```

## Setup

Six generated coding-agent sessions of about 13,000 tokens each, one per
theme, compacted for a 16,000-token window (Base tail 0.25, selective older
share 0.15). Each plants four needles early: an error code in the middle of a
test run, a path in a grep result, a decision with its reason in assistant
text, and a constraint in one user message. By default the latest request
names the work the needles bear on; `-vague` replaces it with "Keep going.".
A probe turn asks one question per needle; a reply scores when it contains
the planted string (case-insensitive).

Five arms compact to the same budget. **Stock** is `compaction.Compactor`.
**Stock, request-aware** adds the latest request to the stock summary
prompt, because the stock summarizer never sees it (it sits in the tail)
while Decide does. **Recency** uses selective's layout, pinning, and prompt,
with every older segment scored by position through an in-process fake, so
`compact.Select` keeps the newest that fit. **Selective** uses Decide.
**Summary first** reuses selective's compaction and moves the summary before
the kept segments.

## Why these results favor selective

Read every number below with these four biases in mind:

1. **The latest request points at the needles** (default sessions). Decide
   sees it in its state; the stock summarizer did not, until the
   request-aware arm.
2. **Most of the gap is paths.** Five of the six extra needles in the first
   run were paths that the Haiku summaries dropped. Errors, decisions, and
   constraints were about equal across arms.
3. **Constraint needles are the user's own words**, so selective and recency
   pin them by construction; they cannot separate those two arms.
4. **The sessions are generated**, with a small last turn, not Decide's
   labeled transcript or recorded CLI sessions.

## Live run 1, 2026-10-02: latest request names the work

Claude Haiku 4.5 wrote the summaries and answered the probes; Decide used
`jev-latest`, which resolved to `jev-1.13.0`. This run predates the
request-aware arm, the current-turn fix, and the shared transcript format
for Decide; its sessions have a small last turn, so the fix does not change
their cut.

| Arm | Recall | Fallbacks | Kept tokens | Summary $ | Decide tokens | Decide $ |
| --- | --- | --- | --- | --- | --- | --- |
| stock | 18/24 (75%) | – | 21,061 | 0.1340 | – | – |
| recency | 18/24 (75%) | 0/6 | 24,011 | 0.1219 | – | – |
| selective | 24/24 (100%) | 0/6 | 13,241 | 0.1285 | 259,666 | 0.0109 |
| summary first | 24/24 (100%) | 0/6 | 13,241 | (shared) | (shared) | (shared) |

By kind (error, path, decision, constraint): stock 6, 1, 5, 6; recency 6, 1,
5, 6; selective 6, 6, 6, 6. OpenAI `gpt-6-luna` accepted one selective
history. The run cost about $0.55.

## Live run 2, 2026-10-03: vague latest request

Same model, sessions, and size, with `-vague`. Jev resolved to `jev-1.13.0`.

| Arm | Recall | Fallbacks | Kept tokens | Summary $ | Decide tokens | Decide $ |
| --- | --- | --- | --- | --- | --- | --- |
| stock | 19/24 (79%) | – | 20,891 | 0.1339 | – | – |
| stock, request-aware | 20/24 (83%) | – | 20,891 | 0.1347 | – | – |
| recency | 18/24 (75%) | 0/6 | 23,895 | 0.1222 | – | – |
| selective | 22/24 (92%) | 0/6 | 18,635 | 0.1270 | 260,390 | 0.0109 |
| summary first | 22/24 (92%) | 0/6 | 18,635 | (shared) | (shared) | (shared) |

By kind: stock 6, 1, 6, 6; request-aware 6, 2, 6, 6; recency 6, 1, 5, 6;
selective 5, 5, 6, 6. Selective kept 0–4 segments whole and 1–3 short per
session; in one session it kept none whole. The longest Decide wait was 324
ms. The run cost $0.79.

With "Keep going." the request-aware arm has nothing useful to add, so it is
a second stock run: its one-needle difference from stock is noise. Stock
itself moved by one needle between the runs on unchanged input.

## Against the bar

| Bar | Run 1 (pointed request) | Run 2 (vague request) |
| --- | --- | --- |
| Beats stock by 10+ points | +25, met on generated sessions | +12, met on generated sessions |
| Beats request-aware stock by 10+ | not run | +8, not met |
| Beats recency by 10+ points | +25, met on generated sessions | +17, met on generated sessions |
| Fallbacks under 5% | 0 of 6, met on generated sessions | 0 of 6, met on generated sessions |
| Decide spend below summary spend | $0.011 vs $0.129, met on generated sessions | $0.011 vs $0.127, met on generated sessions |

One needle is about four points, and six compactions cannot show a fallback
rate.

## Conclusion

The plumbing works, and Decide finds old exact details at about 8% of the
summary's cost, even when the latest request names nothing. Without the
pointed request, the lead over stock fell from six needles to three: four
more paths (which Haiku summaries drop) and one fewer error. Over the
request-aware arm it was two needles, close to the run-to-run noise of one
needle. Selective lost an error needle that every other arm kept; we did not
check why, and a test run kept short, with its middle cut, is one possible
cause.
The data shows that selective helps on these generated sessions; it does not
yet show that the bar is met in general.

## Not yet run

- The request-aware stock arm with the pointed latest request.
- Recorded CLI sessions, as the design requires: the eval reads only
  generated sessions so far.
- A stronger summary model, which may close the path gap.
