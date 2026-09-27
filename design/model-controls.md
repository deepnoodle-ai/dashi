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
  date, and method: live probe, documentation, inference, or fixture. Construction
  rejects mismatched scope; supplied facts are not automatically verified.
- `Client.Prepare(ctx, input)` checks the **whole input** and encodes it
  offline. Its snapshot contains requested and effective settings, adjustments,
  model evidence, content changes, and the exact body `Send` will send. Record
  dispatch separately; preparation does not prove provider receipt.
- Reject known unsupported settings and unknown capabilities by default.
  `UnknownAllow` is explicit and records the policy and unverified features,
  including output-limit support. If Kernel later
  clamps, emulates, or omits a control, that behavior needs explicit policy and
  a machine-readable decision.
- Keep effective settings with each model attempt. Aggregate token usage cannot
  truthfully represent a run that used different settings. The result records
  the served model when the provider reports it.

This uses one configured client and one preparation path. An enumerable
catalog can be added when needed without becoming a second request planner.

## Defaults and evidence

`Adjustment` records a typed field and action plus requested/effective JSON
values. `null` means omitted. Only filling an omitted setting is implemented;
an explicit value cannot be replaced or dropped. `NewRequest` checks that
every setting change has a matching decision.

Messages applies its configured `DefaultMaxTokens` (1024 when unset) and an
automatic tool choice when parallel-tool controls require one. Both appear in
effective settings and decisions. Reasoning budgets are checked against that
same output limit. Conflicting controls are rejected. Wire-name translations
preserve canonical meaning; they are not reasoning emulation.

Effective settings describe the controls Kernel encodes, not undisclosed server
defaults or proof that the model honored them. Unknown numerical bounds remain
unknown even when feature support is supplied. Evidence freshness and catalog
maintenance remain application responsibilities.

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
