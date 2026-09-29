# PRD: skills from the filesystem

**Roadmap item 12 · Status: Draft, second slice · 2026-09-28.** Builds on the in-memory bundle merged in PR #59. Source: [research](12-skills-research.md). Reference: Dive's `skill` package.

## Problem

A developer keeps skills on disk: `.dashi/skills/review/SKILL.md`, `.claude/skills`, or a folder in the home directory. Today the developer must read each file, parse frontmatter, check limits, and build definitions by hand. The developer must also write code so that a CLI user can type `/review`. Every Dashi application that wants Claude Code style skills would repeat this work. If we do nothing, the terminal agent has no skills, and the loader logic spreads into each application.

## Goals and success measures

- **Primary:** `skill.LoadDirs(dirs...)` returns skills and warnings, and one more call gives the agent its catalog and tool. Measure: the skills example loads a fixture directory in under 15 lines.
- **Secondary:** A bad file never stops the load. Measure: a fixture with five bad files and two good files loads two skills and returns five warnings that each name the file.
- **Guardrail:** A skill file cannot grant a tool, run a command, or read a file. The turn core does not change.

## Users

- **Primary:** A Go developer who builds an agent application and wants users to add skills as files.
- **Secondary:** A CLI user who writes `SKILL.md` files and types `/name`. This user may never write Go.

## Public API

New names in package `skill`. The existing `Definition`, `Bundle`, `New`, and `Options` stay. `Definition` gains one field, `Dir`.

```go
// LoadDirs reads every <dir>/<name>/SKILL.md. Earlier dirs win over later dirs.
// A missing dir is not an error. It never fails; bad files become warnings.
func LoadDirs(dirs ...string) (*Set, []Warning)

// DefaultDirs lists the standard search dirs, highest precedence first:
// project .dashi/skills, .agents/skills, .claude/skills, then the same
// three under home. An empty argument skips that level.
func DefaultDirs(projectDir, homeDir string) []string

type Warning struct{ Path, Message string }

type Set struct{ /* immutable */ }
func (s *Set) Skills() []Info                                // all skills, sorted by name
func (s *Set) Bundle(o Options) (*Bundle, error)             // model-visible skills only
func (s *Set) Expand(name, args string) (string, bool)       // user invocation
type Info struct{ Name, Description, ArgumentHint, Dir string; ModelInvocable, UserInvocable bool }
```

The application flow:

```go
set, warns := skill.LoadDirs(skill.DefaultDirs(cwd, home)...)
for _, w := range warns { log.Printf("skill: %s: %s", w.Path, w.Message) }
bundle, err := set.Bundle(skill.Options{})
runner.Tools = tool.Join(tool.List{existing}, bundle)
runner.Agent.Instructions += "\n\n" + bundle.Catalog()

// The CLI user types "/review src/api":
if body, ok := set.Expand("review", "src/api"); ok { prompt = body }
```

## Behavior

1. **Layout.** A skill is a directory with a `SKILL.md` file. Other files in the directory are supporting files. The loader does not read them.
2. **Frontmatter.** YAML between `---` lines. The loader reads `name`, `description`, `argument-hint`, `disable-model-invocation`, and `user-invocable`. The name defaults to the directory name. A file with no frontmatter is valid.
3. **Names.** A name matches `[A-Za-z0-9][A-Za-z0-9_-]*` and has at most 64 bytes, so `/name` works. Lookup ignores case.
4. **Precedence.** The first dir that holds a name wins. A project skill hides a user skill of the same name. This is not a warning.
5. **Warnings.** These skip one skill and warn: unreadable file, missing closing `---`, invalid YAML, bad name, empty body, body over 64 KiB, description over 1,024 bytes. Two skills with the same name in one dir tree warn too. The loader keeps at most 256 skills and warns about the rest.
6. **Ignored fields.** `allowed-tools` and other fields are ignored without a warning. Skill text never grants a tool. The `Bundle` doc already states this.
7. **Catalog.** Names and descriptions go in the prompt. The body enters the record only when the model calls `load_skill`. Skills with `disable-model-invocation: true` are left out of the bundle.
8. **Supporting files.** When `Dir` is set, the loaded body begins with `Base directory: <dir>`. The agent reads supporting files with its own file tool. The loader gives no file access.
9. **Slash use.** `Expand` returns the body with `$ARGUMENTS` and `$1` to `$9` replaced. If the body has no placeholder and `args` is not empty, `Expand` appends `ARGUMENTS: <args>`. For a skill with `user-invocable: false`, `Expand` returns false.
10. **Order.** Skills sort by name. The catalog and digest do not depend on directory order.

