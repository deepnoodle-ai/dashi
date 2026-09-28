# PRD: nullable typed-tool arguments

| Field | Value |
| --- | --- |
| Status | Draft for independent review |
| Date | 2026-09-28 |
| Roadmap | [Item 9](../remaining-work.md), size S / low difficulty |
| Evidence | [Source review](09-nullable-tool-args-research.md), [example-port follow-up](../gaps/F-example-ports.md) |

## User problem

A Go developer uses `*string` or `*Nested` in `tool.Func` to accept a value or null. Today Dashi advertises the field as `nullable`, then rejects explicit null before the callback runs. The model sees a tool failure for an argument the Go type could represent. This makes the generated contract misleading and can waste a model turn. The current tests pass without exercising pointer-property null; the mismatch is established by the generator and validator paths in the [source review](09-nullable-tool-args-research.md).

## Goal and scope

Keep the existing `tool.Func[T]` API. Make its **one generated schema** express pointer nullability in standard JSON Schema draft 2020-12 form, and make schema validation and Go decoding agree. Preserve the separate meaning of omission: `json:",omitempty"` (or `required` tag) controls whether a key must appear; pointer type controls whether a present key may be null. Invalid arguments still never reach the callback. Success means a developer can predict behavior from a Go field and its tags, without learning a Dashi-specific null rule.

## Proposed behavior

For a field `Value *string` tagged `json:"value,omitempty"`, emit `{"type":["string","null"]}` and leave `value` out of `required`. For `Value *string` tagged `json:"value"`, emit the same property schema **and include** `value` in `required`. A present null validates and decodes to nil in either case; an omitted required key fails. The callback's `tool.Call.Args` retains the JSON, so an application that needs the omitted/null distinction can inspect it without a new public type.

Convert the generated `wonton` `nullable` extension at `tool.Func` construction, before publishing and compiling the schema. A nullable pointer property, array item, or map value with a base `type` gets a union with `null`; remove the `nullable` keyword from emitted JSON. A pointer to `any` or `json.RawMessage` has **no base type** in Wonton's schema, which already accepts null, so leave it unconstrained. An explicit `nullable:"false"` is a narrower contract where a base type exists; on an unconstrained field it must fail construction with the field path rather than silently accept null. If an `enum` exists, include `null` when the field is nullable, or the enum would still reject it. Non-null values retain their existing format and other constraints. For this slice, reject `nullable:"true"` on **every non-pointer Go field**, including nilable slices, maps, interfaces, and `json.RawMessage`, with a field path. Do not transform caller-supplied raw `tool.Definition.Schema` values.

```go
type SearchArgs struct {
    Query  string  `json:"query"`
    Region *string `json:"region,omitempty"`
}

search, err := tool.Func("search", "Search documents", searchFn)
// {"query":"term"} and {"query":"term","region":null} both reach searchFn.
// {"query":"term","region":7} is rejected before searchFn.
```

`tool.Func[*SearchArgs]` still requires a root JSON object: a root `null` is rejected. An omitted optional pointer and a present null both reach the callback as nil, but the original `Call.Args` distinguishes them.

## Acceptance criteria

1. Construction emits a draft 2020-12-compatible `type` union for nullable pointer fields inside top-level and nested structs, slices, and maps; the published schema is the one compiled for validation. `nullable` is absent from emitted JSON. An explicit `nullable:"false"` still rejects null.
2. Table tests exercise omitted optional, explicit null, valid value and wrong type for pointer-to-string, pointer-to-object, pointer-to-array/map, `*any`, and `*json.RawMessage`. The callback runs only for valid cases and receives the expected nil/non-nil Go value. Untyped pointer schemas accept null naturally without adding an invalid `type` union.
3. A required pointer property accepts present null but rejects omission. Optionality tags change only presence. A root pointer-to-struct still rejects root null. An optional non-pointer scalar still rejects explicit null. No generic null-to-omission coercion is introduced.
4. Enum, format, and nested constraints continue to apply to non-null values; null is accepted for a nullable pointer with an enum. Explicit `nullable:"true"` on any non-pointer field and `nullable:"false"` on an unconstrained field fail construction with field paths. Malformed tags and tags on flattened embedded fields fail rather than being silently ignored by Wonton.
5. Existing wrong-type, unknown-key, and trailing-JSON rejection remains covered. At least one provider adapter fixture proves that the published schema is forwarded unchanged or explicitly rejected; no live provider acceptance is claimed. `docs/guides/tools.md` describes the new pointer-null behavior and tag limits, replacing its current statement that pointer null is refused.

## Non-goals, risks and open decisions

- **Provider strictness:** Dashi's OpenAI Responses adapter currently sends function tools with `strict:false` and forwards the schema. [OpenAI strict mode](https://developers.openai.com/api/docs/guides/structured-outputs) requires every property in `required`; ordinary Dashi `omitempty` semantics remain optional. Route-specific adaptation and live checks belong to roadmap [items 4–5](../remaining-work.md).
- **Omission versus null:** Go pointers collapse both to nil. This is acceptable for `tool.Func`; callers can inspect `Call.Args` if they need the original distinction. A tri-state generic wrapper is out of scope.
- **Generator boundary:** `wonton` uses a single-string `Type` plus a `Nullable` extension. A local normalization is the smallest Dashi change. If a future `wonton` release emits standard unions directly, remove the normalization after contract tests prove behavior. Keep one schema for publication and validation.
- **Other nilable Go types:** Slices, maps, interfaces, and `json.RawMessage` can hold null, but this item promises pointer-derived nullability. Explicit `nullable:"true"` on them is rejected for this focused slice; a future extension needs matching generation, validation, and decode tests.
