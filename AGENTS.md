# Kernel

Kernel is the foundation of a new Go agent library by Deep Noodle, Inc. Its goal
is a coherent, reusable agent foundation for our applications.

## Goals

- Design terminology, concepts, and component relationships deliberately before
  committing to public interfaces.
- Learn from Dive, other agent libraries, and our applications' integration
  experience. Ground decisions in evidence and explicit tradeoffs.
- Port useful portions of Dive, adapting them to the new design.
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
