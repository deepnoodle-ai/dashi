# PRD: optional skill catalog and loader

**Roadmap item 12 · First slice merged in PR #59 · 2026-09-28.** Based on [source research](12-skills-research.md). Further expansion needs application-consumer qualification.

## User problem

An app with approved task instructions must hand-build a short catalog, a matching load tool, and recovery rules around both. Mobius Cloud already does this for skills assigned to an org's agent; its current Dive integration maintains a separate mutable tracker and suspension checkpoint for action focus. A Dashi developer should be able to give the model a clear list of the skills available **this turn**, let it load one body, and understand what was recorded. App owners must keep control of selection, trust, and executable actions.

## Goals and non-goals

- **Goals:** One optional in-memory catalog/load component; progressive disclosure; model-facing errors that help recovery; deterministic ordering and versioning; a loaded body recorded as an ordinary tool result. Mobius's assigned-skill flow must map without forcing its DB, assignment, or policy model into Dashi.
- **Non-goals:** Automatic project/home scanning, AgentSkills file parsing, resource reads, shell or script execution, marketplace management, slash commands, keyword triggers, action authorization, child turns, and core Turn changes.
- **Success evidence:** A compile-tested Mobius-style fixture can convert assigned skills, advertise a catalog, load a body, and keep action focus in host code. A local example is an integration proof of the API shape, not production migration proof.

## Proposed smallest API and flow

Add an optional skill package. The host supplies a filtered, immutable list of definitions with display name, optional description, and required instructions. Construction trims names, checks the raw description byte limit **before** folding its whitespace to one line, rejects blank names/bodies and remaining control characters in catalog metadata, and uses strings.EqualFold for both duplicate detection and lookup; it preserves readable names with spaces. Limits are 256 skills, 80 bytes/name (Mobius's limit), 1,024 raw bytes/description (a conservative byte cap), 64 KiB/body, and 64 KiB/rendered catalog; construction reports the offending field and limit. Mobius's checked-in document playbooks are below 6.4 KiB, but stored assigned-skill sizes remain a migration check. A stable sort makes the catalog and digest independent of input order. The bundle exposes Catalog() and Digest() and implements tool.Toolset with one read-only named loader, or no tool for an empty bundle. The default tool name is load_skill; an explicit option permits Mobius's invoke_skill name. The implemented API uses these names:

```go
bundle, err := skill.New(defs, skill.Options{ToolName: "invoke_skill"})
if err != nil { return err }

runner := dashi.Runner{
    Agent: dashi.Agent{
        Revision:     appRevision + ":" + bundle.Digest() + ":" + focusRevision,
        Instructions: baseInstructions + "\n\n" + bundle.Catalog(),
    },
    Tools: tool.Join(tool.List{existingTool}, bundle),
    Hooks: hostHooks,
}
```

The host explicitly chooses to place the catalog in Agent.Instructions. That is appropriate only when it treats the selected catalog metadata as operator-approved; lower-trust catalogs should be delivered as lower-trust ContextRuntime/user-role content instead. The package neither injects prompts through hooks nor raises a skill body to operator authority. The load tool accepts a name, matches case-insensitively, and returns the pinned body under a clear heading as tool output. Unknown names produce a bounded model-facing error and available names; no body leaks. The tool performs no I/O and no command execution. Empty input yields an empty catalog and no loader tool.

Mobius keeps assignment and org checks before construction, plus its required/recommended-action checks, focus policy, and focus checkpoint in application code. A host that enforces focus may append a human-readable scope notice to the body it supplies. The package does not derive focus from Turn facts: BeforeTool may authorize a later call in the same batch before the load settles, even with serial execution. Skill prose, including allowed-tools or allowed_actions, never grants a tool. The host binds tools and makes authorization decisions independently.

## Acceptance criteria

1. The package works from in-memory definitions with no filesystem or storage dependency. It accepts Mobius-valid display names and empty descriptions, rejects collisions after trim/case-fold, and produces deterministic catalog/digest output across input order. Errors identify the offending name without exposing instructions.
2. The catalog contains only names, optional concise descriptions, and the correct loader name; the body appears only after load. A successful load is a settled, recorded tool result. Unknown names yield a useful model-facing failure. The host can omit an unavailable skill before bundle creation; the package does not infer action availability.
3. An empty bundle advertises nothing and contributes no tool. The loader composes with an existing Toolset through tool.Join and leaves Hooks alone. A loader-name collision with host tools returns tool.Join's configuration error before model dispatch; neither tool is silently renamed.
4. A body containing shell syntax, relative paths, prompt-injection text, or permissions metadata cannot by itself execute a command, read a file, register a tool, or change authorization. The load result stays at tool-result authority. Catalog placement is a visible host decision.
5. Digest input includes canonical name, description, body, and loader name; body changes change the digest. The Mobius-style example sets an explicit Agent.Revision containing that digest and a separate host focus-policy revision. Tests show a changed explicit revision is rejected on Continue and a zero Agent does not verify a separately supplied tool bundle. The host must rebind the **same immutable bundle snapshot** when resuming a real turn.
6. The consumer fixture exercises case-insensitive lookup, blank description, unknown name, and required-action exclusion while retaining focus selectors in host data. Tests establish the same-batch authorization boundary. A documented migration gate covers persisted selector recovery and changed focus policy on Continue; neither is guaranteed by the package.

## Risks and open decisions

- **Recovery ownership:** Dashi records the load output, but it does not persist the host's bundle source. Mobius must retain an immutable skill snapshot or versioned lookup for continued turns and fail the host migration visibly when that snapshot is missing. A fresh bundle plus a zero Runner.Agent can bypass revision comparison; document this prominently.
- **Mobius behavior change:** Mobius currently accepts skill ID as a loader alias and lists skills that later fail required-action checks. The minimal package matches names only and lets the host exclude unavailable skills. Review actual ID-call usage and desired unavailability UX before migration; add aliases or unavailable-reason metadata only if usage evidence requires them.
- **Focus migration:** Mobius's checkpoint includes action selectors, which are outside the bundle digest. The host must own their version, recovery, same-batch ordering, and dispatch policy. Changing focus policy during Continue is a separate host migration gate. Focus is not security containment.
- **Size and references:** Validate stored Mobius assigned-skill sizes against the proposed construction limits before migration; checked-in document playbooks are only a sample. Dashi still has an open tool-result projection item. Relative references remain plain text until a host provides a file tool or a later scoped resource feature.
