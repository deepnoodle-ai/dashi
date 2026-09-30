# CLI instructions, skills, and resource recovery

Status: Accepted · Author: Codex · Updated: 2026-09-30\
Workflow: Spec → root checkpoint → build, after the named [PRD](../prds/cli-resources.md) review.

## Context and boundaries

The CLI needs instruction selection, skill dispatch, source inspection, and approved supporting-file reads. `skill` already owns parsing, precedence, invocation restrictions, expansion, and catalog delivery. `SeedContext` owns visible-value deduplication. Turn records own execution and recovery. `Agent.Revision` does not verify rebound tool implementations.

Keep a flat CLI adapter. Do not introduce a resource framework, new execution store, public Agent field, or workspace read/write-root option.

| Location | Responsibility |
| --- | --- |
| `skill/load.go`, `skill/skill.go` | Small metadata/definition accessors; preserve parser and catalog semantics. |
| `cmd/dashi/resources.go` | Discovery, inventory, frozen selection, contextual delivery, and `skillWiring`. |
| `cmd/dashi/skill_files.go` | Selected-root validation and `read_skill_file`; no general filesystem API. |
| `cmd/dashi/session/resources.go` | Immutable resource bytes in the existing session directory. |
| `app.go`, `wire.go`, `engine.go`, `control.go` | Scheduled composition, turn binding, `/resources`, and slash dispatch. |

Dependencies flow from CLI adapters to `skill`, `workspace`, and Dashi contracts. Those packages never import the CLI. Use concrete structs and functions; no new plugin interfaces.

## Discovery and inventory

Load AGENTS.md, otherwise CLAUDE.md, from the selected canonical workspace root. Read at most 64 KiB plus one byte. Reject invalid UTF-8, excess size, and links outside that root. Only absence permits fallback. Return an instruction ContextItem with `Origin: ContextRuntime`, `Name: "workspace-instructions"`, and `Snapshot: true`. Its contextual reminder includes the selected source and body. An explicit “no workspace instructions selected” value clears a prior selection without rewriting history.

Discover instructions and call `LoadDirs(DefaultDirs(workspace, home)...)` at startup and before admitting each new user prompt. Rediscover before expanding an idle user slash skill; use that same selection for expansion and turn admission. A queued successor rediscovers at its own admission, after its predecessor ends. Freeze the resulting selection for that turn. Keep the existing skill file/body/catalog bounds and invocation restrictions.

Add `Info.Path` for the defining file, `Set.Candidates()` for cloned selected/shadowed metadata in discovery order, and `Bundle.Definitions()` for cloned model-eligible definitions. Capture candidates during each load, including within-directory conflicts; do not parse files twice per discovery. Source tiers come from the known search directory, not frontmatter. Canonical folder targets define support scope; the inventory shows both the source path and resolved target when different.

Discovery filesystem errors produce path-specific warnings and omit failed content from the new selection. An instruction read error selects explicit empty instructions; only absence permits CLAUDE.md fallback. Skill file/directory errors retain `LoadDirs` partial-success behavior and precedence among valid candidates. Never reuse stale bytes after a failed refresh. Continue with valid resources, including an empty catalog; do not read supporting files during discovery.

`/resources` rediscovers inventory metadata and renders instructions, skill sources/restrictions/argument hints, shadowed candidates, warnings, and actual registered tool sources/scopes. During an open turn, label its frozen selection separately from the discovered selection pending the next turn. Rediscovery may read instruction and defining skill files, but delivers no new model context or replacement pin. Admission rediscovers again; inspection does not commit a future selection. Keep bodies and credentials out. A startup summary names the chosen instruction and source counts. Loading is explicit, empty is final, and warnings preserve partial success. Existing printable Blocks provide screen/line presentation; print notices use stderr. MCP supplies its own inventory facts when wired.

Built-ins take precedence over custom commands. Idle slash dispatch calls `Set.Expand` on the refreshed selection and submits the resulting user text through the existing prompt path. Unknown and restricted names get distinct notices. Composer consumes user-eligible metadata and argument hints. During a running turn, a skill command requests retry when idle. No implicit steer, queue, or shell expansion occurs.

## Minimal recovery pin

Store one versioned immutable JSON snapshot of the selected instruction item, model-eligible `skill.Definition` values, and display/support metadata. User-only bodies need no extra snapshot: slash expansion is already recorded as turn input. Record no approval rules. Serialize deterministically; reject snapshots above 32 MiB without truncation. Rebuild with `skill.New` and validate its digest against the snapshot's catalog digest.

Store bytes as `<session>/resources/<sha256>.json`, with directories `0700` and files `0600`. Write and fsync a temporary file, publish atomically, and fsync the directory before committing the reference. Verify existing bytes before reuse. Keep snapshots for the session's lifetime. They contain resource bytes, never turn status, effect claims, or a mutable “current” pointer.

Before each turn's first model preparation, a named BeforeModel hook commits one contextual ContextItem containing only `{version, sha256, size, catalog_digest}`. Use `Name: "cli-resource-pin"` and a turn-specific delivery ID. This small per-turn item identifies recovery inputs; it exposes no skill bodies. Record it before model intent or tool dispatch. Then join the selected instruction's `SeedContext` hook and the rebuilt bundle's hooks. Handle an empty catalog with a contextual snapshot named `skills` stating that no model skills are available, so an old catalog cannot remain current.

