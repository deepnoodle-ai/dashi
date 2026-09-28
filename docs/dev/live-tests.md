# Live tests

This is how to verify Kernel locally and the record of paid live qualification
runs against provider routes.

## Ordinary verification

```sh
go test -race ./...
go vet ./...
go build ./...
```

Ordinary tests use local servers and fixtures, without credentials.

## Live final-result routes

For selected live final-result routes, set `KERNEL_LIVE_TEST=1` plus
`ANTHROPIC_API_KEY`/`ANTHROPIC_MODEL`, `OPENAI_API_KEY`/`OPENAI_MODEL`, and
`GEMINI_API_KEY`/`GEMINI_MODEL`, then run
`go test -tags live ./providers -run TestLiveAgentFinalFacts -v`. This makes
six paid requests. To test one provider, filter a subtest such as
`-run '^TestLiveAgentFinalFacts/gemini$'` and set only its key and model.

On 2026-09-27, blocking and streaming final-result calls passed for Anthropic
Messages (`claude-haiku-4-5-20251001`), OpenAI Responses (`gpt-4.1-mini`), and
Gemini API (`gemini-2.5-flash`). This qualifies those routes only.

## Live agent turns

On 2026-09-27, Anthropic Messages (`claude-haiku-4-5-20251001`), OpenAI
Responses (`gpt-4.1-mini`), and Gemini API (`gemini-2.5-flash`) agent turns
passed live using `Engine.Apply`: a streamed final answer and a local tool call
followed by a second model call. With the corresponding API keys set:

```sh
KERNEL_LIVE_AGENT_TEST=1 ANTHROPIC_MODEL=claude-haiku-4-5-20251001 \
  OPENAI_MODEL=gpt-4.1-mini GEMINI_MODEL=gemini-2.5-flash \
  go test -tags live . -run '^TestLive(Anthropic|OpenAI|Gemini)AgentTurn$' -v
```

This makes three paid calls per provider and checks the recorded steps, replay,
and duplicate start. Filter `-run` to one provider when needed.

## Structured output

On 2026-09-27, one-call structured-output turns also passed live on these
three routes with a per-turn schema and a recorded `kernel_submit_output` call.
To repeat them, set `KERNEL_LIVE_AGENT_TEST=1` and the corresponding provider
keys and model names, then run:

```sh
go test -tags live . -run '^TestLive(Anthropic|OpenAI|Gemini)StructuredOutput$' -v
```

## Reserved final answer

On 2026-09-27, the reserved final-answer attempt passed live on the same three
routes: each turn used a local multiplication tool once, sent a second request
without application tools, and returned an answer with `StopLimited` and
`model_attempts`. With the same keys and model names, repeat the two-call check
per provider:

```sh
KERNEL_LIVE_AGENT_TEST=1 go test -tags live . -run '^TestLiveReservedFinalAnswer$' -v
```

## Iterator streaming

On 2026-09-27, `Engine.Stream` also passed one live streamed turn per provider
on those models. Run
`go test -tags live . -run '^TestLiveIteratorAgentTurns$' -v`
with `KERNEL_LIVE_AGENT_TEST=1`, keys, and model names to repeat the probe.

## Gemini API version

Gemini's conversational client defaults to `v1beta` because the tested `v1`
route rejected function calling. Pin `v1` explicitly only for turns that must
retain its earlier private replay scope; `Prepare` rejects client function
calls there before dispatch.

## Gemini image input

Gemini LLM image inputs passed live on 2026-09-27 with `gemini-2.5-flash`: two
inline PNGs in blocking and streaming calls, a public image URL, and a Gemini
Files URI. Each response identified image content. Run the opt-in checks with
`GEMINI_API_KEY` and:

```sh
KERNEL_LIVE_IMAGE_INPUT_TEST=1 GEMINI_MODEL=gemini-2.5-flash \
  go test -tags live ./providers/google/gemini -run '^TestLiveImageInput$' -v
```

The full test makes four paid LLM calls and temporarily uploads one PNG; filter
a subtest to run one route.

## Media

Selected Gemini image and video routes were also tested live; see
[media qualification](../../design/media-provider-implementation.md). Opt-in
media tests use `-tags live` with `KERNEL_LIVE_MEDIA_TEST=1` and
`GEMINI_IMAGE_MODEL`, or `KERNEL_LIVE_VIDEO_TEST=1` and the selected
`GEMINI_VIDEO_MODEL`/`GEMINI_OMNI_MODEL`. Filter one test to control paid
calls.
