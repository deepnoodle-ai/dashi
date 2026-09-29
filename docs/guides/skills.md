# Skills

A skill is a folder of instructions that an agent loads when a task needs it.
The `skill` package reads skills from disk, gives the model a short catalog,
and lets a person start a skill by name, for example `/review src/api`.

## Load skills and give them to an agent

```go
set, warnings := skill.LoadDirs(skill.DefaultDirs(repoRoot, home)...)
for _, w := range warnings {
	log.Printf("skill: %s: %s", w.Path, w.Message)
}
bundle, err := set.Bundle(skill.Options{})
if err != nil {
	return err
}
runner.Tools = tool.Join(tool.List{existing}, bundle)
runner.Hooks = dashi.JoinHooks(runner.Hooks, bundle)
```

`bundle.Hooks()` gives the catalog to the model before each model call, once
per turn. The model calls `load_skill` to read the full text of a skill.
[`examples/skills`](../../examples/skills) is a complete program.

## Files

| Path | Kind |
| --- | --- |
| `<dir>/<name>/SKILL.md` | Folder skill. Model and user can use it. |
| `<dir>/<name>.md` | Command file. Only the user can use it. |

The loader reads a flat `.md` file in any search dir, so a stray `NOTES.md`
in a skills dir becomes a command. Keep notes out of these dirs (`README.md`
is skipped). Other files in a skill folder are supporting files. The loader does not read
them. The text that the model receives starts with `Base directory: <dir>`,
so the agent can read a supporting file with its own file tool.

`DefaultDirs` lists these dirs, first match wins: for the project, then the
home dir, `.dashi/skills`, `.agents/skills`, `.claude/skills`,
`.dashi/commands`, and `.claude/commands`. Pass the repository root, not the
working directory. A project skill hides a user skill of the same name
without a warning. To use other dirs, pass them to `LoadDirs` in order.

## SKILL.md format

```markdown
---
name: review
description: Review a change for bugs. Use before a commit.
argument-hint: <path>
---

Review $1 for bugs. Report each bug with its file and line.
```

- `name` is optional. It defaults to the folder or file name. A name has
  letters, digits, `-` and `_`, and at most 64 bytes.
- `description` is what the model sees. Keep it under 1,024 bytes.
- `argument-hint` is for your UI. `Info.ArgumentHint` returns it.
- `disable-model-invocation: true` keeps the skill out of the catalog.
- `user-invocable: false` stops `Expand` from returning the skill.
- The reader accepts the forms that Claude Code files use: `argument-hint:
  [a] [b]`, a colon in an unquoted description, and text after ` #`. If
  strict YAML fails, the loader reads plain `key: value` lines. A field with
  a value it cannot use (for example a list for `description`) is ignored, and
  a warning names the file and the field.
- A file with no frontmatter is valid. Other fields, such as
  `allowed-tools`, are ignored. A skill file cannot grant a tool.

## Warnings

A bad file never stops the load. The loader skips it and returns a `Warning`
with the file path. Causes: a file that cannot be read, a missing closing
`---`, invalid YAML, a bad name, an empty body, a body over 64 KiB, a
description over 1,024 bytes, two skills with one name in one dir, and a
folder that has no `SKILL.md` (nested dirs such as `commands/git/` are not
loaded).

The catalog holds at most 256 skills and 64 KiB. The loader fills it in name
order. It leaves out each skill that does not fit and warns. That skill stays
available to the user.

## Start a skill by name

```go
if body, ok := set.Expand("review", "src/api"); ok {
	prompt = body // send as the user's message
}
```

`Expand` replaces `$ARGUMENTS` with the text after the name and `$1` to `$9`
with its words. If the body has no placeholder, `Expand` appends
`ARGUMENTS: <text>`. It returns false for an unknown name and for a skill
that is not user-invocable. Call `set.Skills()` to find the cause and to
build a `/` menu. Names match without regard to case.

## Trust

Skill files are project text. A cloned repository can contain a hostile one.
For this reason `bundle.Hooks()` delivers the catalog as contextual runtime
context, not as operator text. A skill body that the model loads is tool
result text. Skills grant no tool and run no command. Your tool approval
policy still decides every action.

If you trust your skill dirs, you can put `bundle.Catalog()` in
`Agent.Instructions` instead. Do not put `bundle.Digest()` in
`Agent.Revision` for file skills: a person may edit a file between runs, and
a changed revision refuses to resume the turn. The catalog item ID holds the
digest, so the record shows which catalog the model saw.

## Moving from Dive

Dive shell expansion (`!{cmd}`), trigger words, `model`, and `allowed-tools`
are not supported. A skill that uses them still loads, and the loader
ignores them. Dive `.dive` dirs are not read. Copy the files to `.dashi` or
`.claude`. Namespaced command dirs (`commands/git/commit.md`) are not loaded.

To reload skills, call `LoadDirs` again and build a new bundle.
