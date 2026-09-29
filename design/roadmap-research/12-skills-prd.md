# PRD: skills from the filesystem

**Roadmap item 12 · Status: Approved, implemented on branch `skills/filesystem`, second slice · 2026-09-28.** Builds on the in-memory bundle merged in PR #59. Source: [research](12-skills-research.md). Reference: Dive's `skill` package.

## Problem

A developer keeps skills on disk: `.dashi/skills/review/SKILL.md`, `.claude/skills`, a command file such as `.claude/commands/review.md`, or a folder in the home directory. Today the developer must read each file, parse frontmatter, check limits, and build definitions by hand. The developer must also write code so that a CLI user can type `/review`. Every Dashi application that wants Claude Code style skills would repeat this work. If we do nothing, the terminal agent has no skills, and the loader logic spreads into each application.

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
// LoadDirs reads every <dir>/<name>/SKILL.md and every <dir>/<name>.md.
// Earlier dirs win over later dirs. A missing dir is not an error.
// It never fails; bad files become warnings.
func LoadDirs(dirs ...string) (*Set, []Warning)

// DefaultDirs lists the standard search dirs, highest precedence first. For
// project, then home: .dashi/skills, .agents/skills, .claude/skills,
// .dashi/commands, .claude/commands. An empty argument skips that level.
// Pass the repository root as projectDir, not the working directory.
func DefaultDirs(projectDir, homeDir string) []string

type Warning struct{ Path, Message string }

type Set struct{ /* immutable */ }
func (s *Set) Skills() []Info                                // all skills, sorted by name
func (s *Set) Bundle(o Options) (*Bundle, error)             // model-visible skills only; fails only on a bad ToolName
func (s *Set) Expand(name, args string) (string, bool)       // user invocation
type Info struct{ Name, Description, ArgumentHint, Dir string; ModelInvocable, UserInvocable bool }

// Reminder is the catalog as contextual (non-operator) runtime context.
func (b *Bundle) Reminder() llm.Reminder

// Hooks delivers that reminder before each model call, once per turn, as a
// runtime snapshot item. Join it with the host's hooks.
func (b *Bundle) Hooks() dashi.Hooks
```

The application flow:

```go
set, warns := skill.LoadDirs(skill.DefaultDirs(cwd, home)...)
for _, w := range warns { log.Printf("skill: %s: %s", w.Path, w.Message) }
bundle, err := set.Bundle(skill.Options{})
runner.Tools = tool.Join(tool.List{existing}, bundle)

// Project text is not operator text. Hooks delivers the catalog as runtime
// context, not in Agent.Instructions. The item ID holds the digest for audit.
runner.Hooks = dashi.JoinHooks(runner.Hooks, bundle.Hooks())
// Do not put bundle.Digest() in Agent.Revision for file skills (see Decisions).

