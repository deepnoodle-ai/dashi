# CLI destinations and next-turn controls

Status: Accepted · Author: Codex · Date: 2026-09-30\
Workflow: Spec before implementation, following the ready independent PRD review.

## Context and boundaries

The [PRD](../prds/cli-providers.md) owns I01–I08, including custom system prompts. Replace the hardcoded Responses client with CLI-owned destination resolution. Preserve Wonton, recorded Turn authority, existing provider preparation, session locks, and approvals. `cli-resources` supplies contextual instructions/skills. #103 owns compaction and #116 owns accounting. No provider adapter, credential store, or replay-policy implementation belongs in this slice.

## Configuration and resolution

Add private CLI helpers for destination definitions/factories, preference resolution/provenance, safe inspection, and persistence. `options.go`, `settings.go`, `control.go`, `app.go`, and `engine.go` remain integration points. Add Gemini's nested module dependency/replacement to the CLI module only.

User/project JSON keeps existing `model` and adds `provider`, `thinking_effort`, `show_thinking`, `temperature`, `max_tokens`, and the PRD's `providers` map. Decode presence separately from value. Zero temperature and false thinking override lower tiers. Empty effort means omission; `temperature: null` clears inherited temperature; zero max tokens selects the library default. Slash commands accept `omit` for effort/temperature and `default` for output limit. Preserve unrelated JSON keys for resources/MCP.

Merge each destination definition as a whole, retaining its source. Never merge a project's endpoint with a user's credential binding. Validate stable nonempty identities, supported APIs, exact nonempty model IDs, membership of `default_model`, and credential variable names. Reject inline keys, command sources, URL userinfo/query/fragment, and invalid URLs. Show the scheme in trust decisions; never rewrite it silently.

Treat nonempty `credential_env` and built-in credential aliases as credential-bearing, even when their values are absent or empty. Validate transport before credential lookup or dispatch-client construction. Require HTTPS except HTTP with a parsed literal IP in `127.0.0.0/8` or exactly `::1`. Do not resolve DNS for exemptions: `localhost`, loopback-resolving names, and other host spellings do not qualify. Keyless HTTP remains permitted when accepted by the adapter. Apply this policy to resolved definitions and `--api-endpoint` overrides, including continued turns. Trust approval cannot bypass it. No insecure-transport opt-in exists.

Built-in native routes use existing constructors and credential aliases. `openai` defaults to Responses; explicit API registration also reaches Chat Completions. Native model facts/defaults come from the catalog and existing destination metadata. Local/compatible models require explicit registration. A custom name uses only its named credential variable, never an implicitly inherited vendor key. Custom endpoints receive no public-endpoint capability or price facts unless separately qualified by the library contract.

