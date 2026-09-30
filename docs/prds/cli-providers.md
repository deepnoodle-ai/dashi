# Choose a provider and control the next turn

Status: Draft\
PRD PR: Pending · Implementation PR: Pending · Updated: 2026-09-30

## Problem and context

A developer moving from Dive needs native, local, and compatible models in one terminal coding agent. Dashi's CLI constructs only OpenAI Responses. Its startup replaces unknown models with `gpt-6-luna`; its settings store only a model name. The [parity matrix](../research/cli-parity-integrations.md) assigns I01–I08 to this slice, with I08's custom prompt shared with `cli-resources`.

Dive supplies provider selection, key detection, a model list, persisted switches, and generation controls. Dashi already supplies [configured clients](../guides/model-calls.md), exact model evidence, and [offline capability fitting](../../design/model-controls.md). Use those contracts. [Pi](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/models.md) separates custom endpoints, model defaults, and restored session selections. [Claude Code](https://code.claude.com/docs/en/model-config) saves interactive selections and identifies settings that override them. Adopt inspectable selection and persistence while preserving Dashi's recorded Turn authority.

## Users and concepts

The primary user is a developer choosing where a coding turn runs and checking which settings apply.

- **Destination:** a stable configured identity, its API, endpoint, and credential source. A model ID alone does not identify a destination.
- **Next-turn settings:** preferences for a new turn. They cannot replace an open turn's recorded model, destination, or controls.
- **Effective controls:** settings encoded by the provider's `Prepare`, including recorded defaults, adjustments, warnings, and unknown support. They do not prove server behavior.

## Use cases and acceptance

### UC-1: Discover and select a destination (I01–I04)

Run `dashi models [--available]`. Open `/model`, or launch with `--provider NAME --model ID`. Choose a native model or a model explicitly registered for a local/compatible destination.

- [ ] Keyless fixtures cover Anthropic Messages, OpenAI Responses and Chat Completions, Google Gemini, Grok, Mistral, Meta, DeepInfra, OpenRouter, and Ollama.
- [ ] Custom destinations select an existing API adapter explicitly. Model IDs, including publisher slashes, reach that adapter unchanged.
- [ ] List and picker show destination, exact model ID, context/output limits, and relevant capabilities. Missing facts show `unknown`, with evidence source/date when available.
- [ ] `--available` means configured for use: required credentials are present or the destination needs none. It performs no network probe or paid request.
- [ ] An empty picker names the missing configuration. Cancellation preserves selection. A successful switch identifies the next-turn destination. Invalid or ambiguous IDs produce an actionable error and no fallback.
- [ ] `--workspace DIR` establishes the canonical workspace for tools, project settings, and session filtering. Invalid directories fail before dispatch.
- [ ] `--api-endpoint URL` requires an explicit destination identity and API configuration. It cannot silently repurpose a native vendor identity or credential.

### UC-2: Persist preferences without changing recorded work (I05–I06)

Set a model with `/model`, inspect `/settings`, reopen Dashi, then continue an interrupted turn recorded against another destination.

- [ ] Startup resolves each preference independently: flag, `DASHI_*` environment, project `.dashi/settings.json`, user `~/.dashi/settings.json`, default.
- [ ] With no model selection, vendor-key detection uses the documented Dive order: Anthropic, Google, OpenAI, Grok, Mistral, DeepInfra, Meta. Otherwise retain the OpenAI default and explain missing credentials. Local/custom routes require explicit selection.
- [ ] `/model` saves the destination/model pair to user settings and changes this session's next turn. Saving preserves unrelated fields and never edits project settings. Save failure leaves the session selection active and reports that it was not saved.
- [ ] `/settings` shows requested next-turn values and their exact winning sources. It distinguishes flag, process environment, explicit environment file, project file, user file, session change, and default.
- [ ] A later launch still applies project, environment, and flag overrides. Inspection explains why a saved user default did not win.
- [ ] Continuing an open turn uses its recorded model, controls, API, and endpoint despite changed preferences. Removed destinations or changed endpoint/API bindings fail clearly before dispatch. Current credentials must satisfy existing replay rules.
- [ ] Legacy OpenAI sessions remain readable. Session metadata cannot override destination facts in a Turn. Unknown provenance is reported instead of invented.
- [ ] `/settings --json` exports the same safe inspection values without keys, authorization headers, environment values, or secret-bearing URLs.

### UC-3: Control generation and inspect adjustments (I07)

Launch with `--thinking-effort`, `--show-thinking`, `--temperature`, or `--max-tokens`. Change the next-turn values with `/effort`, `/thinking`, `/temperature`, and `/max-tokens`.

- [ ] Flags, matching `DASHI_*` variables, and settings accept the same values. Explicit zero temperature, thinking off, and effort omission remain distinct from absence. Each slash command reports its value and persists the user preference.
- [ ] Unset effort, thinking, and temperature add no CLI generation default. Unset output limit uses the library default. `/thinking on` requests summarized thinking through existing `llm.Settings`; it does not promise private reasoning disclosure.
- [ ] Invalid syntax or conflicting controls fail before dispatch and retain prior preferences. Validation uses the selected configured client's `Prepare`, not copied provider rules.
- [ ] The default capability policy remains `CapabilityAdjust`. Known adjustments show requested/effective values and warnings before dispatch. Known unsupported controls with no safe substitute fail. Unknown support is labeled unverified.
- [ ] Inspection separates the next-turn preparation preview from a recorded attempt's effective settings. A preview is not a recorded or dispatched call.
- [ ] Switching models rechecks existing controls. An incompatible switch leaves the previous selection/preferences intact. A continued turn keeps its recorded controls.
- [ ] During a running turn, mutating commands request a retry after it ends. They do not alter the turn or promise unqueued execution.

### UC-4: Load an explicitly named environment file (I08)

Run `dashi --workspace DIR --env-file PATH`. Inspect settings and credential source names before starting work.

- [ ] Dashi loads only explicitly named files. Relative environment-file paths resolve from the invocation directory. Existing process variables win, including explicit empty values.
- [ ] Missing or unreadable named files fail clearly. No shell commands or variable substitutions execute from file content. Inspection shows the file path and selected variable name without its value.
- [ ] A project `.env` is untouched without `--env-file`. A custom endpoint does not receive a native vendor key implicitly. Project configuration cannot silently redirect credentials to another endpoint.
- [ ] `cli-resources` supplies I08's `--system-prompt` behavior and instruction provenance. The integrated example proves that the custom prompt and provider controls coexist without widening workspace or tool authority.

## Decisions and boundaries

- Use explicit destination identities and exact-ID catalog lookup. Reject Dive's model-prefix routing and substring capability inference because model IDs overlap across endpoints.
- Permit a catalog-unknown ID only when explicitly registered for its destination. Report unknown capability evidence. Reject unrestricted typo-prone bare IDs and copied public-endpoint facts.
- Keep user persistence separate from recorded work. Reject changing an open turn or its facts when the user saves a next-turn preference.
- Keep Wonton and offline catalogs. Reject provider discovery requests, stored credentials, subscription sign-in, and command-based credential loading in this slice.

Compaction (#103), accounting (#116), richer `/status`, usage reports, JSONL turns, hosted/media tools, Vertex authentication, and new provider capabilities remain separate. A missing adapter/capability requires a foundation dependency, not CLI emulation. The technical spec must define safe destination trust, credential binding, settings writes, and resume resolution before implementation.

## Success and verification

All I01–I08 outcomes have keyless acceptance evidence, including the shared custom-prompt case. A runnable example covers configuration, preparation, and rejected selection without payment. Wonton recordings at 80×24 cover empty, loading/preparing, error, and success states. There is no network-backed discovery loading state. Loading feedback applies only when local preparation delays display. Independent review exercises selection, persistence, resume, and secret-free export. Existing recovery, session-lock, approval, and queue behavior passes unchanged.

Fixture support does not establish live-provider qualification. No paid probe is required. No unresolved product decision blocks the draft review.
