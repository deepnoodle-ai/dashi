# Draft PRD: optional skill catalog and loader

**Roadmap item 12 · Draft · 2026-09-28.** Grounded in [comparative research](12-skills-research.md). No approved implementation design yet.

## User problem

An application that already has named task instructions must currently hand-build a model-visible catalog, a load tool, and the mapping between them. Mobius Cloud does this for assigned org skills today. Repeating that glue in each Dashi host invites catalog/body mismatch and weak errors. Developers need a small way to let an agent discover **which approved skills are available this turn**, then load one into its recorded conversation. They should still own where skills come from and which tools an agent may run.

## Goals and non-goals

- **Goal:** A portable, explicit catalog/load flow for host-selected skills, with stable per-turn contents and a normal recorded tool result.
- **Goal:** Predictable setup, validation, duplicate handling, and unknown-skill errors. No catalog/tool is offered for an empty set.
- **Goal:** Keep skill content at tool-result authority; a skill cannot grant tools, change policy, or impersonate operator instructions.
- **Non-goal:** Org storage/assignment, marketplace install/update, shell or script execution, skill-driven MCP registration, automatic keyword triggers, slash commands, child turns, or action-scope enforcement. Those are host features or separate roadmap items.

## Smallest cohesive API direction

Add an **optional** `skill` package, outside Dashi's turn core. Its first consumer should be a Mobius-style host adapter. The host passes already-authorized definitions; the package validates and freezes their name, description, and body into an immutable bundle. It returns (1) catalog text to compose into the host's `Agent.Instructions`, (2) a fixed read-only `load_skill` tool, and (3) a digest the host can include in `Agent.Revision`. Exact names remain a design choice; the intended use is:

```go
bundle, err := skill.New([]skill.Definition{
    {Name: "triage", Description: "Investigate a customer incident", Instructions: triageText},
})
if err != nil { return err }

runner := dashi.Runner{
    Agent: dashi.Agent{
        Revision:     agentRevision + ":" + bundle.Digest(),
        Instructions: baseInstructions + "\n\n" + bundle.Catalog(),
    },
    Tools:  tool.List{bundle.LoadTool(), existingTool},
    Hooks:  hostHooks,
}
```

The example shows composition, not a promise of these exact method signatures. `load_skill(name)` returns the pinned body, identifies the loaded skill, and gives a useful error for unknown or unavailable names. The tool does no filesystem I/O or command execution. A host may omit `LoadTool` and show no catalog when its selected set is empty. Host code still filters skill assignments before `New` and enforces any action restrictions through `Runner.Hooks.BeforeTool` or its tool dispatch. If explicit user invocation is desired, the host can call a read-only `bundle.Lookup(name)` and deliver that content with an appropriate recorded origin; Dashi need not parse `/skill:`.

**Trust and lifecycle:** Only the host chooses whether the **catalog** is operator-approved material suitable for `Agent.Instructions`. The loaded **body** is a tool result and cannot elevate itself. The bundle is frozen before `Start`; resumed turns use the same revision and body snapshot. If the host cannot retrieve that snapshot on Continue, it fails visibly rather than silently substituting changed instructions. Never place credentials in a catalog or skill body. The package must not interpret prose such as `allowed-tools` as an authorization grant.

## Acceptance criteria

1. A host can construct the bundle from in-memory definitions without adopting filesystem layout or storage interfaces. Names and descriptions validate before a turn; duplicate names fail with a source-independent diagnostic. Catalog order is deterministic.
2. The catalog exposes only name and concise description, plus an instruction to call the bundled loader. Full instructions appear only after an explicit load call, as a recorded tool result. Unknown names return a clear, bounded error and available names without leaking bodies.
3. Empty bundles add neither catalog nor tool. The host can compose the loader with existing tools without replacing its `Toolset` or `Hooks`.
4. A skill body containing a command, relative path, prompt-injection text, or `allowed-tools` metadata cannot itself execute code, read files, register tools, or change permissions.
5. A stopped and continued turn receives the same skill content when the host uses the pinned bundle. Changing the bundle changes its digest and the host's explicit agent revision; a mismatched revision is surfaced by existing Dashi continuation checks.
6. One realistic Mobius-style example loads assigned skills, advertises them, and keeps its action constraints in host code. It demonstrates correct results and errors without porting Mobius's database layer.

## Risks and open decisions

- **Consumer gate:** Implement after validating the proposed composition against Mobius's current `invoke_skill` and assignment flow. That app still runs Dive; the PRD alone is not evidence of a Dashi integration request.
- **Filesystem format:** An explicit `SKILL.md` reader may later be useful for Dive/pi compatibility. Defer it until a Dashi consumer needs it; automatic home/project scanning brings precedence and trust questions. If added, loading must reject path escape/symlink escape and cap file sizes.
- **References:** First slice returns only the body. Relative references require either a host file tool or a later scoped `read_skill_resource` tool. Avoid displaying unusable file paths in catalog; test with a real skill before adding resources.
- **Catalog authority:** A host-managed catalog may be operator text, while user/project/marketplace skill metadata may be less trusted. Keep origin choice explicit in examples and never let the package mutate the system prompt behind the host's back.
- **Revision ergonomics:** Manual digest composition is correct but may be awkward. Confirm whether a helper can pin tool-backed behavior without broadening the core `Agent` API.
