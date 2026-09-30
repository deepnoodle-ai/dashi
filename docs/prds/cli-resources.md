# Load and inspect workspace instructions and skills

Status: Approved\
PRD PR: Not opened · Implementation PR: Not opened · Updated: 2026-09-30

## Problem and context

A developer moving from Dive cannot use repository instructions or reusable skills in Dashi's terminal agent. They cannot inspect the selected sources or read a home skill's supporting files through scoped tools.

This delivers I09–I11 and the approved loaded-resource inventory in the [umbrella PRD](cli-parity.md). Dive loads AGENTS.md before CLAUDE.md, discovers project/home skills, and expands slash arguments. Dashi's [skill package](../guides/skills.md) already supplies discovery, invocation controls, safe expansion, and contextual catalogs. The CLI only wires workspace tools. [Claude Code](https://code.claude.com/docs/en/skills) separates user and model invocation. The [Agent Skills specification](https://agentskills.io/specification) separates startup metadata, invoked instructions, and supporting files loaded on demand.

## Users and concepts

The primary user codes with one terminal agent and wants the same selected instructions and skills across conversation turns and recovery.

**Workspace instructions** are the selected AGENTS.md or fallback CLAUDE.md at the workspace root. **Skill catalog** lists the skills the model may load. **Supporting file** belongs to a selected skill directory but is read separately. **Resource inventory** shows selected sources, availability, and effective access. Catalog discovery does not mean every skill body or supporting file entered the conversation.

## Use cases and acceptance

### UC-1: Start with repository instructions

Start Dashi in a workspace containing both instruction files. Inspect the startup summary and `/resources` before sending work.

- [ ] AGENTS.md wins. CLAUDE.md loads only when AGENTS.md is absent. Neither file means an explicit empty instruction state.
- [ ] The inventory shows the chosen path, contextual authority, and the fallback's unselected status. Instruction bodies do not appear in the inventory.
- [ ] An unreadable, invalid UTF-8, oversized, or escaping-symlink instruction produces a path-specific warning and no partial content. It never silently selects the fallback.
- [ ] File instructions enter recorded contextual runtime content, never system/developer instructions or tool authorization. Startup alone starts no model call or turn.

### UC-2: Discover and invoke the selected skill

Define `review` in project and home directories. Inspect `/resources`, then enter `/review src/api`. Let the model separately call `load_skill` for an eligible skill.

- [ ] Use `skill.DefaultDirs(workspaceRoot, home)` ordering: project before home; within each, `.dashi/skills`, `.agents/skills`, `.claude/skills`, `.dashi/commands`, `.claude/commands`.
- [ ] Case-insensitive conflicts resolve deterministically. Inspection identifies the effective source and shadowed candidates. Invalid files warn without preventing valid skills from loading.
- [ ] Folder skills default to user/model invocation. Command files are user-only. `disable-model-invocation: true` hides the skill from the model catalog and loader. `user-invocable: false` rejects slash invocation. Inspection shows both effective restrictions and argument hints.
- [ ] Built-in commands win name collisions. The inventory explains the collision. Unknown and non-user-invocable skill names produce distinct errors without starting a turn.
- [ ] `Expand` sends the expanded body as user input. Preserve `$ARGUMENTS`, `$1`–`$9`, empty missing arguments, and appended `ARGUMENTS:` when no placeholder exists.
- [ ] Shell syntax, including `!{command}`, backticks, and `$(command)`, remains literal. A keyless fixture proves expansion creates no subprocess or filesystem effect.
- [ ] The model receives only its eligible catalog until a named load. Loaded bodies remain recorded tool results. Loading never changes the model, tools, or approval policy.
- [ ] Inventory and help remain available during work. Skill invocation during an active turn requests an explicit retry when idle; it never becomes a literal steer or promises queued execution.

### UC-3: Read a supporting file with explicit approval

Invoke a home skill that references `checklist.md`. Read the file through the skill support-file path. Inspect its scope, approve or deny the exact read, then continue coding.

- [ ] Eligible paths stay inside the selected canonical skill directory. Traversal and symlink escapes fail before reading or requesting approval. Command files do not make their parent directory eligible.
- [ ] Reads outside the workspace need exact-call approval. Show the canonical target and selected skill source. Loading or invoking a skill grants no supporting-file permission.
- [ ] Allow once covers one call. “Allow for this Dashi run” remembers matching external-tool arguments across `/new` and session switches. A fresh policy/process loses remembered answers.
- [ ] Denial, cancellation, and an unanswered unattended approval read no bytes. A changed target cannot reuse a previous target's approval. Inventory shows eligible scope separately from remembered approval scope.
- [ ] Workspace write/edit roots remain unchanged. Skill metadata, including `allowed-tools`, grants no action. Shell work uses the existing explicitly approved shell tool.

### UC-4: Reopen and inspect current resources

Send two turns, exit, reopen the session, and send another. Repeat with changed instructions or skills and with a stopped open turn.

- [ ] Unchanged instructions and catalogs keep their context identity and occur once in proposed requests, including after reopen. Existing provider-facing prompt prefixes remain unchanged.
- [ ] Changed resources append the new contextual value for subsequent turns without editing earlier messages. Compaction retention follows [#117](session-context-dedup.md); this CLI introduces no deduplication or compaction engine.
- [ ] Open-turn continuation reuses its selected instruction and skill content. Changed files cannot silently rebind that turn's skill loader. Missing recovery content causes a clear stop before dispatch.
- [ ] `/resources` distinguishes the active turn's resources from newly discovered resources awaiting the next turn. It never infers execution or successful model use from catalog presence.

## Requirements, decisions, and limits

- Show a compact startup summary and a complete `/resources` inventory in screen and line modes. Print-mode notices use stderr. List currently registered tool sources and effective scopes alongside instructions and skills. MCP contributes its own source/status facts when its slice lands.
- Cover empty, loading, partial-error, and success states at 80×24. Loading cannot appear as an empty success. Paths, descriptions, warnings, and arguments render as printable text. Show no credentials, instruction bodies, or file contents.
- Bound workspace instructions to 64 KiB, matching the existing skill-body limit. Reject partial truncation because it can change a requirement's meaning.
- Use the selected workspace root for discovery. Reject ancestor walking, nested instruction stacks, `.dive` paths, and automatic source reload during a running turn.
- Reuse `LoadDirs`, `Bundle`, `Expand`, and ContextItem delivery. Reject file instructions in `Agent.Instructions`, shell expansion, and file digests in `Agent.Revision`. The [technical spec](../design/cli-resources.md) defines the host's recovery pin for selected skill bodies.

Custom system prompts and provider settings belong to slice 3. Composer completion belongs to slice 2 and consumes the eligible command catalog. MCP lifecycle belongs to slice 5. Attachment bytes, general context inspection, skill editing/installing, scripts, trigger matching, marketplaces, and compaction are outside this slice. Shared dispatch/wiring edits require scheduled integration ownership.

Success requires a keyless Example/transcript and Wonton recordings of the customer flows and states. Independent implementation/acceptance, security, and taste reviews must pass. Existing recovery, queue, approval, and prompt-prefix tests must pass. No material question blocks PRD review.
