# PRD: nullable typed-tool arguments

| Field | Value |
| --- | --- |
| Status | Draft for discussion |
| Date | 2026-09-28 |
| Roadmap | [Item 9](../remaining-work.md), size S / low difficulty |
| Evidence | [Source review](09-nullable-tool-args-research.md), [example-port follow-up](../gaps/F-example-ports.md) |

## User problem

A developer defines `tool.Func` with a `*string` or `*Nested` argument so the model can provide a value or null. Dashi advertises that field as `nullable`, yet its validator rejects explicit null before the callback runs. The error appears as a tool that was not executed, even though the Go type could represent the value. That makes a simple typed tool surprising to use and can waste a model turn.

## Goal and scope

Keep the existing `tool.Func[T]` API. Make the **one generated schema** used for model presentation and local validation express pointer nullability in standard JSON Schema draft 2020-12 form, and make decoding agree with it. Preserve the separate meaning of omission: `json:",omitempty"` controls whether a key is required; pointer type controls whether a present key may be null. Keep invalid arguments from reaching the callback. This is a small construction/schema correction, not a new optional-value abstraction or a provider-specific schema rewrite.

## Proposed behavior

For a field `Value *string` tagged `json:"value,omitempty"`, emit `{"type":["string","null"]}` and leave `value` out of `required`. For `Value *string` tagged `json:"value"`, emit the same property schema **and include** `value` in `required`. In both cases, present null validates and decodes to nil; omitted optional also decodes to nil, while omitted required is rejected. The callback's `tool.Call.Args` still contains the exact JSON, so an application that cares can distinguish omitted from explicit null without another public type.

Normalize the generated `wonton` schema at `tool.Func` construction before storing it in `Definition` and compiling it. Replace pointer-derived `nullable:true` with a `type` union at each nested property/item; remove the obsolete keyword from the published bytes. Respect an explicit `nullable:"false"` tag as a narrower contract. When a nullable pointer also has an enum, include `null` in the enum so the type union actually permits it. Preserve all other constraints on the non-null arm, and fail construction with a useful field path if a tag claims null for a Go field that cannot represent it. Do not transform caller-supplied raw `tool.Definition.Schema` values.

```go
type SearchArgs struct {
    Query  string  `json:"query"`
    Region *string `json:"region,omitempty"`
}

search, err := tool.Func("search", "Search documents", searchFn)
// {"query":"term"} and {"query":"term","region":null} both reach searchFn.
// {"query":"term","region":7} is rejected before searchFn.
```

## Acceptance criteria

1. Construction emits a draft 2020-12-compatible `type` union for nullable pointer fields at top level and inside nested structs, slices and maps; published and compiled schema bytes are identical. `nullable` is absent from the emitted schema. An explicit `nullable:"false"` still rejects null.
2. Table tests exercise omitted optional, explicit null, valid value and wrong type for pointer-to-string, pointer-to-object, and pointer-to-array/map. The callback runs only for valid cases and receives the expected nil/non-nil Go value.
3. A required pointer property accepts present null but rejects omission. An optional non-pointer field continues to reject explicit null unless its Go type and declared schema can represent null. No generic null-to-omission coercion is introduced.
4. Enum, format and nested constraints continue to apply to non-null values; null is accepted for a nullable pointer with an enum. A contradictory `nullable` tag fails at construction with a path to the field.
5. Existing `tool.Func` argument rejection, unknown-key behavior, and root object requirement remain covered. Provider adapter fixture checks verify the emitted schema is transported unchanged or rejected explicitly, without claiming current live provider acceptance.

## Non-goals, risks and open decisions

- **Provider strictness:** [OpenAI strict mode](https://developers.openai.com/api/docs/guides/structured-outputs) requires every property in `required`; ordinary Dashi `omitempty` semantics remain optional. Do not silently rewrite typed-tool arguments for one provider in this item. Route-specific adaptation and live checks belong to roadmap [items 4–5](../remaining-work.md).
- **Omission versus null:** Go pointers collapse both to nil. This is acceptable for `tool.Func`; callers can inspect `Call.Args` if they need the original distinction. A tri-state generic wrapper is out of scope.
- **Generator boundary:** `wonton` uses a single-string `Type` plus a `Nullable` extension. A local normalization is the smallest Dashi change. If a future `wonton` release emits standard unions directly, remove the normalization after contract tests prove byte-level behavior. The choice should not create two competing schemas.
- **Null on other nilable Go types:** Slices, maps, interfaces and `json.RawMessage` can encode/decode null, but this item is about pointer-derived nullability. Follow-on changes require the same emitted-schema/validator/decoder test before claiming support.
