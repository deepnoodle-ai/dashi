# Choose a provider and control the next turn

Status: Approved\
PRD PR: Pending · Implementation PR: Pending · Updated: 2026-09-30

## Problem and context

A developer moving from Dive needs native, local, and compatible models. Dashi's CLI supports only OpenAI Responses, replaces unknown IDs with `gpt-6-luna`, and stores only a model name. This slice owns I01–I08 in the [parity matrix](../research/cli-parity-integrations.md), including environment files and custom prompts. `cli-resources` owns contextual AGENTS.md/CLAUDE.md files and skills.

Dive supplies selection, key detection, listings, persistence, and controls. Reuse Dashi's [configured clients](../guides/model-calls.md), exact evidence, and [capability fitting](../../design/model-controls.md). [Pi](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/models.md) separates custom endpoints, defaults, and restored selections. [Claude Code](https://code.claude.com/docs/en/model-config) saves selections and identifies overrides. Adopt inspectable selection while preserving recorded Turn authority.

## Users and concepts

The primary user is a developer choosing where a coding turn runs and checking which settings apply.

- **Destination:** a stable configured identity, its API, endpoint, and credential source. A model ID alone does not identify a destination.
- **Next-turn settings:** preferences for a new turn. They cannot replace an open turn's recorded model, destination, or controls.
- **Effective controls:** settings encoded by the provider's `Prepare`, including recorded defaults, adjustments, warnings, and unknown support. They do not prove server behavior.

## Use cases and acceptance

### UC-1: Configure, discover, and select a destination (I01–I04)

From empty settings, write this to `~/.dashi/settings.json`. Entries register stable identities, APIs, exact endpoints/model IDs, defaults, and credential variables. Empty `credential_env` means no credential.

```json
{"providers": {
  "local": {"api": "openai.completions",
    "endpoint": "http://127.0.0.1:11434/v1/chat/completions",
    "models": ["qwen2.5-coder:7b"], "default_model": "qwen2.5-coder:7b",
    "credential_env": ""},
  "team": {"api": "openai.responses",
    "endpoint": "https://proxy.example/v1/responses",
    "models": ["acme/code-v1"], "default_model": "acme/code-v1",
    "credential_env": "TEAM_LLM_KEY"}
}}
```

Run `dashi models [--available]`. Open `/model`, type `/model team acme/code-v1`, or launch with `--provider team --model acme/code-v1`. `/model ID` follows the model-only rule below.

- [ ] Keyless fixtures cover Anthropic Messages, OpenAI Responses and Chat Completions, Google Gemini, Grok, Mistral, Meta, DeepInfra, OpenRouter, and Ollama.
- [ ] Custom destinations select an existing API adapter explicitly. Model IDs, including publisher slashes, reach that adapter unchanged.
- [ ] A keyless example starts with empty settings, registers/selects both examples, inspects `/settings`, and exits without a prompt. `team` uses a fixture credential. Bad API/endpoint, absent default, or default outside `models` fails validation.
- [ ] List and picker show destination, exact model ID, context/output limits, and relevant capabilities. Missing facts show `unknown`, with evidence source/date when available.
- [ ] `--available` means configured for use: required credentials are present or the destination needs none. It performs no network probe or paid request.
- [ ] An empty picker names the missing configuration. Cancellation preserves selection. A successful switch identifies the next-turn destination. Invalid or ambiguous IDs produce an actionable error and no fallback.
- [ ] `--workspace DIR` establishes the canonical workspace for tools, project settings, and session filtering. Invalid directories fail before dispatch.
- [ ] `--api-endpoint URL` requires an explicit destination identity and API configuration. It cannot silently repurpose a native vendor identity or credential.
- [ ] Project credential use first prompts with source file, destination, API, endpoint, models, and credential variable name. Allow authorizes that binding for this invocation. Denial or changed binding prevents dispatch. Unattended use requires a matching reviewed user definition. Project overrides cannot inherit trust.

### UC-2: Persist preferences without changing recorded work (I05–I06)

Set a model with `/model`, inspect `/settings`, reopen Dashi, then continue an interrupted turn recorded against another destination.

- [ ] Startup selects the destination/model pair from one winning tier: explicit flags, `DASHI_PROVIDER`/`DASHI_MODEL`, reopened-session next-turn preferences, project settings, user settings, autodetection/default. A new session omits the reopened-session tier. Generation controls resolve per field through the same tiers.
- [ ] Provider-only selection uses its configured default, ignoring lower-tier models. Model-only selection needs exactly one configured exact-ID match; zero/multiple matches fail. Pairs validate the ID within their destination. `--provider local` selects `qwen2.5-coder:7b` despite saved OpenAI preferences. `--model acme/code-v1` selects `team` unless another destination registers that ID.
- [ ] With no model selection, vendor-key detection uses the documented Dive order: Anthropic, Google, OpenAI, Grok, Mistral, DeepInfra, Meta. Otherwise retain the OpenAI default and explain missing credentials. Local/custom routes require explicit selection.
- [ ] `/model` saves the destination/model pair to user settings and changes this session's next turn. Saving preserves unrelated fields and never edits project settings. Save failure leaves the session selection active and reports that it was not saved.
- [ ] `/settings` shows requested next-turn values and their exact winning sources. It distinguishes flag, process environment, explicit environment file, project file, user file, session change, and default.
- [ ] Inspection identifies winning and overridden preferences using the specified precedence. Interactive changes win for the next turn until exit and save session preferences.
- [ ] Continuing an open turn uses its recorded model, controls, API, and endpoint despite changed preferences. Removed destinations or changed endpoint/API bindings fail clearly before dispatch. Current credentials must satisfy existing replay rules.
- [ ] Legacy OpenAI sessions remain readable. Session metadata cannot override destination facts in a Turn. Unknown provenance is reported instead of invented.
- [ ] `/settings --json` exports the same safe inspection values without keys, authorization headers, environment values, or secret-bearing URLs.

