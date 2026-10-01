# Live tests

This is how to verify Dashi locally and the record of paid live qualification
runs against provider routes.

## Ordinary verification

```sh
go test -race ./...
go vet ./...
go build ./...
```

Run the same commands in `providers/google/gemini`, which is a separate module.
Its `go.mod` replaces `github.com/deepnoodle-ai/dashi` with the local checkout.
Ordinary tests use local servers and fixtures, without credentials.

The shared checks live in `internal/providertest`. Each module keeps its own
cases, so Gemini's live tests run from `providers/google/gemini` with the same
variables; commands below that name Gemini show both locations.

## Live final-result routes

For selected live final-result routes, set `DASHI_LIVE_TEST=1` plus
`ANTHROPIC_API_KEY`/`ANTHROPIC_MODEL`, `OPENAI_API_KEY`/`OPENAI_MODEL`, and
`GEMINI_API_KEY`/`GEMINI_MODEL`, then run
`go test -tags live ./providers -run TestLiveAgentFinalFacts -v` and, in
`providers/google/gemini`, `go test -tags live . -run TestLiveAgentFinalFacts -v`.
Each provider makes two paid requests. To test one provider, filter a subtest
such as `-run '^TestLiveAgentFinalFacts/openai$'` and set only its key and
model.

On 2026-09-27, blocking and streaming final-result calls passed for Anthropic
Messages (`claude-haiku-4-5-20251001`), OpenAI Responses (`gpt-4.1-mini`), and
Gemini API (`gemini-2.5-flash`). This qualifies those routes only.

## Live agent turns

On 2026-09-27, Anthropic Messages (`claude-haiku-4-5-20251001`), OpenAI
Responses (`gpt-4.1-mini`), and Gemini API (`gemini-2.5-flash`) agent turns
passed live: a streamed final reply and a local tool call
followed by a second model call. With the corresponding API keys set:

```sh
DASHI_LIVE_AGENT_TEST=1 ANTHROPIC_MODEL=claude-haiku-4-5-20251001 \
  OPENAI_MODEL=gpt-4.1-mini GEMINI_MODEL=gemini-2.5-flash \
  go test -tags live . -run '^TestLive(Anthropic|OpenAI)AgentTurn$' -v
(cd providers/google/gemini && DASHI_LIVE_AGENT_TEST=1 \
  GEMINI_MODEL=gemini-2.5-flash go test -tags live . -run '^TestLiveAgentTurn$' -v)
```

This makes three paid calls per provider and checks the recorded steps, replay,
and duplicate start. Filter `-run` to one provider when needed.

## Prompt caching

`TestLivePromptCache` in `./providers` sends two calls with a long shared
prefix for Claude (default and one-hour lifetimes) and an OpenAI model, and
logs usage and the net saving against uncached input. Set `DASHI_LIVE_TEST=1`
with the keys and models above; `OPENAI_KEY_CHECK_MODEL` (a GPT-5.6 or later
model) adds the key check, and `DASHI_LIVE_CACHE_PAUSE=6m` measures a call
after a pause. It has **not been run** as of 2026-09-29.

## Structured output

### Native direct-call output

The direct OpenAI Responses route has offline blocking and streaming fixtures.
Its paid probe is opt-in and has **not been run or qualified** as of
2026-09-28. Set `OPENAI_API_KEY` and an exact `OPENAI_MODEL`, then run:

```sh
DASHI_LIVE_STRUCTURED_OUTPUT_TEST=1 go test -tags live \
  ./providers/openai/responses -run '^TestLiveNativeStructuredOutput$' -v
```

This makes two paid calls, one blocking and one streaming. Record the date,
model, endpoint, schema, output, and usage from the log before claiming that
model's route is qualified. A local fixture or a skipped probe is not live
qualification.

### Agent turn output

On 2026-09-27, one-call structured-output turns also passed live on these
three routes with a per-turn schema and a recorded `dashi_submit_output` call.
To repeat them, set `DASHI_LIVE_AGENT_TEST=1` and the corresponding provider
keys and model names, then run:

```sh
go test -tags live . -run '^TestLive(Anthropic|OpenAI)StructuredOutput$' -v
```

In `providers/google/gemini`, the Gemini probes drop the provider from the
name: `TestLiveStructuredOutput`, `TestLiveReservedFinalReply`,
`TestLiveIteratorAgentTurn`, and `TestLiveRecordedReminder`.

