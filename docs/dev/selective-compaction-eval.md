# Selective compaction: needle-in-history results

The eval in `decide/selective/eval` implements the comparison in
[design/decide/selective-compaction.md](../../design/decide/selective-compaction.md#eval-part-of-v1).
Run it from `decide/`:

```sh
go run ./selective/eval              # keyless: plumbing checks only
go run ./selective/eval -live -max-cost 1 # models and Jev; conservative cap in USD
go run ./selective/eval -live -max-cost 1 -vague # the latest request is "Keep going."
go run ./selective/eval -long-turn   # a large file read in the last turn
go run ./selective/eval -live -max-cost 1 -recorded ~/.dashi/sessions -needles needles.json
```

A recorded case runs on its own provider: an OpenAI case needs
`OPENAI_API_KEY`, and a case of another provider stops the run.
`needles.json` quotes private sessions; it is written with mode 0600 and
ignored by Git. Missing needles require explicit `-live -mine-needles`.

`-live` requires `-max-cost`: one shared USD ledger reserves each request's
conservative cost before dispatch, including full output and cache writes.
Failed or unknown calls retain their full reserve. A complete successful model
result settles unused risk only with verified model/tier and complete priced
usage. Required numeric input, output, cache-read, and cache-write counters
must be present and match normalized usage. Missing counters mean unknown
charges, never measured zero; the [prompt caching guide](https://developers.openai.com/api/docs/guides/prompt-caching)
shows the native cache counters. OpenAI requests explicitly use the default serving tier and verify the
reported tier; see the [Responses reference](https://developers.openai.com/api/reference/resources/responses/methods/create).
Decide reservations remain held in full. Reports separate held reserve,
gross full reservations, and observed cost. SDK and HTTP transports perform zero
retries. Safe failure diagnostics retain category, stage, status, and recognized
code; raw provider errors stay private.
`-preflight` prepares real provider requests with scripted replies and fake
selection. It makes no paid calls. Its reservation total covers those payloads;
real model output can change later payloads, so the live guard checks them again.
Use the provider configuration intended for the live run: credentials qualify
saved private replay, so changing that namespace can reject opaque content.

By default recorded cases keep their own model and provider. An explicit
`-override-recorded-model -provider openai -model gpt-6-luna` compares all cases
on a cheaper common model. `-summary-tokens`, `-probe-tokens`, and
`-reasoning-effort` control output ceilings and reasoning. Reports identify
these changes. The optional `-provider-smoke` check also uses the same reserve.
Public reports use per-run ordinal case IDs and never quote private needles.
Keyless recorded checks validate selected-content preservation. Their oracle
can identify a needle while its segment still exceeds a request or history
budget; omitted needles count as recall misses, not plumbing failures.

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

On recorded CLI sessions the bar is not met. Selective beats the stock
summary by 11 points, but keeping the newest older messages without Decide
recalls as much, so Decide's choice has not yet earned its place; the
design says to drop the package in that case. So the dashi CLI does not
ship it; the library package stays experimental.

## Live run 3, 2026-10-06: recorded CLI sessions

These totals came from an earlier window calculation that measured each
case with reasoning, although every arm gets the history without it. The
eval now measures the history without reasoning (13 cases, 64 needles from
the same mined needles), but this run has not been repeated with it; see
[#221](https://github.com/deepnoodle-ai/dashi/issues/221).

`-recorded` reads dashi CLI session folders. Each turn end of 20,000 to
120,000 estimated tokens is one case, compacted for a window its history
fills to 80%, as above. Its own recorded model writes the summaries and
answers the probe, as in the CLI; reasoning is dropped from every arm's
history, because only the model that wrote it may receive it. Needles are
not planted: Claude Sonnet 5 mines up to six per case from the part every
arm compacts, and code keeps an answer only when it is there word for word
and not in the tail, the user's own words, or a system message. The needles
are saved before any arm runs.

Corpus: 14 cases from 7 sessions, 70 needles (20 names, 19 values, 12
paths, 12 errors, 7 decisions). Six sessions were recorded for this run
with `dashi run --yes`, three prompts each, on scratch copies of six of our
repositories (one with a planted bug): three with Claude Haiku 4.5 and
three with Claude Sonnet 5. The seventh is an earlier GPT-6 Luna security
audit. Three first Haiku recordings stopped when the CLI's in-turn
compaction failed; they were set aside and recorded again with Sonnet.

| Arm | Recall | Fallbacks | Kept tokens | Summary $ | Decide tokens | Decide $ |
| --- | --- | --- | --- | --- | --- | --- |
| stock | 27/70 (39%) | – | 60,999 | 0.7354 | – | – |
| stock, request-aware | 29/70 (41%) | – | 60,999 | 1.0817 | – | – |
| recency | 35/70 (50%) | 0/14 | 174,627 | 1.0212 | – | – |
| selective | 35/70 (50%) | 0/14 | 108,290 | 0.8678 | 690,783 | 0.0290 |
| summary first | 34/70 (49%) | 0/14 | 108,290 | (shared) | (shared) | (shared) |

By kind (error, path, name, value, decision): stock 1, 8, 13, 4, 1;
request-aware 2, 8, 12, 6, 1; recency 7, 6, 17, 3, 2; selective 5, 7, 16,
6, 1. Selective kept 0–5 segments whole and 0–5 short per case, and kept
none in 5 of 14. The longest Decide wait was 738 ms. The run cost $5.10.
OpenAI refused the selective history of the first case ("private replay
data has no lossless canonical conversion") and accepted the stock one,
which kept a single message; a kept assistant message carries the first
provider's private replay data, so a provider switch after a selective
compaction can fail. Three earlier attempts stopped on provider content
rules before printing a result; the fixes above (each case's own model, no
reasoning) came from them.

Against the bar: beats stock by +11 (met); beats request-aware stock by +9
(not met); beats recency by +0 (not met); no fallbacks (met); Decide spend
3% of the summary spend (met). Selective matched recency while keeping 38%
fewer tokens, so later turns cost less; it did not recall more.

## Not yet run

- The request-aware stock arm with the pointed latest request.
- More recorded sessions, and repeated runs to measure run-to-run noise
  on them.
- A stronger summary model, which may close the path gap.
