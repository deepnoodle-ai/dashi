# PRD: tolerate surplus typed-tool arguments

| Field | Value |
| --- | --- |
| Status | Implemented and independently reviewed; merged in [PR #66](https://github.com/deepnoodle-ai/dashi/pull/66), 2026-09-28 |
| Roadmap | [Item 10](../remaining-work.md), S / medium difficulty |
| Evidence | [Source survey](10-extra-tool-args-research.md), [tools guide](../../docs/guides/tools.md), [Pi edit-call report](https://github.com/earendil-works/pi/issues/6278) |

## Problem and outcome

An application author declares a typed tool with `tool.Func[T]`. A model supplies every required value correctly but adds a key that `T` does not declare. Dashi rejects the call before the callback or authorization hook runs and asks the model to try again. This costs latency, tokens, and a model attempt for an argument the ordinary Go struct decoder could ignore. Pi reports this failure mode in real edit calls; Dashi's own tests prove rejection but do not establish its frequency in production.

The outcome is predictable typed-tool behavior: a harmless surplus key does not block the common case, declared constraints still protect the callback, and an author can choose the existing closed-object contract for a particular tool. A scripted runner test should show **one** callback execution for `{"query":"yes","extra":1}` and no additional model call to repair its shape. Rejected malformed or missing declared values must still result in zero callback executions.

## Proposed user contract

Keep the existing `tool.Func` call working. By default, its generated JSON Schema leaves ordinary struct objects open at the root and at nested struct locations; absent `additionalProperties` means unknown keys are allowed. The runner validates the effective JSON before `BeforeTool` hooks. The typed executor rejects case-only aliases of declared Go fields. For duplicate object keys, it applies Dashi's decided last-key JSON meaning before both schema validation and Go decoding; a repeated nested object replaces its earlier value instead of merging with it. This keeps raw and JSONB-normalized replay equivalent. For plain Go structs, other undeclared sibling keys are ignored, while declared fields retain their required, type, enum, range, and pointer-null rules. Declared map fields remain maps: their keys and value constraints are not discarded.

Add one per-tool opt-in, provisionally named `tool.WithStrictArguments()`, to retain closed root and nested struct schemas and unknown-field rejection:

```go
lookup, err := tool.Func("lookup", "Find a record", lookupFn)
strictLookup, err := tool.Func("lookup", "Find a record", lookupFn,
    tool.WithStrictArguments())
```

The option controls local `tool.Func` validation and its published schema, **not** provider-side constrained sampling. An open schema guides the model and governs Dashi's local validation; it cannot guarantee what arguments a provider will generate or accept. Hand-written `tool.Definition` schemas, external/MCP tools, and hosted tools keep their existing semantics. No global policy or new argument-normalization type is needed.

`Call.Args` continues to carry the effective raw JSON. A `BeforeTool` hook can inspect, deny, or rewrite it. If rewritten, `Call.RequestedArgs` retains the model's original bytes and the replacement is validated before execution. The callback receives `T` and a cloned `Call`; `Call.Args` is **not sanitized**. Dynamic `any`/`json.RawMessage` fields and custom `UnmarshalJSON` methods can still inspect data within their declared locations. Authors should use a declared map or a custom tool when unknown keys have intended meaning, and choose strict mode when extra keys should fail.

## Acceptance criteria

1. **Default path:** Schema, runner precheck, and direct executor agree that ordinary surplus keys at the root, inside nested structs, and inside struct values of slices/maps are permitted. A plain typed callback sees its declared values and runs once. Raw `Args` in the recorded call and callback retain the surplus key.
2. **Validation retained:** Missing required fields (including a misspelled required name), wrong types, enum/range failures, unsupported null, malformed/trailing JSON, case-only aliases of declared fields, and wrong tool identity prevent callback execution. Alias checks cover nested structs and struct values in slices/maps; both key orders are rejected. Duplicate keys use the last value at every object depth for validation and decoding; tests prove raw and JSONB-like replay agree for valid and invalid last values. Declared map values remain schema checked. A misspelled optional field may be ignored; this is documented as a deliberate tradeoff.
3. **Strict path:** The per-tool option emits `additionalProperties:false` for ordinary struct objects and rejects surplus keys in both runner and direct execution before callback. A runner call rejected this way never reaches `BeforeTool`.
4. **Authorization and record:** A hook sees the raw accepted arguments, can deny them, and can rewrite them. Rewrites are revalidated under the chosen policy, including last-key meaning and alias checks. When the matching typed executor is bound, alias checks run before `BeforeTool`; its binding is reused for immediate execution. A Toolset lookup error stops before hooks. A definition advertised without its executor may reach deferred approval, but cannot gain a new binding and execute in that same run; later binding rechecks before callback execution. `Args`, `RequestedArgs`, outcome state, and model-visible correction text reflect what actually happened; no phantom execution or extra retry is introduced.
5. **Provider wire:** Offline preparation tests inspect both default and strict generated schemas in all four currently relevant wire shapes: OpenAI Responses `tools[].parameters` (`strict:false` remains separate), OpenAI Chat Completions `tools[].function.parameters`, Anthropic `tools[].input_schema`, and Gemini `tools[].functionDeclarations[].parametersJsonSchema`. They prove neither route silently closes an open schema or opens a strict one. Live provider sampling or acceptance requires separate route/model qualification.
6. **Migration and documentation:** Audit every in-repo `tool.Func` call site. Keep the read-only `skill` loader lenient unless its contract gives reason otherwise; use strict opt-in in the approval `deploy`, durable approval `deploy`, and detached `send_email` examples, where an exact argument set should be visible before external effects. Document that this does not replace approval or idempotency. The tools guide and `Func` godoc explain the default, strict option, raw-versus-typed boundary, and an optional-field typo example: `{"query":"yes","regoin":"us"}` silently leaves optional `region` unset in default mode but is refused in strict mode. Release notes call out the changed default.

## Scope and tradeoffs

This changes the default of existing `tool.Func` calls, including downstream tools whose authors implicitly relied on rejection. Optional-field typos can now go unnoticed; strict mode is appropriate when an exact field set is part of the tool's meaning. There is no fuzzy renaming, type coercion, JSON repair, automatic retry, provider-wide strictness switch, or change to output schemas.

Two alternatives were considered. Keeping strictness by default and adding a lenient option avoids a behavior change but leaves every ordinary tool exposed to a needless repair step. Stripping extras into a new effective `Call.Args` would mislead hooks or audit records. Canonicalizing only for validation and typed decoding preserves the raw recorded call and Dashi's existing JSON meaning; hand-written and external tool contracts are unaffected.

## Review outcomes

- The PRD received independent feedback. `WithStrictArguments()` was chosen as the small per-tool opt-in; the call-site audit selected the three side-effecting examples above.
- Two independent code review rounds caught case-folded aliases, a duplicate-key replay mismatch against [decision 27](../decisions.md), and an extra Toolset lookup. The implementation and tests now cover those cases.
