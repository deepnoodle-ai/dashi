# Instructions and skills in the CLI

Dashi reads AGENTS.md at your workspace root. It uses CLAUDE.md only when
AGENTS.md is absent. Instructions are contextual conversation content; they
do not change the system prompt or approve tools. Files must be regular UTF-8
text, at most 64 KiB. Links must stay inside the workspace. A read error warns
and selects empty instructions rather than using the fallback.

## Find and invoke a skill

Project sources take precedence over home sources. Within each, Dashi searches
`.dashi/skills`, `.agents/skills`, `.claude/skills`, `.dashi/commands`, and
`.claude/commands`. Folder skills use SKILL.md; commands are Markdown files.
See the [skill format](skills.md) for frontmatter and argument placeholders.

```text
/resources
/review src/api
```

`/resources` shows selected and shadowed sources, invocation restrictions,
argument hints, instruction status, and registered tool scopes. It omits
instruction bodies and file contents. Built-in commands win name collisions.
A skill typed during a running turn requests retry when idle.

Slash invocation expands the skill as your user prompt. `$ARGUMENTS` and
`$1`–`$9` substitute literal arguments; shell expressions never execute.
`user-invocable: false` prevents slash invocation. Command files are user-only.
`disable-model-invocation: true` excludes a skill from the model catalog.

The model sees an eligible catalog and requests `load_skill` for a full body.
Loading instructions never grants tool authority. Metadata such as
`allowed-tools` does not bypass approvals.

## Approve a supporting-file read

The model can request `read_skill_file` with a selected folder skill and a
relative path. The reader stays inside that skill's canonical folder; traversal
and escaping links are denied before approval. Command files have no supporting
folder. Reads return complete UTF-8 text up to 64 KiB, or a failed outcome.

The approval shows the selected skill source and canonical target. Allow once
covers one call. Allow for this Dashi run remembers only those exact tool
arguments, across session switches and `/new`. A changed canonical target
requires approval again. Restarting Dashi forgets remembered approvals.
Skill folders do not expand workspace writes or shell permissions.

## File changes and session history

Dashi refreshes instructions and the skill catalog before model requests.
Slash commands and new skill loads use current files. Queued work sees edits
when it starts. `/resources` shows what is currently available.

Skill problems never appear in the transcript. `/resources` lists each with
its path under "Not loaded", and adjustments, such as a shortened description,
under "Loaded with adjustments". A file reached through two search dirs counts
once. A folder without SKILL.md is not a skill and is skipped silently. An
instruction file that exists but cannot be used warns once per problem, since
the model then works without it.

A loaded skill's text is an ordinary recorded tool result. Reloading the
session keeps that result in conversation history. A later load can return
the updated file without changing the earlier result. No separate resource
files are needed when moving a session.

Unchanged contextual values are not repeated in requests; changed values append
without rewriting history. Normal session compaction rules apply.