### UC-3: Control generation and inspect adjustments (I07)

Launch with `--thinking-effort`, `--show-thinking`, `--temperature`, or `--max-tokens`. Change the next-turn values with `/effort`, `/thinking`, `/temperature`, and `/max-tokens`.

- [ ] Flags, matching `DASHI_*` variables, and settings accept the same values. Explicit zero temperature, thinking off, and effort omission remain distinct from absence. Each slash command reports its value and persists the user preference.
- [ ] Unset effort/thinking/temperature adds no CLI default. Unset output limit uses the library default. `/thinking on` and `--show-thinking` request and display provider-supported public reasoning summaries through the dependency below.
- [ ] Invalid syntax or conflicting controls fail before dispatch and retain prior preferences. Validation uses the selected configured client's `Prepare`, not copied provider rules.
- [ ] The default capability policy remains `CapabilityAdjust`. Known adjustments show requested/effective values and warnings before dispatch. Known unsupported controls with no safe substitute fail. Unknown support is labeled unverified.
- [ ] Inspection separates the next-turn preparation preview from a recorded attempt's effective settings. A preview is not a recorded or dispatched call.
- [ ] Switching models rechecks existing controls. An incompatible switch leaves the previous selection/preferences intact. A continued turn keeps its recorded controls.
- [ ] During a running turn, mutating commands request a retry after it ends. They do not alter the turn or promise unqueued execution.
- [ ] Fixtures prove summary encoding, capability rejection/adjustment, streaming/final display without duplication, and display after reopen. Summaries stay distinct from answers; private replay artifacts remain hidden. Absent summaries permit completion with inspection showing `no summary returned`. `/thinking off` stops requesting/showing summaries for new turns without deleting records.

### UC-4: Use explicit environment and system instructions (I08)

Run `dashi --workspace DIR --env-file PATH --system-prompt "Review changes before editing"`. Inspect settings and credential source names before starting work.

- [ ] Dashi loads only explicitly named files. Relative environment-file paths resolve from the invocation directory. Existing process variables win, including explicit empty values.
- [ ] Missing or unreadable named files fail clearly. No shell commands or variable substitutions execute from file content. Inspection shows the file path and selected variable name without its value.
- [ ] A project `.env` is untouched without `--env-file`. A custom endpoint does not receive a native vendor key implicitly. Project configuration cannot silently redirect credentials to another endpoint.
- [ ] `--system-prompt TEXT` replaces built-in coding guidance. Omission keeps the default; explicit empty text fails. Workspace facts and contextual instructions/skills retain their sources. Custom guidance grants no tool authority or wider roots.
- [ ] A fixture records custom instructions alongside provider controls. Continue retains recorded instructions despite changed launch text. Inspection names the prompt source without exporting its text. This slice owns the flag, composition, provenance, and acceptance; `cli-resources` supplies contextual instructions/skills.

## Foundation dependency: Reasoning summary requests

`llm.ReasoningSettings` expresses effort/budget, not a summary request. CLI rendering discards reasoning deltas. A foundation change must define the request control, capability/effective facts, public text, and private replay boundaries. This slice owns UC-3 rendering/reopening acceptance, including unsupported routes and absent summaries. I07 and full slice parity remain incomplete until both land. Displaying unsolicited reasoning alone does not complete parity.

## Decisions and boundaries

- Use explicit destination identities and exact-ID catalog lookup. Reject Dive's model-prefix routing and substring capability inference because model IDs overlap across endpoints.
- Permit a catalog-unknown ID only when explicitly registered for its destination. Report unknown capability evidence. Reject unrestricted typo-prone bare IDs and copied public-endpoint facts.
- Keep user persistence separate from recorded work. Reject changing an open turn or its facts when the user saves a next-turn preference.
- Keep Wonton and offline catalogs. Reject provider discovery requests, stored credentials, subscription sign-in, and command-based credential loading in this slice.

Compaction (#103), accounting (#116), richer `/status`, usage reports, JSONL turns, hosted/media tools, Vertex authentication, and other new provider capabilities remain separate. The technical spec must define safe destination trust, credential binding, settings writes, and resume resolution before implementation.

## Success and verification

All I01–I08 outcomes have keyless acceptance evidence, including custom prompts and the reasoning-summary dependency. A runnable example covers registration, selection, preparation, and rejection without payment. Wonton recordings at 80×24 cover empty, preparing, error, and success states. Independent review exercises selection, persistence, resume, and secret-free export. Existing recovery, session-lock, approval, and queue behavior passes unchanged.

Fixture support does not establish live-provider qualification. No paid probe is required. The [accepted technical spec](../design/cli-providers.md) defines implementation boundaries. Reasoning-summary completion remains dependency-gated.
