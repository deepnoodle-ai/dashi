# Providers and next-turn settings

Choose a **destination** with `--provider`: a stable name for an API, endpoint,
registered models, and credential source. `--model` names an exact registered
ID. IDs such as `acme/code-v1` pass through unchanged. Unknown or ambiguous IDs
fail; Dashi does not substitute another model.

## Discover and select

Try the registration/preparation fixture without payment:

```sh
cd cmd/dashi
go test -run Example_configuredDestinations -v .
```


```sh
dashi models
dashi models --available
dashi --provider anthropic
dashi --provider openai --model gpt-6-luna
```

Listings use offline catalog facts and your registrations. One table shows
each destination's models, their context and output limits in tokens, whether
they take a reasoning effort, and a status: `ready`, or what the destination
still needs, such as a credential variable. Missing facts show `?`, and a
destination with no registered models is left out.
`--available` lists only `ready` destinations: configured credentials and any
required binding trust are present. It makes no network probe or paid request,
and does not establish authentication or server health.

`/model` opens the picker; `/model team acme/code-v1` selects a pair. `/model ID`
requires exactly one destination registering that ID. Cancelling or rejecting a
switch preserves the previous selection. A switch prepares the existing controls
against the candidate before saving it.

Built-in names and credential variables are:

| Destination | Variables, in lookup order |
| --- | --- |
| `anthropic` | `ANTHROPIC_API_KEY` |
| `google` | `GEMINI_API_KEY`, `GOOGLE_API_KEY` |
| `openai` | `OPENAI_API_KEY` |
| `grok` | `XAI_API_KEY`, `GROK_API_KEY` |
| `mistral` | `MISTRAL_API_KEY` |
| `deepinfra` | `DEEP_INFRA_API_KEY`, `DEEPINFRA_API_KEY`, `DEEPINFRA_TOKEN` |
| `meta` | `MODEL_API_KEY`, `META_API_KEY` |
| `openrouter` | `OPENROUTER_API_KEY` |
| `ollama` | optional `OLLAMA_API_KEY` |

DeepInfra, OpenRouter, and Ollama need model registration because the offline
catalog supplies no models for them. Built-in Ollama uses
`http://127.0.0.1:11434/v1/messages` and works without a key.

## Register local or custom destinations

Put complete definitions in `~/.dashi/settings.json`:

```json
{
  "providers": {
    "local": {
      "api": "openai.completions",
      "endpoint": "http://127.0.0.1:11434/v1/chat/completions",
      "models": ["qwen2.5-coder:7b"],
      "default_model": "qwen2.5-coder:7b",
      "credential_env": ""
    },
    "team": {
      "api": "openai.responses",
      "endpoint": "https://proxy.example/v1/responses",
      "models": ["acme/code-v1"],
      "default_model": "acme/code-v1",
      "credential_env": "TEAM_LLM_KEY"
    }
  }
}
```

```sh
dashi --provider local
dashi --provider team --model acme/code-v1
```

Supported `api` values are `anthropic.messages`, `openai.responses`,
`openai.completions`, and `google.generate_content`. Supply the full request
endpoint; Gemini needs `{model}:generateContent` in its path. `default_model`
must appear in the nonempty `models` list. Empty `credential_env` means keyless;
custom names use only their named variable. Inline keys and command-based
credential sources are rejected. Custom endpoints do not inherit native catalog
capability or price evidence.

A workspace `.dashi/settings.json` can define destinations too. Each definition
replaces the whole lower-tier definition. A changed native endpoint/API/credential
binding needs a custom name. `--api-endpoint URL` requires an explicit
`--provider` naming a configured custom destination; it keeps that destination's
API and model registration.

To register models under a built-in name, retain its binding and first credential
variable from the table above, including optional `OLLAMA_API_KEY`:

| Name | API | Endpoint |
| --- | --- | --- |
| `deepinfra` | `openai.completions` | `https://api.deepinfra.com/v1/openai/chat/completions` |
| `openrouter` | `openai.completions` | `https://openrouter.ai/api/v1/chat/completions` |
| `ollama` | `anthropic.messages` | `http://127.0.0.1:11434/v1/messages` |

## Precedence, persistence, and continuation

Startup chooses the destination/model pair from the first tier containing either
selector: the command line (a flag, or else its `DASHI_PROVIDER`/`DASHI_MODEL`
variable); reopened-session preferences; workspace settings; user settings;
credential detection/default. It never fills a
missing selector from a lower tier. Provider-only uses its configured default;
model-only requires one exact match. Controls resolve independently by field
through the same tiers.

With no selection, credential detection tries Anthropic, Google, OpenAI, Grok,
Mistral, DeepInfra, then Meta. Otherwise Dashi keeps OpenAI's `gpt-6-luna`
default, which still needs credentials to send requests. Select local/custom
routes and OpenRouter explicitly.

