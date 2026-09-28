# Tolerance of extra tool arguments — draft PRD

| Field | Value |
| --- | --- |
| Status | Draft for discussion, 2026-09-28 |
| Roadmap | [Item 10](../remaining-work.md), size S, difficulty Medium |
| References | [Source survey](10-extra-tool-args-research.md), [adjacent-library findings](../gaps/C-adjacent-libraries.md), [tools guide](../../docs/guides/tools.md) |

## User problem

A model may send a correct local tool call with one surplus key. Today `tool.Func` rejects it before the application function runs, returns `not_executed`, and asks the model to try again. The next attempt spends time, tokens, and one of Dashi's limited model calls; it can fail again for the same harmless mistake. Dive and Pi's ordinary typed-tool path show that a callback can still get only its declared Go fields. A developer who intentionally needs a closed argument contract should retain that choice.

## Goals

- Make `tool.Func[T]` tolerate unknown object keys **by default** at the top level and within typed nested structs, while still validating all declared fields and required values.
- Keep the model-facing schema, runner precheck, and typed decoder consistent; an argument accepted before `BeforeTool` hooks must not be rejected later merely for the same extra key.
- Preserve original JSON in the Turn and in `tool.Call.Args` for hook review, audit, and replay; give the callback `T` containing only declared fields.
- Provide one per-tool strict option for callers who require the current closed-object behavior. Avoid changing hand-written `tool.Definition` or external/hosted tool contracts.

Success: `{"query":"yes","extra":1}` invokes a typed tool once with `Query=="yes"`, using no repair model call. `{"query":3,"extra":1}` and `{"extra":1}` remain `not_executed`; strict mode rejects the first example before any `BeforeTool` hook or executor runs.

## Smallest API direction and user flow

Keep the existing `tool.Func` call shape and add one optional constructor argument, conceptually `tool.WithStrictArguments()`:

```go
lookup, err := tool.Func("lookup", "Find a record", handler)
strictLookup, err := tool.Func("lookup", "Find a record", handler,
    tool.WithStrictArguments())
```

The default generates schemas whose typed struct objects allow unknown properties. Do not introduce an `Extras` map into the callback type or silently add unknown values to a declared map field. The runner validates the **original** JSON against the generated schema before `BeforeTool` hooks; the typed executor validates that same schema and decodes without `DisallowUnknownFields`. Its known fields retain all existing type, required, enum, range, nullability, and nested checks. A strict option generates closed objects and keeps strict decoding, matching today's behavior. Hand-authored schemas retain their exact `additionalProperties` semantics, and tools with no typed `Func` wrapper do not change.

The runner still records and exposes the full original `Call.Args`. `BeforeTool` hooks see those raw arguments and can deny or replace them; any replacement is revalidated under the selected tool schema. The callback receives the decoded `T` plus the original cloned `Call`, so the docs must say explicitly that unknown JSON keys are absent from `T` but may remain in `Call.Args`. A callback that needs to act on dynamic keys should declare a map field or use a custom tool; it should not treat unknown keys in `Call.Args` as validated inputs. No new Turn event, normalization layer, or provider-wide switch is needed.

## Acceptance criteria

1. Default `tool.Func` advertises permissive `additionalProperties` behavior for each typed struct object. Its runner precheck and direct executor agree: top-level and nested surplus keys are ignored by `T`, the callback runs exactly once, and the original JSON stays unchanged in the recorded call and callback's `Call.Args`.
2. Required fields, type/range/enum constraints, malformed JSON, trailing values, wrong tool name, and unsupported null remain rejected before callback execution. Declared map fields preserve their keys and schema constraints. A typo that omits a required field is not healed by ignoring its misspelled key.
3. The strict option reproduces today's closed schema and direct/runner rejection for extra keys. It is per tool, requires no global setting, and remains stable across a Toolset refresh and recorded-definition comparison.
4. A `BeforeTool` hook can inspect raw extras, deny, or rewrite arguments; a rewritten call is revalidated. The Turn and model-visible outcome correctly show either one execution or `not_executed`, with no phantom side effect or hidden extra model attempt.
5. Tests include Dashi's existing unknown-field case updated for the new default, a strict-mode counterpart, nested structs, a required-field typo, a declared map, and a runner flow with a scripted model that emits a surplus key. One provider fixture verifies the advertised schema is transmitted unchanged; live provider acceptance remains a separately labeled qualification.
6. The tools guide and `Func` godoc explain the default, strict option, raw-versus-typed difference, and migration impact in a short example.

## Non-goals

No fuzzy renaming, coercion of wrong types, arbitrary JSON repair, automatic retry, new validation engine, or change to provider-hosted/MCP/external tools. This PRD does not loosen output schemas or authorize unknown keys to affect a typed callback's behavior.

## Risks and open decisions

- **Compatibility:** Existing applications may rely on extra keys being rejected, especially for sensitive tools. The strict option preserves that path; release notes must call out the changed default and show how to opt in. Decide whether any built-in Dashi local tool should select strict mode explicitly.
- **Provider constraints:** Some provider-side strict sampling requires closed schemas. Dashi's current OpenAI Responses adapter sends `strict:false`; if a later adapter forces strict sampling, it must negotiate that separately rather than silently change `tool.Func` runtime semantics.
- **Schema generator:** Verify how `wonton/schema.Generate` represents open nested structs and whether `additionalProperties` must be postprocessed. The emitted schema, runner, and decoder must pass the same cases before publishing the option.
- **Visibility:** Raw extras remain auditable, but a user-facing view need not list ignored keys by default. If real support cases show confusion, add a small diagnostic on the existing call record rather than a new event type.
