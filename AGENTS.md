# Dashi

Dashi is the foundation of a new Go agent library by Deep Noodle, Inc. It is
effectively Dive v2: the successor foundation to Dive, our existing Go library
for cross-provider LLM support and agents. Its goal is a coherent, reusable
agent foundation for our applications.

## Goals

Dashi should give application developers a better foundation than Dive while
keeping what Dive does well.

- Fix, at the library layer, the shortcomings our production applications
  worked around when building durable agent runtimes on Dive: reconstructing
  execution facts for recovery, accounting, and live presentation; repairing
  transcripts; interpreting errors with several meanings; and inventing billing
  identities.
- Improve the developer experience of the programmatic API relative to Dive,
  with clearer types and concepts centered on the Turn.
- Design terminology, types, and component relationships deliberately, so they
  form one coherent vocabulary before public interfaces are committed.
- Remain excellent at cross-provider LLM support, with thorough coverage of
  provider-specific behavior.
- Remain a leading library for LLM cost tracking and budgeting, and extend it
  from measurement to enforceable budget limits.
- Use newer Go features where they pay off, notably iterators (range-over-func)
  for streaming.
- Borrow successful ideas from adjacent agent frameworks and coding-agent
  projects, and avoid the pitfalls their public issue trackers reveal.
- Adopt good ideas and patterns from open agent protocol specifications,
  including their newer revisions.
- Build live turn updates on the careful design of our existing streaming
  protocol.
- Port useful portions of Dive, adapting them to the new design. Ground
  decisions in evidence and explicit tradeoffs.
- Keep the foundation small, understandable, and idiomatic Go, with clear
  responsibilities and boundaries.

## Context

Project design documents live in `design/`, starting with
`design/functional-areas.md`.

Research is copied into the temporary, git-ignored `tmp/research/` directory:
`design/`, `comparison/`, and `acpresearch/`. The supplied comparison of the two
proposals is `tmp/research/proposal-comparison.md`.

The primary starting point is
`tmp/research/design/2026-09-26-dive-v2-conceptual-model.md`, followed by
`2026-09-26-dive-v2-conceptual-design.md` in the same directory. These are
proposals; unresolved choices remain open.

## Documentation

Keep Markdown documents short and focused: **soft limit two pages; hard limit
four pages**. Prefer concise explanations of concepts, decisions, and tradeoffs.

## Package layout

- A feature belongs in the root package `dashi` only if the Turn engine itself
  must know about it. Everything else is a sub-package that imports `dashi`,
  as `compaction`, `delegate`, `skill`, `filerecorder`, and `mcp` do.
- Generic helpers that touch no public `dashi` type go in `internal/`.
- Root files use a prefix by area so related files sort together: `turn_`
  (Turn, facts, views, replay), `run_` (Runner, model attempt, tool loop),
  `steer_`, `budget_`, `hooks_`, `context_`, `history_`, and `session_`.
  Name a test file after the source file it exercises.

## Modules and releases

Dashi is four Go modules: the root, `mcp`, `providers/google/gemini`, and
`cmd/dashi`. A committed `go.work` ties them together for local work.

- Never add `replace` directives to a `go.mod`. `go install …@version` refuses
  a module that has them, which breaks
  `go install github.com/deepnoodle-ai/dashi/cmd/dashi@latest`.
- `go.work` holds `replace` lines pinned to the current release version;
  `make release-prep` rewrites them. Do not edit them by hand.
- `go mod tidy` ignores the workspace. Avoid it in sub-modules between
  `release-prep` and tagging, and expect it to fail when a sub-module imports a
  root package added since the last release.
- Run `make check` before opening a PR; it covers all four modules.
- Record user-visible changes under `## [Unreleased]` in `CHANGELOG.md`.
- Release steps are in `docs/dev/releasing.md`. Tag every module at the same
  version.
