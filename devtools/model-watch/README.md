# model-watch

`model-watch` calls each provider's model-list endpoint and compares the
result with Dashi's catalog (the `catalog` package, read from this checkout).
It shows which listed models the catalog lacks and which cataloged models a
provider no longer lists. The list calls are free. It is a report, not a gate:
it exits 0 unless something is broken.

## Run

```sh
make model-watch                 # from the repository root
make model-watch ARGS=--json
cd devtools/model-watch && go run . --provider openai,google
```

Keys come from the environment, or from `.env` in the current directory or
the repository root (set variables win). A provider without a key is skipped.

| Provider | Key variable |
| --- | --- |
| anthropic | `ANTHROPIC_API_KEY` |
| openai | `OPENAI_API_KEY` |
| grok | `XAI_API_KEY` or `GROK_API_KEY` |
| mistral | `MISTRAL_API_KEY` |
| meta | `META_API_KEY` |
| google | `GEMINI_API_KEY` or `GOOGLE_API_KEY` |

Flags: `--provider`, `--env-file`, `--json`, `--no-verify`, `--fail-on-new`
(exit 1 when a new text model is listed).

## Groups

- **new**: listed text models not in the catalog. Candidates to add.
- **missing**: cataloged models the provider did not list. Candidates to
  remove, or models this key cannot see.
- **resolved**: cataloged Anthropic aliases absent from the list that
  `GET /v1/models/{id}` still serves (skip the check with `--no-verify`).
- **snapshots**: dated IDs, such as `gpt-5.4-2026-03-05`, whose undated alias
  is cataloged. A model counts as cataloged also through a catalog entry's
  `snapshot` or a Mistral `aliases` entry.
- **media and other**: models that do not generate text (image, video,
  speech, embedding, moderation, OCR), by name pattern and by Mistral's
  `capabilities` or Google's `supportedGenerationMethods`.
- **ignored**: deliberate skips in [`ignore.json`](ignore.json), each with a
  reason.
