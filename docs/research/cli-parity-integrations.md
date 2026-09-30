# Provider, instruction, and MCP coverage

The baseline is Dashi `95ec475`, Dive `da15e91` (v1.34.0, experimental CLI 0.1.0), and Wonton v0.2.1. Inspection date: 2026-09-29. **Supported** means implemented CLI behavior. **Partial** means a narrower path. **Missing** means no CLI path, even when a library package exists. These labels do not establish live-provider or cross-terminal qualification.

**V** names files in [Dive's pinned CLI](https://github.com/deepnoodle-ai/dive/tree/da15e916148fcc93f248c08648d22dfab1a4b73a/experimental/cmd/dive). **D** names files in [Dashi's CLI](../../cmd/dashi/). Functions provide the code evidence. Slice numbers refer to the [umbrella PRD](../prds/cli-parity.md). The Dashi CLI and session keyless tests pass at this baseline.

| ID | Dive workflow | Dashi baseline | Evidence and disposition |
|---|---|---|---|
| I01 | Model picker/direct switch across providers | Partial | V `app.go:handleModelCommand/switchModel`, `providers.go`; D `control.go:modelNames` restricts OpenAI. 3. |
| I02 | `models [--available]`, context/capability list | Missing | V `cmd_models.go:runModels`; D `options.go` accepts no subcommands. 3. |
| I03 | Vendor-key autodetection, local/compatible routes | Missing | V `main.go:getDefaultModel`, `providers.go:createModel`; D requires `OPENAI_API_KEY`, creates Responses client only. 3. |
| I04 | `--api-endpoint`, explicit workspace | Missing | V `main.go` flags, `workspace_scope.go`; D cwd only, no endpoint flag. 3. |
| I05 | User/project settings and model environment override | Supported | V `cli_settings.go`; D `settings.go:chooseModel/loadSettings`. D settings contain only model. |
| I06 | Persist `/model` default, show configuration provenance | Missing | V `cmd_settings.go:persistModelSwitch/handleStatusCommand`; D SetModel only changes next-turn model. 3. |
| I07 | Effort, thinking, temperature, max output controls | Missing | V `main.go` flags, `cmd_settings.go`; D `engine.go:agent` sets no generation settings. 3. |
| I08 | `.env`, custom system prompt | Missing | V `main.go:loadDotEnv/defaultSystemPrompt`; D no startup instruction/config path. 3/4. |
| I09 | AGENTS.md then CLAUDE.md startup instructions | Missing | V `main.go:loadStartupInstructionAttachment`; D `engine.go:systemPrompt` only static text. 4. |
| I10 | Skills/catalog/support-file reads/custom slash args | Missing | V `main.go:skill.Load`, `app.go:handleCommand`; D `wire.go:adapters` only workspace. Library `skill` exists. 4. |
| I11 | Skill shell expansion | Missing replacement | V `app.go:cmd.Expand`; D library `skill.Expand` deliberately does no shell execution. Keep explicit approved shell tools. 4. |
| I12 | MCP config tiers, trust/startup, timeout, tool listing | Missing | V `mcp.go:loadMCPServerConfigs/startMCPServers/printMCPReport`; D no MCP adapter. Library `mcp` exists. 5. |
| I13 | MCP legacy SSE and browser OAuth/token store | Missing | Dive library `experimental/mcp/config.go:parseServerEntry`, `client.go:Connect/handleOAuthAuthorization`, `token_store.go`; CLI configuration reaches these paths. D library rejects SSE and has no browser OAuth. Foundation dependency 14. |
| I14 | MCP failure isolation, naming, close child processes | Missing | V `mcp.go:connectMCPServers/Close`; D no lifecycle. 5. |

## Required replacements and dependencies

I08 loads an environment file only when the user explicitly names it. It does not automatically consume a repository `.env`. I11 substitutes skill arguments without executing embedded shell syntax. Approved tools perform shell work. Neither instructions nor skill text grants tool access.

I13 needs a separate remote-MCP foundation contract for OAuth and legacy transport compatibility. Existing [MCP configuration](../../mcp/config.go) rejects SSE. Do not hide this gap behind CLI-local tokens or silently skip an authenticated server. Optional unsupported legacy transports require a documented migration path and explicit server status.
