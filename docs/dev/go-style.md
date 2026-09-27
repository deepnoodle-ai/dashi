# Go Style Guide

How Kernel code should read. Baseline: `gofmt`, `go vet`, and
[Effective Go](https://go.dev/doc/effective_go). This guide covers what those
don't enforce. `content/` is the reference example.

Apply these rules to new and changed code, including tests, even when nearby
code uses an older layout.

## Formatting and layout

- Run `gofmt` (or `goimports`). Formatting-clean is the floor, not the goal.
- Separate top-level declarations with a blank line. Don't pack type
  definitions together.
- Write functions on multiple lines, even short constructors. Keep sibling
  functions in the same shape.
- Wrap comments at about 80 columns. Keep code under 80 columns when it can be
  broken cleanly; long wire fixture strings and URLs may stay intact.
- Keep a composite literal on one line only when the whole line is under 80
  columns. Otherwise put the opening `{` after the type, one field, map entry,
  or slice element per line, and the closing delimiter after the last element
  on a separate line. Apply this rule to nested literals too; a short inner
  literal may stay on one line.
  Never start a multiline literal with a field or pack fields onto continuation
  lines.
- Keep a call on one line when it fits. If a call with multiple arguments wraps,
  start each argument on its own line and close after the last one. A sole
  multiline literal argument can stay attached to the call, as below.
- Order a file as: package doc, imports, types with their constructors and
  methods, then unexported helpers at the bottom.

```go
// The whole line is short enough.
ref := llm.ModelRef{Provider: "grok", Model: model}

// The full literal would be too long, so each field gets a line.
client, err := responses.New(responses.Config{
	Provider:            "grok",
	APIKey:              config.APIKey,
	Endpoint:            config.Endpoint,
	Client:              config.Client,
	Models:              config.Models,
	UnknownCapabilities: config.UnknownCapabilities,
})
```

## Comments

A comment should tell the reader something the code can't: why it exists, what
goal it serves, what it guarantees, or what would break if it changed. If a
comment only repeats the identifier or the code, delete it or rewrite it.

```go
// Bad: restates the name.
// Image is an image payload.

// Good: states the rule and why it exists.
// Raw may only be replayed to Provider; a URL alone does not replace that data.

// Good: states a boundary callers rely on.
// Source ... never fetches external data.
```

- Every exported identifier gets a doc comment that starts with its name. Use
  it to explain purpose, invariants, ownership, and intended use, not type
  names or field lists.
- Explain design decisions where they live. If a choice was deliberate (a
  tagged union instead of an interface, bytes copied instead of shared), say
  why in a sentence, so the next person doesn't "fix" it.
- Name the constraint behind code that looks odd: a provider requirement, a
  compatibility promise, or a bug it works around.
- Put the package comment on one file only. Explain what the package is for,
  what it deliberately does not depend on, and where adapted code came from.
- Inside function bodies, comment only on non-obvious choices. Don't narrate
  steps.
- Never make up a rationale. If the reason isn't known, find it in `design/`
  or ask rather than guessing.

## Errors

- Prefix messages with the package name: `content: source requires ...`.
- Use `errors.New` for fixed messages. Use `fmt.Errorf` only when you format a
  value, and use `%w` when wrapping.
- Lowercase, no trailing punctuation, and state what was required rather than
  what went wrong in general.
- Declare sentinel errors (`var ErrX = errors.New(...)`) only when callers need
  to branch on them.

## Control flow and structure

- Remove repetition with a small unexported helper (for example,
  `count(...bool)`) instead of copying the same `if` blocks.
- Prefer a flat `switch` where each case returns and a single fallthrough error
  sits at the end. Fold conditions into the case expression rather than nesting
  `if`s inside cases.
- Use the standard library (`slices`, `maps`, `errors`) before writing loops by
  hand.
- Value receivers for small, immutable-by-convention types. Methods that
  "modify" a copy (such as `Clone`) can use the value receiver as scratch space.

## Data types

- Tagged unions: a `Kind` string type, typed constants, one pointer field per
  payload, and a `Validate` method that enforces "exactly one, matching Kind".
- JSON tags use `snake_case` with `omitempty` on optional fields.
- Use initialisms consistently: `URL`, `ID`, `URI`, `MIMEType`, `JSON`.
- Give enum constants consistent family prefixes: `RoleUser`, `ContentBlock`,
  `StopFinished`, `DeltaText`, `KindText`, and `StateSucceeded`. Use typed
  constants instead of string literals for the library's closed vocabularies.
- Constructors (`TextBlock`, `ImageBlock`) cover the common cases; struct
  literals remain valid for everything else.
- Document aliasing. If a value holds slices or maps, provide `Clone` and
  deep-copy byte and raw-JSON fields.

## Tests

- Tests live in the same package (`package content`) unless you are
  specifically testing the public API.
- One behavior per test function, named as type plus behavior in plain words:
  `TestBlockCloneCopiesCitations`.
- Use table-driven tests with a `name` field and `t.Run` for sets of similar
  cases, so failures identify the case.
- Standard `testing` only. Use `t.Fatal` / `t.Fatalf` with messages that
  describe the broken property ("image bytes were shared").
- Test invariants and ownership, not just the happy path.
- Split long test cases into one field per line. Avoid long `||` chains that
  combine unrelated expectations; use separate checks so a failure identifies
  the broken property. Keep large wire fixtures in `testdata/` when inline JSON
  obscures the test.

## Before you finish

```sh
test -z "$(gofmt -l .)" && go vet ./... && go test ./... && go build ./...
```

Review multiline literals, calls, and test failures by eye; `gofmt` does not
enforce these layout rules. Keep public APIs and behavior unchanged during
style-only changes. Say explicitly when a change goes beyond formatting.