## Reserved final reply

On 2026-09-27, the reserved final-reply call passed live on the same three
routes: each turn used a local multiplication tool once, sent a second request
without application tools, and returned a reply with `StopLimited` and
`model_calls`. With the same keys and model names, repeat the two-call check
per provider:

```sh
DASHI_LIVE_AGENT_TEST=1 go test -tags live . -run '^TestLiveReservedFinalReply$' -v
```

## Compaction

On 2026-09-28, `TestLiveCompactionContinuity` passed on OpenAI Responses
(`gpt-6-luna`). Three turns, one with a tool call, were compacted into a
summary alone. The agent then replied to a follow-up from the summary. The test
makes about six paid calls:

```sh
DASHI_LIVE_AGENT_TEST=1 OPENAI_MODEL=gpt-6-luna \
  go test -tags live ./compaction -run '^TestLiveCompactionContinuity$' -v
```

## Iterator streaming

On 2026-09-27, the turn iterator (now `Runner.Stream`) also passed one live
streamed turn per provider on those models. Run
`go test -tags live . -run '^TestLiveIteratorAgentTurns$' -v`
with `DASHI_LIVE_AGENT_TEST=1`, keys, and model names to repeat the probe.

## Error reasons

`TestLiveErrorReasons` provokes context window and media rejections and logs
each provider's error body. Each provider is skipped without its usual API key
variable; filter `-run` to one provider when needed:

```sh
DASHI_LIVE_TEST=1 go test -tags live ./providers -run '^TestLiveErrorReasons$' -v
```

The Gemini probe is `TestLiveErrorReasons` in `providers/google/gemini`.

On 2026-09-28 every probe was classified as expected: a corrupt PNG and a TIFF
were rejected as media by Anthropic Messages (`claude-haiku-4-5-20251001`),
OpenAI Responses and Chat Completions (`gpt-4o-mini`), and xAI
(`grok-4.20-0309-non-reasoning`). Input of about 1.5 context windows was
rejected as context overflow by Anthropic, both OpenAI routes, Mistral
(`ministral-3b-2512`), OpenRouter (`mistralai/mistral-small-24b-instruct-2501`),
and DeepInfra (`meta-llama/Meta-Llama-3.1-8B-Instruct`). The recorded bodies
are unit-test fixtures. Rejections are not billed. Gemini was not run because
no key was available. xAI's context overflow was not probed because its
current windows are too large to exceed cheaply. Anthropic accepted 168k input
with `max_tokens` 64000 on a 200k window, so input plus output is no longer a
rejection trigger there.

## Gemini API version

Gemini's conversational client defaults to `v1beta` because the tested `v1`
route rejected function calling. Pin `v1` explicitly only for turns that must
retain its earlier private replay scope; `Prepare` rejects client function
calls there before sending.

## Gemini image input

Gemini LLM image inputs passed live on 2026-09-27 with `gemini-2.5-flash`: two
inline PNGs in blocking and streaming calls, a public image URL, and a Gemini
Files URI. Each response identified image content. Run the opt-in checks with
`GEMINI_API_KEY` and:

```sh
DASHI_LIVE_IMAGE_INPUT_TEST=1 GEMINI_MODEL=gemini-2.5-flash \
  go test -tags live ./providers/google/gemini -run '^TestLiveImageInput$' -v
```

The full test makes four paid LLM calls and temporarily uploads one PNG; filter
a subtest to run one route.

## Media

Selected Gemini image and video routes were also tested live; see
[media qualification](../../design/media-provider-implementation.md). Opt-in
media tests use `-tags live` with `DASHI_LIVE_MEDIA_TEST=1` and
`GEMINI_IMAGE_MODEL`, or `DASHI_LIVE_VIDEO_TEST=1` and the selected
`GEMINI_VIDEO_MODEL`/`GEMINI_OMNI_MODEL`. Filter one test to control paid
calls.

## Coding tools

`workspace/live_test.go` runs an OpenAI agent that fixes a failing test in a
temporary Go module, using only the `workspace` tools and policy. Set
`OPENAI_API_KEY`; `OPENAI_MODEL` defaults to `gpt-6-luna`:

```sh
go test -tags live ./workspace -run Live -v
```

On 2026-09-28 the test passed with `gpt-6-luna`: two approvals, about 8 seconds.
