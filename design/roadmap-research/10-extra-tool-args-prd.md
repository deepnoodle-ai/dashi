# PRD: tolerate surplus typed-tool arguments

| Field | Value |
| --- | --- |
| Status | Draft for independent review, 2026-09-28 |
| Roadmap | [Item 10](../remaining-work.md), S / medium difficulty |
| Evidence | [Source survey](10-extra-tool-args-research.md), [tools guide](../../docs/guides/tools.md), [Pi edit-call report](https://github.com/earendil-works/pi/issues/6278) |

## Problem and outcome

An application author declares a typed tool with `tool.Func[T]`. A model supplies every required value correctly but adds a key that `T` does not declare. Dashi rejects the call before the callback or authorization hook runs and asks the model to try again. This costs latency, tokens, and a model attempt for an argument the ordinary Go struct decoder could ignore. Pi reports this failure mode in real edit calls; Dashi's own tests prove rejection but do not establish its frequency in production.

The outcome is predictable typed-tool behavior: a harmless surplus key does not block the common case, declared constraints still protect the callback, and an author can choose the existing closed-object contract for a particular tool. A scripted runner test should show **one** callback execution for `{"query":"yes","extra":1}` and no additional model call to repair its shape. Rejected malformed or missing declared values must still result in zero callback executions.

## Proposed user contract

Keep the existing `tool.Func` call working. By default, its generated JSON Schema leaves ordinary struct objects open at the root and at nested struct locations; absent `additionalProperties` means unknown keys are allowed. The runner validates the effective JSON before `BeforeTool` hooks. The typed executor validates against that same schema, then decodes with ordinary `encoding/json` behavior. For plain Go structs, undeclared sibling keys are ignored, while declared fields retain their required, type, enum, range, and pointer-null rules. Declared map fields remain maps: their keys and value constraints are not discarded.

Add one per-tool opt-in, provisionally named `tool.WithStrictArguments()`, to retain closed root and nested struct schemas and unknown-field rejection:

```go
lookup, err := tool.Func("lookup", "Find a record", lookupFn)
strictLookup, err := tool.Func("lookup", "Find a record", lookupFn,
    tool.WithStrictArguments())
```

The option controls local `tool.Func` validation and its published schema, **not** provider-side constrained sampling. Hand-written `tool.Definition` schemas, external/MCP tools, and hosted tools keep their existing semantics. No global policy or new argument-normalization type is needed.

`Call.Args` continues to carry the effective raw JSON. A `BeforeTool` hook can inspect, deny, or rewrite it. If rewritten, `Call.RequestedArgs` retains the model's original bytes and the replacement is validated before execution. The callback receives `T` and a cloned `Call`; `Call.Args` is **not sanitized**. Dynamic `any`/`json.RawMessage` fields and custom `UnmarshalJSON` methods can still inspect data within their declared locations. Authors should use a declared map or a custom tool when unknown keys have intended meaning, and choose strict mode when extra keys should fail.

## Acceptance criteria

1. **Default path:** Schema, runner precheck, and direct executor agree that surplus keys at the root, inside nested structs, and inside struct values of slices/maps are permitted. A plain typed callback sees its declared values and runs once. Raw `Args` in the recorded call and callback retain the surplus key.
2. **Validation retained:** Missing required fields (including a misspelled required name), wrong types, enum/range failures, unsupported null, malformed/trailing JSON, and wrong tool identity still prevent callback execution. Declared map values remain schema checked. A misspelled optional field may be ignored; this is documented as a deliberate tradeoff.
3. **Strict path:** The per-tool option emits `additionalProperties:false` for ordinary struct objects and rejects surplus keys in both runner and direct execution before callback. A runner call rejected this way never reaches `BeforeTool`.
4. **Authorization and record:** A hook sees the raw accepted arguments, can deny them, and can rewrite them. Rewrites are revalidated under the chosen policy. `Args`, `RequestedArgs`, outcome state, and model-visible correction text reflect what actually happened; no phantom execution or extra retry is introduced.
5. **Provider and documentation:** At least one adapter fixture checks that the generated schema reaches provider preparation without a silent closed-object conversion. The tools guide and `Func` godoc explain the default, strict option, migration impact, and raw-versus-typed boundary. No live-provider behavior is claimed without a live check.

## Scope and tradeoffs

This changes the default of existing `tool.Func` calls, including tools whose authors implicitly relied on rejection. Release notes should show the strict opt-in. Optional-field typos can now go unnoticed; strict mode is appropriate when an exact field set is part of the tool's meaning. There is no fuzzy renaming, type coercion, JSON repair, automatic retry, provider-wide strictness switch, or change to output schemas.

Two alternatives were considered. Keeping strictness by default and adding a lenient option avoids a behavior change but leaves every ordinary tool exposed to a needless repair step. Stripping extras into a second normalized JSON value creates a new raw/effective identity boundary and can mislead hooks or audit records. Using the generated schema as the single acceptance contract, then ordinary typed decoding, is the smaller design.

## Review questions

- Is `WithStrictArguments()` the clearest small opt-in name, or does the tool package's constructor style call for an options value? This does not change the proposed semantics.
- Do any Dashi-owned local `tool.Func` tools rely on closed keys and need strict opt-in in the same change? Search call sites during implementation; do not alter hand-written or provider-owned tool contracts by default.
