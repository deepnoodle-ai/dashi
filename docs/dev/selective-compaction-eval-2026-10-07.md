# Selective compaction: corrected recorded comparison, 2026-10-07

All five arms completed all 13 corrected cases and 64 valid saved needles
from the same seven original sessions. Windows use each arm's history after
reasoning is removed, at 80% fill. No sessions or needles were mined again.
GPT-6 Luna wrote every summary and probe, explicitly overriding recorded
models. Reasoning effort was `none`, serving tier `default`, summary ceiling
4,096 tokens, and probe ceiling 1,024. Decide was pinned to `jev-1.13.0`.
The combined implementation head was `ffef901475958d0bbf740e03d65cd97dd4bbf232`.

This completes the corrected comparison requested by
[#221](https://github.com/deepnoodle-ai/dashi/issues/221). It does not isolate
the effect of the window correction from the changed summary/probe models,
and is not a replacement estimate for the historical expensive-model run.

| Arm | Recall | Fallbacks | Kept tokens | Summary $ | Decide tokens | Decide $ |
| --- | --- | --- | --- | --- | --- | --- |
| stock | 20/64 (31.25%) | 0/13 | 60,999 | 0.0220 | – | – |
| stock, request-aware | 12/64 (18.75%) | 0/13 | 60,999 | 0.0418 | – | – |
| recency | 29/64 (45.3125%) | 0/13 | 161,090 | 0.0396 | – | – |
| selective | 24/64 (37.5%) | 0/13 | 105,905 | 0.0429 | 655,312 | 0.027523 |
| summary first | 22/64 (34.375%) | 0/13 | 105,905 | (shared) | (shared) | (shared) |

By kind (error/path/name/value/decision), selective found 5/6/6/7/0;
recency found 8/5/11/4/1, out of 12/11/18/18/5. Selective kept 34.3% fewer
tokens than recency but recalled five fewer needles. Decide cost about 64%
of selective's summary cost. The longest Decide wait was 427 ms.

**Decision:** selective leads stock by 6.25 points and trails recency by
7.8125 points. The 10-point recall bar is not met. Keep the library package
experimental and leave CLI integration out. Zero observed fallbacks in 13
cases does not establish a population fallback rate. More cases and repeat
runs remain unperformed; no further paid run was authorized for this work.

The free preflight quoted $1.048777177 in full retained bounds. Actual
payloads changed that counter; verified successful results settled unused
risk. The atomic runtime cap still applied before every dispatch. All 157
requests (117 model, 40 Decide) completed with 117 verified model settlements
and zero unknown charges in the final run. The final run's observed priced
usage, including summaries, probes and Decide, was $0.227599129.

| Attempt | Assigned cap $ | Full reservations $ | Held exposure $ | Observed priced $ | Unknown calls | Matrix |
| --- | --- | --- | --- | --- | --- | --- |
| Initial transport failure | 0.99 | 0.039347 | 0.039347 | 0 | 3 | incomplete |
| Corrected transport, semantic stop | 0.960653 | 0.069287375 | 0.047469025 | 0.011395075 | 2 | incomplete |
| Final complete run | 0.913183975 | 1.069913427 | 0.283163827 | 0.227599129 | 0 | complete |

The first failure exposed an HTTP/1 ALPN mismatch, fixed before subsequent
runs. The second run's primary semantic stop enums were not retained and
cannot be reconstructed; this prompted private-safe canonical diagnostics.
Those incomplete attempts contribute no recall conclusion. Their uncertain
reserves remain held. Across all attempts, observed priced usage is
$0.238994204 and conservative exposure is $0.369979852, below the user's $1
limit. The $0.130985648 difference is retained risk, not a measured charge.
Gross full reservations count released risk repeatedly and are not spending
or simultaneous exposure. No paid retries, extra smoke calls, or new mining
occurred. These usage figures are not invoice proof.

The original keyless corpus also completed all arms without plumbing errors:
selective's fake oracle recovered 41/64 versus recency 24/64, with zero
fallbacks. This proves saved-needle plumbing, not model quality. Raw sessions,
needles, logs, and provider bodies stay private outside Git; public cases use
per-run ordinals.