Interactive changes win until exit and save next-turn session and user
preferences. Writes preserve unrelated user fields and never edit workspace
settings. A save failure reports that the live choice remains active. Selecting a session
through `/sessions` resolves its saved next-turn preferences through the same
tiers. Flags, `DASHI_*` variables, and live interactive changes retain priority.

Continuing an open turn uses its recorded destination, controls, and instructions.
Changing preferences cannot change that work. Removing a destination or changing
its recorded binding blocks continuation; restore its configuration. Legacy
public OpenAI sessions remain readable, without invented routing provenance.

## Controls and public summaries

| Flag / environment variable | Interactive command / settings key |
| --- | --- |
| `--thinking-effort LEVEL\|omit` / `DASHI_THINKING_EFFORT` | `/effort LEVEL\|omit` / `thinking_effort` |
| `--show-thinking[=false]` / `DASHI_SHOW_THINKING` | `/thinking on\|off` / `show_thinking` |
| `--temperature N\|omit` / `DASHI_TEMPERATURE` | `/temperature N\|omit` / `temperature` |
| `--max-tokens N\|default` / `DASHI_MAX_TOKENS` | `/max-tokens N\|default` / `max_tokens` |

JSON uses an empty string to omit effort, `null` to omit temperature, and `0`
for the default output limit. Explicit temperature `0` and thinking `false`
override inherited values. Unset effort, summary request, and temperature add no
CLI default; unset output limit uses the library default.

Provider preparation validates controls with `CapabilityAdjust`. Inspect its
effective values and warnings: supported adjustments may change the request,
unsupported summary opt-in can be omitted, and unknown support stays unverified.
Preparation rejects controls it cannot safely represent. Failed changes retain
prior preferences. Mutating commands during a turn request a retry after it ends.

Summary requests depend on the adapter and selected model. Anthropic additionally
requires supported reasoning effort; use, for example, `--thinking-effort high
--show-thinking`. Effort alone does not request a summary. Chat Completions and
native xAI/Meta routes have no summary opt-in mapping. See
[public reasoning summaries](reasoning-summaries.md) for qualification details.

Public summaries appear separately from replies. Provisional streaming text is
replaced by the finished records and rebuilt from them on reopen. Private
replay data stays hidden. A successful response may contain no summary;
inspection says `no summary returned`. `/thinking off` changes new-turn
requests/display without deleting previous records. Print mode keeps stdout
limited to the reply.

## Workspace, environment files, and guidance

```sh
dashi --workspace ~/work/repo --env-file ./team.env \
  --provider team --system-prompt "Review changes before editing"
```

The workspace must exist; Dashi resolves symlinks before loading workspace
settings, tools, or session lists. Relative environment-file paths resolve from
the invocation directory, even with `--workspace`. Only repeated, explicit
`--env-file` flags load files; `.env` is untouched otherwise. Existing process
variables win, including empty values. Among named files, the first value wins.

Environment files support comments, quoted values, `export`, and `=`/`:`
separators. Malformed lines and invalid variable names are silently skipped.
Duplicate entries within a file use the last value. Shell commands and variable
substitution never execute. Missing/unreadable files stop startup. Files load
after the command line is read, so a `DASHI_*` setting in one has no effect and
draws a warning; set it in the shell or use its flag.

`--system-prompt` requires nonempty text and replaces built-in coding guidance
for this invocation. Workspace facts and contextual instructions remain. It
grants no tool authority or additional filesystem access. Continuation retains
recorded instructions.

## Inspect settings and trust

`/settings` shows the next-turn request, winning/overridden sources, credential
variable/source, named environment files, preparation preview, warnings, and
unverified support. It distinguishes this unsent preview from the last recorded
model call and shows that Turn's Active-Turn protection window. The transcript
does not repeat unverified support or the protection window on every Turn.
`/settings --json` exports safe inspection values without
credential values, headers, system-prompt text, or transcript content.

Project destinations need binding approval unless the whole definition matches
reviewed user settings. The prompt identifies the source, destination, API,
endpoint, models/default, and credential variable. Approval lasts for this
invocation and grants no tool permission. Decline prevents any request.
Unattended runs need a matching user definition; `--yes` cannot grant
destination trust.

Credential-bearing destinations require HTTPS, except HTTP with literal `127/8`
or `::1`. `localhost` and DNS names do not qualify, even when their key variable
is absent or empty. Trust cannot override transport checks. URLs with userinfo,
query strings, or fragments are rejected. Credential-bearing custom calls never
follow redirects.

A successful `/model` switch saves the session's next-turn preferences. It also
saves the destination/model pair globally when the identical binding is
available from user or built-in settings. Project-only and changed project
bindings stay in that session; register their complete binding in user settings
for use across workspaces. Missing required credentials stop setup before
adapter construction or any request. Explicitly keyless custom routes and
Ollama remain available without a key.