The CLI defines built-in `ollama` with `http://127.0.0.1:11434/v1/messages` before validation, rather than inheriting the [constructor's localhost default](../../providers/ollama/ollama.go). Its optional credential alias is `OLLAMA_API_KEY`. Configured endpoints retain their spelling and receive the same transport checks.

After validation and required trust, resolve that variable. Pass its nonempty value, or the adapter's `ollama` placeholder when absent/empty, explicitly as `Config.APIKey` together with `Config.Endpoint`. This prevents constructor-side credential lookup. The built-in remains available without a key. Its binding fingerprint includes the literal endpoint and alias, excluding credential values and the placeholder.

Resolve the destination/model pair from one tier: flags, environment, reopened-session preferences, project, user, autodetection/default. Do not combine selectors across tiers. Provider-only selects its default; model-only needs one exact configured match; pairs validate membership. Generation controls resolve per field through the same tiers. Keep winning and overridden source descriptors. Session changes override startup values until exit. Use the PRD's vendor detection order, with no server probe.

Resolve `--workspace` to an existing canonical directory before settings/session/tool loading. Load only named `--env-file` paths, relative to invocation cwd, before configuration resolution. Use Wonton's literal environment parser and preserve already-present process variables. Track file origin for added variables; parse behavior includes ignored malformed lines and must be documented. File errors stop startup. No shell expansion executes.

## Credential trust and inspection

Treat project destination configuration as untrusted routing input. After transport validation, compare the entire resolved binding with a user-owned definition. Otherwise request interactive trust showing source, identity, API, endpoint, model list/default, and credential variable name. Approval covers the binding fingerprint for this invocation only. Denial, changed binding, or unattended use without a matching user definition fails before dispatch. This trust grants no tool permission. `--yes` cannot authorize it.

Fingerprint canonical destination, API, normalized endpoint, model IDs/default, and credential variable name. Exclude credential values. Validate CLI endpoint overrides against the selected custom identity/API and transport policy; never allow an override to retain a native vendor identity. Include the override in the fingerprint and trust decision. Resolve actual credentials only inside factory construction after validation and trust.

Preserve automatic-redirect refusal for credential-bearing custom destinations. Adapters expose `Config.Client`; any CLI-supplied `http.Client` must refuse redirects with `CheckRedirect` returning `http.ErrUseLastResponse`. The existing [shared dispatch](../../internal/providerhttp/http.go) and [Gemini SDK](../../providers/google/gemini/sdk.go) already force this policy. Treat 3xx as a failed call under existing error semantics; never follow `Location` manually. This blocks HTTPS downgrades and changed endpoints without a foundation change. Transports must not hide redirects or retries.

`models`, `/model`, and `/settings` construct/prepare without sending. Availability means credentials/configuration exist, not authentication or server health. Inspection reports requested controls, source descriptors, preparation adjustments/warnings, unknown support, and evidence dates. JSON export uses a dedicated allowlisted projection, not marshaled config or environment. Exclude keys, headers, prompt text, and arbitrary environment values. Errors print field/source names without echoing rejected secret content.

## Turn routing and recovery

Represent immutable next-turn selection separately from the client driving an open Turn. Before a new start, resolve/trust the destination, construct its client, and validate controls with `Prepare`. Snapshot provider/API/model/settings/instructions into `Agent`; subsequent settings mutations cannot change it.

Use a private forwarding client around immutable configured clients. Select the continued turn's binding before its command. At each synchronous committed `StepTurnStarted` stream boundary, select that new turn's recorded binding before model inspection/preparation. This covers a Continue that automatically starts queued next-turn work on another destination. `Send` uses the client bound to the prepared request, not a later preference. Never change routing during a call. Compaction retains its separately configured #103 client.

Include a versioned route fingerprint in explicit `Agent.Version`, alongside the CLI behavior revision. This records the binding even when interruption precedes the first ModelFact. It records no credential. On continue, read the started Agent and find the current definition matching its provider/API/model and fingerprint. Reject missing or changed bindings before dispatch. Do not reconstruct a lost request or retry an uncertain model effect.

Compare any recorded ModelFact provenance with the prepared destination using existing replay validation. `content.Origin.Scope` is opaque endpoint/credential scope, not a readable endpoint field. Preserve library rules when credentials change. The fingerprint proves binding equality, not remote service behavior.

Legacy `dashi-cli` records from the hardcoded public Responses implementation remain readable. Their missing route fingerprint is reported as unknown. Continue supports that historical public OpenAI route only, with recorded provider/API/model and available provenance checks. Never infer a custom endpoint from mutable session metadata. Other unprovable legacy routes fail with a configuration/recovery explanation.

Session metadata gains optional next-turn preferences, including controls and summary display. Old model-only metadata decodes unchanged. This store remains a convenience default, never execution authority. Before `/model` or another mutating command, validate the complete candidate selection/settings. Apply only successful candidates, then save session/user preferences. Save failure leaves the live selection active with an honest notice.

User writes update only touched fields. Serialize CLI writers with a per-file advisory lock. Reread the JSON under the lock, preserve unrelated keys, and check for external changes before replacement. Write an owner-only temporary file in the same directory, sync, then rename. Refuse symlink destinations and unsafe directory targets. A detected collision requests retry instead of overwriting. The lock cannot serialize uncooperative external editors. Never modify project settings or record credentials.

## Instructions and reasoning summaries

`--system-prompt TEXT` replaces built-in coding guidance, not workspace facts or contextual resource sections. Retain deterministic section order and source descriptors supplied by `cli-resources`. Custom text is recorded in new-turn instructions and is invocation-only. Empty explicit text fails. Continue uses the recorded instructions. Export shows source only. Instruction text grants no authority.

**Reasoning summary requests** is a separate foundation dependency. It must add a provider request control, capability/effective facts and adjustments, and safe public-summary/private-artifact boundaries. Do not emulate that control with prompt text, reasoning budgets, or undocumented request fields. Other provider/settings work can proceed, but I07/full slice completion waits for this dependency.

Once that contract lands, `/thinking on` requests summaries and enables display for new turns. Render public reasoning deltas as provisional blocks distinct from answers. Reconcile them with final recorded reasoning content by attempt/content identity, without duplicated final text. Reopen reconstructs summaries from recorded settled messages; private artifacts remain hidden. Absent summaries produce `no summary returned` in inspection, not invented text. `/thinking off` changes new-turn request/display preferences without deleting records. Unsolicited-text display alone is not parity.

## Alternatives, tradeoffs, and rollout

- A model-prefix registry is shorter but conflates model IDs with destinations. Explicit identities preserve native IDs and endpoint scope at the cost of configuration.
- A provider list fetched online discovers more models but requires network access and ambiguous qualification. Use offline catalogs/registration and label unknown facts.
- Session metadata could pin routing without changing Agent revisions, but it cannot prove the binding before the first attempt after metadata changes. Record the safe fingerprint in the started revision.
- Strict CLI capability rules are simpler to describe but conflict with existing adjust behavior. Consume Prepare and show its effective decisions.

Ship additive settings/session fields and updated CLI documentation. Existing files remain readable. Unknown IDs now fail instead of falling back. Route changes can deliberately prevent continuation; explain the affected identity without weakening recorded provenance. No database migration or paid qualification.

## Verification

Run root/CLI vet, build, and race tests. Keyless fixtures cover every PRD route, exact IDs, provider-only/model-only selection, tier precedence, zero/omitted controls, route trust/denial/change, credential isolation, JSON redaction, atomic-write failure, custom prompts, and continuation before/after the first ModelFact. Continue-to-queue fixtures prove different recorded/next-turn routes, even with overlapping model IDs. Assert no dispatch on rejection or untrusted routes. Preserve recovery, queue, compaction, and approval tests.

Transport fixtures use counters for credential lookup, factory construction, initial requests, and redirected requests:

| Configuration | Required result |
|---|---|
| Remote HTTP + nonempty `credential_env` (present, absent, or empty value) | Reject before lookup/construction/dispatch, even with trusted binding |
| HTTP `127.0.0.1`, another `127/8` IP, or `[::1]` + fixture credential | Accept transport; dispatch only after required trust |
| Built-in `ollama`, `OLLAMA_API_KEY` absent or empty | Validate explicit `127.0.0.1` endpoint before lookup. Construct with `APIKey: "ollama"`. Prepare/dispatch successfully without constructor-side lookup |
| Built-in `ollama`, `OLLAMA_API_KEY` present | Validate the same literal endpoint before lookup. Construct with the fixture key. Prepare/dispatch successfully after required trust |
| HTTP `localhost` or a name resolving to loopback + credential | Reject without DNS-based exemption |
| Keyless local HTTP | Preserve successful preparation/dispatch |
| Credential-bearing HTTPS returns 301/302/303/307/308 to HTTP or another HTTPS endpoint | Fail call; one initial request, zero redirected requests/credentials |

Repeat rejection and loopback cases through `--api-endpoint`; exercise redirect refusal across registered adapters, including Gemini's SDK path.

Commit a runnable no-payment Example for registration/preparation. Capture Wonton empty/preparing/error/success states and interactive selection outside git. Independent implementation, security, and taste reviews exercise integrated workflows. Summary request/render/reopen/absence fixtures gate I07 once its foundation contract lands. No unresolved CLI architectural decision blocks implementation; the summary API remains the named foundation dependency.
