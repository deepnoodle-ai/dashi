# Structured turn output

**Status:** Implemented, API experimental · **Date:** 2026-09-27

## Decision

An output schema is a contract for a turn's final value. A definition may set a
default; `Start` may override it for one turn. The admitted schema is recorded
with the start fact, so recovery validates against the same contract even if a
later turn requests a different shape. Completion requires one validated
object in `Outcome.Output`, with source and model attempt provenance.

The model sees a reserved `kernel_submit_output` tool whose argument schema is
the output schema. Kernel handles this tool locally. It has no application
executor, approval, or external effect. Its accepted call, validation, and
final output are separate recorded facts. A malformed submission gives the
model bounded feedback and a chance to correct it. A strict JSON final text
answer is also accepted, with text provenance; prose around JSON is rejected.
Refusal, output limit, and uncertain model effects retain their existing stop
semantics. A final response that cannot satisfy the schema fails explicitly.

## Why this boundary

Nvoken and Mobius expose a schema on an individual turn and return a validated
value; Nabaname consumes that value as application data. Dive's lower-level
response format supports provider-specific formatting, but does not itself
define a recoverable agent turn result. Kernel therefore owns the output
contract at the turn level. Providers differ in native JSON-schema modes and
tool behavior; the reserved tool works through the existing common tool-call
path. Text fallback covers a provider that returns valid JSON without calling
the tool. Neither path promises provider-constrained generation: validity is
checked by Kernel before completion.

Object-root JSON Schema keeps the value representable as a function argument
across the current providers. Schemas are compiled without external fetches,
and schema/value size limits bound recorded data and validation. Output remains
in the turn record, rather than being inferred from answer text, so replay and
host accounting see the same accepted result. A future native response-format
route can supply the same `StructuredOutput` fact after validation without
changing the caller contract.
