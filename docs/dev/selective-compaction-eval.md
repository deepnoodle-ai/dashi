# Selective compaction: needle-in-history results

The eval in `decide/selective/eval` implements the comparison in
[design/decide/selective-compaction.md](../../design/decide/selective-compaction.md#eval-part-of-v1).
Run it from `decide/`:

```sh
go run ./selective/eval              # keyless: plumbing checks only
go run ./selective/eval -live        # Claude (ANTHROPIC_API_KEY) and Jev (TYPESAFE_API_KEY)
```

## Setup

Six generated coding-agent sessions of about 13,000 tokens each, one per
theme, compacted for a 16,000-token window (Base tail 0.25, selective older
share 0.15). Each plants four needles early: an error code in the middle of a
test run, a path in a grep result, a decision with its reason in assistant
text, and a constraint in one user message. The latest request names the
work the needles bear on. A probe turn asks one question per needle; a
reply scores when it contains the planted string (case-insensitive).

Recency uses selective's layout, pinning, and prompt, with every older
segment scored by position through an in-process fake, so `compact.Select`
keeps the newest that fit. Summary first reuses selective's compaction and
moves the summary before the kept segments.

## Live run, 2026-10-02

Claude Haiku 4.5 wrote the summaries and answered the probes; Decide used
`jev-latest`, which resolved to `jev-1.13.0`.

| Arm | Recall | Fallbacks | Kept tokens | Summary $ | Decide tokens | Decide $ |
| --- | --- | --- | --- | --- | --- | --- |
| stock | 18/24 (75%) | – | 21,061 | 0.1340 | – | – |
| recency | 18/24 (75%) | 0/6 | 24,011 | 0.1219 | – | – |
| selective | 24/24 (100%) | 0/6 | 13,241 | 0.1285 | 259,666 | 0.0109 |
| selective, summary first | 24/24 (100%) | 0/6 | 13,241 | (shared) | (shared) | (shared) |

By needle kind (error, path, decision, constraint): stock 6, 1, 5, 6;
recency 6, 1, 5, 6; selective 6, 6, 6, 6. The longest Decide wait was 396
ms. OpenAI `gpt-6-luna` accepted one selective history. Kept tokens are the
six histories after compaction without their summaries. The run cost about
$0.55; probe spend was not tallied in that run (the eval reports it now).

## Against the bar

- Beats stock and recency by 10+ points: met, +25 each.
- Fallbacks under 5%: 0 of 6, met, but six compactions cannot show a rate.
- Decide spend below summary spend: met, $0.011 against $0.129.

## Caveats

- One run, 24 needles: one needle is about four points.
- The sessions are generated, not Decide's labeled transcript or recorded
  CLI sessions, and the latest request points at the needles, which suits a
  relevance judge.
- Stock and recency lose paths: the Haiku summaries dropped five of six.
  A stronger summary model may close part of the gap.
- Selective kept less than either competitor, because the zero floor drops
  segments judged not needed even when room is left.
- Summary first matched summary last, so placement stays as designed.