On continuation, read the pin from that Turn's recorded ContextItems, verify digest/size/version, and rebuild the selected bundle and instruction hook. Never select by latest local file contents or a session metadata pointer. A crashed turn with only Start and no pin may rediscover current resources only after proving no model/tool intent exists. Otherwise a missing, unreadable, or corrupt pin/snapshot stops before dispatch. Snapshot publication failure also stops before dispatch; never fall back to disk or an older snapshot. Do not modify Agent revision or historical messages.

Use stable CLI forwarding tools named `load_skill` and `read_skill_file`. Their schemas do not depend on the selected catalog. BeforeModel and BeforeTool select the bundle from the current turn's pin, including queued successor turns. The loader delegates to that bundle; an empty bundle returns an unavailable-skill outcome. A host-owned loader can be explicitly trusted after duplicate-name rejection. Supporting-file calls remain subject to approval. A resource failure returns an explicit BeforeModel Stop or BeforeTool Deny, never a hook error that the runner could skip.

Engine.Open prebinds recorded resources before continuing a pending tool call. New turns use their admission selection; open turns retain their pin. Reopened unchanged instruction/catalog snapshots follow #117's presence rule. The per-turn pin remains a small additional audit message. `/resources` exposes disk changes as pending next-turn resources without changing the open loader or supporting-file scope.

## Supporting-file contract and approval

```go
type skillFileArgs struct {
    Skill string `json:"skill"` // selected folder skill, case-insensitive
    Path  string `json:"path"`  // relative to that skill's base directory
}
```

`read_skill_file` returns bounded UTF-8 text for a regular file, with the selected skill and canonical path. Its description directs the model to use this tool for supporting files outside the workspace. Ordinary workspace reads keep `read_file`. Return path-specific failed outcomes for filesystem read errors, missing/nonregular/invalid files, and reads over 64 KiB. Return no partial content; do not silently truncate.

A BeforeTool validator resolves the selected skill and path, rejects traversal and escaping links, and rewrites arguments to normalized skill name plus canonical absolute target. Accept the rewritten absolute form only within that selected canonical root. Restart the hook chain so workspace Policy approves the effective arguments. Set RequestedArgs through the existing Rewrite contract. Invalid paths return Deny before the approval callback.

Use the existing exact-external-call approval rule, with printable skill source and canonical target in its presentation. Allow once applies once. “Allow for this Dashi run” retains exact matching arguments in the process policy across `/new` and session switches. A fresh Policy loses those rules; snapshot recovery does not restore grants. A changed canonical target changes the arguments and needs approval again.

Open through a rooted filesystem handle. Revalidate target identity after approval and before reading bytes; refuse a link/file replacement between validation and open. Retain only the opened validated handle. Never reuse an approved raw relative path to open an unchecked target. The selected root admits reads only; it does not affect workspace writes, tool grants, or shell authority.

## Alternatives, costs, and compatibility

- Re-reading disk on Continue is smaller, but silently changes the loader. Reject it. Persist only selected model definitions and instruction bytes.
- Putting all bodies in model context needs no byte snapshot, but defeats progressive loading and increases every request. Reject it. Record a small digest reference.
- A new core metadata fact could keep the pin outside model input, but changes the foundation for one CLI consumer. Use existing ContextItem delivery.
- Expanding workspace roots reuses file tools, but couples reads to write scope and larger remembered grants. Keep a narrow support tool with exact-call approval.

The cost is discovery at admission, immutable local bytes, and one small pin message per turn. Snapshots can contain sensitive user-authored instructions, so apply session permissions and never print them. They are not attachments or credentials. Older sessions without resource tools can continue under their recorded tool selection; do not inject new resources into an already started legacy turn. New turns adopt freshly discovered resources. No database migration or new flag is required.

## Verification and integration

Keyless tests cover preference/fallback/read failures, source precedence, invocation restrictions, built-in collisions, literal shell syntax, approval denial/cancellation, process-policy lifetime, traversal/link replacement, and resource byte corruption. Edit instructions and skill files between prompts in one process; the next prompt and slash expansion must use the edits without `/resources` or restart. Recovery fixtures interrupt before/after the pin, between skill load and completion, and before pending tool execution. Change disk files and inspect `/resources`; prove the open bundle stays pinned while pending inventory changes. Reopen with a fresh Engine and prove continuation retains the old bundle. Test queued successor rediscovery/binding, refresh errors without stale reuse, empty-catalog clearing, unchanged context, retained/dropped compaction, and unchanged provider prefixes.

Run root and CLI race tests, formatting/build checks, a runnable Example/transcript, and actual Wonton flows at 80×24 in empty/loading/error/success states. Required independent implementation/acceptance, security, and taste reviews remain implementation checkpoints. Schedule shared wiring/dispatch edits with slices 2/3/5 and #103/#116. Custom system prompts stay with cli-providers. No unresolved design question blocks implementation.