// The CLI user types "/review src/api":
if body, ok := set.Expand("review", "src/api"); ok { prompt = body }
// On false, call set.Skills() to tell "unknown" from "not user-invocable".
```

## Behavior

1. **Layout.** A folder skill is a directory with a `SKILL.md` file. Other files in it are supporting files. The loader does not read them. A command file is a flat `<dir>/<name>.md` file, found in any search dir. Its name is the file name without `.md`. A command file is user-invocable only and is never in the catalog. The loader skips `README.md` and dotfiles. A subdirectory with no `SKILL.md` (for example `commands/git/`) produces a warning that nested directories are not loaded.
2. **Frontmatter.** YAML between `---` lines. The loader reads `name`, `description`, `argument-hint`, `disable-model-invocation`, and `user-invocable`. The name defaults to the directory or file name. A file with no frontmatter is valid.
3. **Names.** A name matches `[A-Za-z0-9][A-Za-z0-9_-]*` and has at most 64 bytes, so `/name` works. Lookup ignores case.
4. **Precedence.** The first dir that holds a name wins. A project skill hides a user skill of the same name. This is not a warning.
5. **Warnings.** These skip one skill or file and warn: unreadable file, missing closing `---`, invalid YAML, bad name, empty body, body over 64 KiB, description over 1,024 bytes. Two skills with the same name in one dir tree warn too.
6. **Ignored fields.** `allowed-tools` and other fields are ignored without a warning. Skill text never grants a tool. The `Bundle` doc already states this.
7. **Catalog.** Names and descriptions go to the model as contextual runtime context. The body enters the record only when the model calls `load_skill`. Folder skills with `disable-model-invocation: true`, and all command files, are left out of the bundle. The loader keeps the rendered catalog within the 64 KiB limit and the 256-skill limit. It fills the catalog in name order and drops each skill that does not fit, with one warning that names it. A dropped skill is not model-invocable. It stays user-invocable through `Expand`.
8. **Base directory.** One rule for both paths: for a folder skill, the text that the model or the user path receives starts with `Base directory: <dir>`. Both `load_skill` and `Expand` add it. A command file has no directory and gets no line. The agent reads supporting files with its own file tool. The loader gives no file access.
9. **Slash use.** `Expand` returns the body, with the base-directory line, and with `$ARGUMENTS` and `$1` to `$9` replaced. If the body has no placeholder and `args` is not empty, `Expand` appends `ARGUMENTS: <args>`. For a skill with `user-invocable: false`, `Expand` returns false.
10. **Order.** Skills sort by name. The catalog and digest do not depend on directory order.

## User stories

- **US-1.** As a developer, I call `LoadDirs` and hand the result to the agent. *Accept:* fixture dirs load; the catalog lists each model-visible skill; `load_skill` returns the body.
- **US-2.** As a skill author, I break one file and still use the rest. *Accept:* each failure case in behavior 5 has a fixture and a test that checks the file path in the warning.
- **US-3.** As a CLI user, I type `/review src/api`. *Accept:* `Expand("review","src/api")` returns the body with the argument in place. An unknown name returns false.
- **US-4.** As a project owner, I override a user skill. *Accept:* with the same name in project and home dirs, the project body loads.
- **US-5.** As a developer, I read the guide and run the example. *Accept:* `docs/guides/skills.md` exists; `examples/skills` loads a fixture dir; its test passes offline.

## Out of scope

- Namespaced command subdirectories (`commands/git/commit.md`). The loader warns when it finds one. Dive's `.dive` dirs are not read.
- Shell expansion (`!{cmd}`), trigger keywords, `model` override, and `allowed-tools` enforcement. Dive had some of these. A skill body must not run code.
- A `load_skill` argument. The model reads the body and applies it.
- File watching and reload. The CLI reloads on start or on request by calling `LoadDirs` again.
- Remote skill sources, marketplaces, nested skill dirs, and conditional skills.
- Slash command parsing and completion. The CLI owns them.

## Risks

| Risk | Impact | Mitigation |
|---|---|---|
| A cloned repo carries hostile skill text | Prompt injection | The catalog is contextual runtime context, not operator text. A loaded body is tool-result text. Skills grant no tool. |
| Editing a skill file between runs | The reopened session sees the new catalog and body | The digest stays out of `Agent.Revision`. The catalog item ID records it for audit. |
| Users expect Dive features that we cut | Skills that use `!{cmd}` load but do not run it | The guide lists the differences and how to move a file. |
| Symlinks and odd file trees | Loader reads outside the dir | The loader reads only `SKILL.md`, follows symlinked dirs, and reads no other file. |

## Decisions

- **New type `Set`, not `Catalog`.** `Bundle.Catalog()` already means the prompt text. A second `Catalog` would confuse readers.
- **`LoadDirs` takes explicit dirs.** The library does not guess paths. `DefaultDirs` gives the standard list, so the CLI and other apps share one rule. Dive hid this in options.
- **Read `.dashi`, `.agents`, and `.claude` dirs.** Users already have skills there. Order inside a level: `.dashi`, then `.agents`, then `.claude`.
- **No shell expansion in the library.** It breaks the rule that a body cannot run code. Dive gated it by flag. Dashi drops it until a host asks.
- **Expansion only for the user path.** The model loads the body without argument replacement. Both paths add the same base-directory line (behavior 8). Review finding 3, accepted.
- **Slash invocation is a function, not a tool.** `Expand` returns text. The CLI sends that text as the user's prompt. No turn-core change.
- **`Dir` on `Definition`, in the digest.** A moved skill changes its base directory line, so the digest must change.
- **Review finding 1, accepted: the catalog never fails.** The loader enforces the size limits and drops what does not fit, with a named warning each. `Bundle` fails only on a bad `ToolName`. A fixture test with 70 long descriptions covers it.
- **Review finding 2, accepted: project text is not operator text.** The catalog goes in a `ContextRuntime` item with a contextual reminder, as the sample shows. An app that trusts its skill dirs may put `Catalog()` in `Agent.Instructions`. The guide says so.
- **Review finding 4, accepted: command files load.** Users expect `/review` to work from `.claude/commands`. The cost is one small parser case. Nested command directories warn.
- **Review finding 5, accepted: no digest in the revision.** File skills change by design between runs, and demo step 7 reopens a session. The digest goes in the context item ID.
- **Review finding 6, accepted as written.** `Expand` keeps its bool. The guide shows `Skills()` for the cause.
- **Review finding 7, accepted.** `DefaultDirs` takes the repository root. The CLI passes the nearest `.git` directory, not the working directory.

- **Orchestrator correction: `Bundle.Hooks` delivers the catalog.** The first sample made every host write about 12 lines of `BeforeModel` code. One call now does it: `Hooks` returns a `dashi.Hooks` with one `BeforeModel` hook that delivers the catalog snapshot unless the turn already holds it. `Reminder` stays for hosts that deliver the catalog their own way. An empty bundle returns no hooks. The `skill` package now imports the root `dashi` package. The root package does not import `skill`, so there is no cycle.
- **Implementation calls.** The library module adds `gopkg.in/yaml.v3` for frontmatter, because real skills use block scalars. The loader measures the catalog with the longest tool name (64 bytes), so any valid `Options.ToolName` fits. `load_skill` output for a folder skill is the base-directory line, then the `# name` heading, then the body. A file over 80 KiB warns and is skipped. `ArgumentHint` is kept for UIs and is not shown to the model.

## Ship plan

**By Tuesday night (this slice):** `LoadDirs`, `DefaultDirs`, `Set`, `Expand`, `Dir` support, fixtures and tests, updated example, `docs/guides/skills.md`, roadmap update. The CLI wiring is a later, short PR: load in `main`, add the bundle and catalog item, route `/name` to `Expand`.

**Later:** namespaced commands, `load_skill` arguments, reload, shell expansion behind an explicit host option, and skill-scoped tool requests that a host may honor.

## Open questions

None.
