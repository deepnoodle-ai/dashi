# CLI instructions and skills

Status: Accepted · Updated: 2026-09-30

## Responsibilities

The CLI connects existing skill discovery, contextual delivery, slash expansion,
and recorded tool results. It adds source inspection and approved supporting-file
reads. It does not add resource persistence or session recovery machinery.

| Location | Responsibility |
| --- | --- |
| `skill/load.go`, `skill/skill.go` | Existing discovery and loading; expose defining paths and cloned candidate metadata. |
| `cmd/dashi/resources.go` | Workspace instruction selection and skill discovery. |
| `cmd/dashi/resource_manager.go` | Existing context hooks and skill loader composition. |
| `cmd/dashi/control_resources.go` | Inventory, slash invocation, and approval presentation. |
| `cmd/dashi/skill_files.go` | Selected-folder validation and supporting-file reads. |

Dependencies flow from CLI adapters to existing Dashi, skill, and workspace
contracts. No new public Agent field or resource framework is needed.

## Discovery and delivery

Select AGENTS.md at the workspace root; use CLAUDE.md only if AGENTS.md is absent.
Files must be regular UTF-8 text up to 64 KiB. Rooted reads reject escaping
links, nonregular files, and identity changes. Read failures warn and select
explicit empty instructions without fallback or partial content.

Use separate contextual messages for source and body, preserving the full
64 KiB body allowance. Deliver through `SeedContext`; existing visible-value
deduplication and compaction rules apply. Instructions grant no tool authority.

Use `LoadDirs(DefaultDirs(workspace, home)...)` and `Set.Bundle` for skills.
Discover at startup, inventory inspection, slash invocation, and before model
requests. Catalogs contain only model-eligible metadata. An empty selection
delivers an explicit empty catalog. Discovery warnings retain partial success.

The stable CLI `load_skill` tool discovers current skills and delegates to the
existing bundle loader. Its returned body is an ordinary recorded tool result.
Normal session reload preserves that result. Future loads use current files.
Do not serialize unrequested definitions, cache resource bytes beside sessions,
add per-turn references, or implement special resource restoration.

## Slash commands and inventory

Built-ins win case-insensitive collisions. Idle custom commands use
`Set.Expand` and the normal prompt path. Arguments remain literal. Unknown and
restricted commands produce distinct notices; commands received during work
request retry when idle. Composer completion uses eligible metadata.

`/resources` shows current instruction status, selected and shadowed skills,
source paths, invocation restrictions, argument hints, warnings, registered
tools, eligible supporting folders, and remembered exact-read approvals.
It delivers no model context and prints no bodies or credentials.
Display source paths and resolved folders separately. Startup uses a compact
summary; print-mode notices go to stderr. Loading has an explicit state.

## Supporting-file reads

`read_skill_file` accepts a selected folder skill and a path. Command files
do not make their parent directory eligible. The BeforeTool validator rejects
traversal and escaping links, then rewrites arguments to the normalized skill
name and canonical absolute target. The existing policy approves those effective
arguments. Invalid paths are denied before approval.

Approval names the skill source and canonical target. Allow once covers one
call. Remembered exact arguments live in the process policy across `/new`
and session switches; a fresh process loses them. A changed canonical target
needs approval again. The inventory's remembered-answer list is display only;
the policy remains authoritative.

Open through a rooted filesystem handle. Validate root and target identity
after approval and before reading. Refuse file or link replacement. Return
complete regular UTF-8 text up to 64 KiB or a failed outcome. Denial and
cancellation return no bytes. Skill folders grant no write or shell authority.

## Verification

Keyless tests cover instruction preference and failures, source precedence,
invocation restrictions, literal expansion, built-in collisions, changed files,
queued work, and unchanged context. Reload a session after a skill load and
prove the old tool result remains while a new load returns current content.

Security checks cover traversal, escaping links, defining-file links,
nonregular files, cancellation, remembered approval lifetime, changed canonical
targets, and replacement after approval. Run root and CLI race tests, vet and
build, plus actual Wonton flows at 80×24. Independent implementation, security,
and taste reviews cover the [PRD](../prds/cli-resources.md).