## User stories

- **US-1.** As a developer, I call `LoadDirs` and hand the result to the agent. *Accept:* fixture dirs load; the catalog lists each model-visible skill; `load_skill` returns the body.
- **US-2.** As a skill author, I break one file and still use the rest. *Accept:* each failure case in behavior 5 has a fixture and a test that checks the file path in the warning.
- **US-3.** As a CLI user, I type `/review src/api`. *Accept:* `Expand("review","src/api")` returns the body with the argument in place. An unknown name returns false.
- **US-4.** As a project owner, I override a user skill. *Accept:* with the same name in project and home dirs, the project body loads.
- **US-5.** As a developer, I read the guide and run the example. *Accept:* `docs/guides/skills.md` exists; `examples/skills` loads a fixture dir; its test passes offline.

## Out of scope

- `.claude/commands/*.md` flat command files. Later.
- Shell expansion (`!{cmd}`), trigger keywords, `model` override, and `allowed-tools` enforcement. Dive had some of these. A skill body must not run code.
- A `load_skill` argument. The model reads the body and applies it.
- File watching and reload. The CLI reloads on start or on request by calling `LoadDirs` again.
- Remote skill sources, marketplaces, nested skill dirs, and conditional skills.
- Slash command parsing and completion. The CLI owns them.

## Risks

| Risk | Impact | Mitigation |
|---|---|---|
| A cloned repo carries hostile skill text | Prompt injection at the trust level of the catalog | The host picks the catalog placement. The CLI guide says how. Skills grant no tool. |
| Editing a skill changes the bundle digest | `Continue` rejects a session whose revision holds the digest | The guide says to keep the digest out of `Agent.Revision` for file skills, or to keep the snapshot. The CLI decides. |
| Users expect Dive features that we cut | Skills that use `!{cmd}` load but do not run it | The guide lists the differences. |
| Symlinks and odd file trees | Loader reads outside the dir | The loader reads only `SKILL.md`, follows symlinked dirs, and reads no other file. |

## Decisions

- **New type `Set`, not `Catalog`.** `Bundle.Catalog()` already means the prompt text. A second `Catalog` would confuse readers.
- **`LoadDirs` takes explicit dirs.** The library does not guess paths. `DefaultDirs` gives the standard list, so the CLI and other apps share one rule. Dive hid this in options.
- **Read `.dashi`, `.agents`, and `.claude` dirs.** Users already have skills there. Order inside a level: `.dashi`, then `.agents`, then `.claude`.
- **No shell expansion in the library.** It breaks the rule that a body cannot run code. Dive gated it by flag. Dashi drops it until a host asks.
- **Expansion only for the user path.** The model loads the raw body. This keeps the recorded tool result equal to the file text and keeps existing `Bundle` behavior.
- **Slash invocation is a function, not a tool.** `Expand` returns text. The CLI sends that text as the user's prompt. No turn-core change.
- **`Dir` on `Definition`, in the digest.** A moved skill changes its base directory line, so the digest must change.

## Ship plan

**By Tuesday night (this slice):** `LoadDirs`, `DefaultDirs`, `Set`, `Expand`, `Dir` support, fixtures and tests, updated example, `docs/guides/skills.md`, roadmap update. The CLI wiring is a later, short PR: load in `main`, add the bundle, route `/name` to `Expand`.

**Later:** command files, `load_skill` arguments, reload, shell expansion behind an explicit host option, and skill-scoped tool requests that a host may honor.

## Open questions

None block this slice. The CLI track owns the catalog placement and the revision policy in the risks table.
