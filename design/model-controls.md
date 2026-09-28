# Model controls

## Why this matters

Dive may clamp reasoning effort, translate it to a token budget, or omit
temperature while the call succeeds. Applications otherwise must copy adapter
rules to validate settings, and some changes surface only in logger warnings.
[PR #273][pr] exposes exact-model facts, an offline request preview, and
effective controls on response usage. It distinguishes native support from
emulation and ties live verification to the API tested.

Dive's preview is separate from its configured provider. For example, its
Anthropic preview assumes `DefaultMaxTokens`, while a provider instance can
have a different default and therefore a different thinking-budget clamp
([review comment][preview-comment]).

## Kernel's approach

- `Client.ModelInfo(model)` returns exact-ID facts supplied to that client
  instance. `ModelEvidence` names the model, API, exact endpoint, owner, source,
  date, and method: live probe, documentation, inference, or fixture.
  Construction rejects mismatched scope; supplied facts are not automatically
  verified.
- `Client.Prepare(ctx, input)` checks the **whole input** and encodes it
  offline. Its snapshot contains requested and effective settings, adjustments,
  model evidence, content changes, and the exact body `Send` will send. Record
  dispatch separately; preparation does not prove provider receipt.
- Work by default ([decision 4](decisions.md)). Under the default
  `CapabilityAdjust` policy, unknown support is sent and recorded as
  unverified, and a value beyond a known limit is clamped to the nearest
  accepted value or, where the model default keeps the meaning, omitted. Each
  change is a machine-readable `Adjustment` and a caller-facing `Warning`.
  `CapabilityStrict` restores fail-closed preparation. Known unsupported
  features with no safe substitute fail under both policies. An effort fits to
  the nearest accepted level but never down to `none`, and the spellings
  `extra-high`, `extra_high`, and `extra high` rank as `xhigh`.
- Keep effective settings with each model attempt. Aggregate token usage cannot
  truthfully represent a run that used different settings. The result records
  the served model when the provider reports it.

This uses one configured client and one preparation path. The
[model catalog](model-catalog.md) supplies default `ModelInfo` through the same
path; it is data, not a second request planner.

## Defaults and evidence

`Adjustment` records a typed field and action plus requested/effective JSON
values. `null` means omitted. Actions are `default` (fills an omission, no
warning), `clamp`, and `omit`; the last two replace caller intent and always
project to a `Warning` on the request, its snapshot, and the result. `Fit`
applies API-wide limits intersected with model evidence before encoding.
Emulation is not implemented. `NewRequest` checks that every setting change has
a matching decision. Snapshots and model facts always record the resolved
policy; a record written before the policy existed decodes as strict, which was
its default then.

`Fit` fills an unset `MaxTokens` with `llm.DefaultMaxTokens` (32,768), lowered
to the model's known output limit, for every provider. Messages also applies
an automatic tool choice when parallel-tool controls require one. Both appear
in effective settings as `default` decisions, which are not warnings. Under
strict policy the output-limit default needs evidence of `max_tokens` support.
Reasoning budgets are checked against the effective output limit, and a budget
beyond it is clamped below it. Temperature with
thinking is omitted with a warning; other conflicting controls are rejected.
Wire-name translations preserve canonical meaning; they are not reasoning
emulation.

Effective settings describe the controls Kernel encodes, not undisclosed server
defaults or proof that the model honored them. Unknown numerical bounds remain
unknown even when feature support is supplied. The built-in catalog is dated
documentation evidence; an application that needs fresher or verified claims
supplies its own entries, which replace the catalog's.

Content adaptation shares this same preparation path and records separate
`ContentChange` decisions. `Input.ContentPolicy` may explicitly permit omission
of unreplayable reasoning; it does not loosen capability checks or change
reasoning settings. See [content replay](provider-content-replay.md).

Local tests compare prepared and dispatched bytes in complete and streaming
calls, including defaults and rejected combinations. Fixture evidence remains
fixture evidence after a successful local call. Live provider qualification is
separate and has not been performed for these changes.

[pr]: https://github.com/deepnoodle-ai/dive/pull/273
[preview-comment]: https://github.com/deepnoodle-ai/dive/pull/273#discussion_r3908307703
