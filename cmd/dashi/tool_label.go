package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/runewidth"
)

// toolLabel is how a tool row names its call in words: Running while the
// call runs, such as "Reading render.go…", and Done once it ended, such as
// "Read render.go". The row shows no raw input or output; a click shows
// those.
type toolLabel struct {
	Running string
	Done    string
}

// Tool names the CLI itself defines.
const (
	toolLoadSkill     = "load_skill"
	toolReadSkillFile = "read_skill_file"
)

// mcpToolPrefix starts the name of a tool an MCP server provides, as in
// mcp__github__create_issue.
const mcpToolPrefix = "mcp__"

// Widths, in columns, of the subjects a label shows.
const (
	labelPathWidth    = 48
	labelPatternWidth = 32
	labelGlobWidth    = 24
	labelNameWidth    = 32
)

// toolLabelArgs holds the arguments a label reads. Each tool uses a few.
type toolLabelArgs struct {
	Path     string `json:"path"`
	Pattern  string `json:"pattern"`
	Name     string `json:"name"`
	Skill    string `json:"skill"`
	Subagent string `json:"subagent"`
	TaskID   string `json:"task_id"`
}

// labelPhrase is one row of toolLabels: the phrases with a subject, which
// take the subject in place of %s, and the phrases without one.
type labelPhrase struct {
	running, done         string
	runningBare, doneBare string
	subject               func(toolLabelArgs, string) string
}

// pathSubject is a path argument, relative to the workspace when it is
// inside it, and shortened from the left so the file name stays.
func pathSubject(a toolLabelArgs, dir string) string {
	if a.Path == "" {
		return ""
	}
	return shortenLeft(relPath(dir, a.Path), labelPathWidth)
}

// toolLabels holds the phrases of the tools Dashi knows. Running phrases end
// with "…"; done phrases are past tense.
var toolLabels = map[string]labelPhrase{
	workspace.ToolReadFile: {
		running: "Reading %s…", done: "Read %s",
		runningBare: "Reading a file…", doneBare: "Read a file",
		subject: pathSubject,
	},
	workspace.ToolEditFile: {
		running: "Editing %s…", done: "Edited %s",
		runningBare: "Editing a file…", doneBare: "Edited a file",
		subject: pathSubject,
	},
	workspace.ToolWriteFile: {
		running: "Writing %s…", done: "Wrote %s",
		runningBare: "Writing a file…", doneBare: "Wrote a file",
		subject: pathSubject,
	},
	workspace.ToolListDirectory: {
		running: "Listing %s…", done: "Listed %s",
		runningBare: "Listing the workspace…", doneBare: "Listed the workspace",
		subject: func(a toolLabelArgs, dir string) string {
			if a.Path == "." {
				return ""
			}
			return pathSubject(a, dir)
		},
	},
	workspace.ToolGlob: {
		running: "Finding files matching %s…", done: "Found files matching %s",
		runningBare: "Finding files…", doneBare: "Found files",
		subject: func(a toolLabelArgs, _ string) string {
			// A long glob says little at a glance; leave it out.
			if a.Pattern == "" || strings.Contains(a.Pattern, "\n") || runewidth.StringWidth(a.Pattern) > labelGlobWidth {
				return ""
			}
			return quoted(a.Pattern)
		},
	},
	workspace.ToolGrep: {
		running: "Searching for %s…", done: "Searched for %s",
		runningBare: "Searching files…", doneBare: "Searched files",
		subject: func(a toolLabelArgs, _ string) string {
			if a.Pattern == "" {
				return ""
			}
			return quoted(shortenRight(firstLine(a.Pattern), labelPatternWidth))
		},
	},
	workspace.ToolBash: {
		runningBare: "Running a command…", doneBare: "Ran a command",
	},
	workspace.ToolAskUser: {
		runningBare: "Asking you…", doneBare: "Asked you a question",
	},
	toolLoadSkill: {
		running: "Loading the %s skill…", done: "Loaded the %s skill",
		runningBare: "Loading a skill…", doneBare: "Loaded a skill",
		subject: func(a toolLabelArgs, _ string) string { return shortenRight(a.Name, labelNameWidth) },
	},
	toolReadSkillFile: {
		running: "Reading %s…", done: "Read %s",
		runningBare: "Reading a skill file…", doneBare: "Read a skill file",
		subject: func(a toolLabelArgs, _ string) string {
			if a.Path == "" || a.Skill == "" {
				return ""
			}
			return shortenLeft(a.Path, labelPathWidth) + " from the " + shortenRight(a.Skill, labelNameWidth) + " skill"
		},
	},
	delegate.ToolName: {
		running: "Starting the %s subagent…", done: "Delegated a task to %s",
		runningBare: "Starting a subagent…", doneBare: "Delegated a task",
		subject: func(a toolLabelArgs, _ string) string { return shortenRight(a.Subagent, labelNameWidth) },
	},
	delegate.StatusToolName: {
		running: "Checking task %s…", done: "Checked task %s",
		runningBare: "Checking a task…", doneBare: "Checked a task",
		subject: taskSubject,
	},
	delegate.StopToolName: {
		running: "Stopping task %s…", done: "Stopped task %s",
		runningBare: "Stopping a task…", doneBare: "Stopped a task",
		subject: taskSubject,
	},
	delegate.SendToolName: {
		running: "Messaging task %s…", done: "Messaged task %s",
		runningBare: "Messaging a task…", doneBare: "Messaged a task",
		subject: taskSubject,
	},
	delegate.MessageParentToolName: {
		runningBare: "Messaging the parent agent…", doneBare: "Messaged the parent agent",
	},
}

func taskSubject(a toolLabelArgs, _ string) string {
	return shortenRight(a.TaskID, labelNameWidth)
}

// newToolLabel returns the label of a call to the tool name with args. A
// path inside dir shows relative to dir. An MCP tool shows its name in words
// and its server, such as "Create issue · github"; any other tool shows its
// name in words. Neither has a tense, so both states share the text.
func newToolLabel(name string, args json.RawMessage, dir string) toolLabel {
	phrase, ok := toolLabels[name]
	if !ok {
		text := humanToolName(name)
		return toolLabel{Running: text, Done: text}
	}
	if phrase.subject != nil {
		var a toolLabelArgs
		_ = json.Unmarshal(args, &a)
		if s := phrase.subject(a, dir); s != "" {
			return toolLabel{Running: fmt.Sprintf(phrase.running, s), Done: fmt.Sprintf(phrase.done, s)}
		}
	}
	return toolLabel{Running: phrase.runningBare, Done: phrase.doneBare}
}

// created returns the label of a write that made a new file: "Created
// notes.md" in place of "Wrote notes.md".
func (l toolLabel) created() toolLabel {
	if rest, ok := strings.CutPrefix(l.Done, "Wrote "); ok {
		l.Done = "Created " + rest
	}
	return l
}

// humanToolName writes a tool name in words: underscores and hyphens become
// spaces, and the first letter is upper case, so read_notes reads "Read
// notes". An MCP tool adds its server: "Create issue · github".
func humanToolName(name string) string {
	if rest, ok := strings.CutPrefix(name, mcpToolPrefix); ok {
		if server, remote, ok := strings.Cut(rest, "__"); ok && server != "" && remote != "" {
			return sentenceCase(remote) + " · " + server
		}
	}
	return sentenceCase(name)
}

func sentenceCase(name string) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' || unicode.IsSpace(r) })
	if len(words) == 0 {
		return "Tool call"
	}
	s := strings.Join(words, " ")
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

// quoted puts curly quotes around s.
func quoted(s string) string { return "“" + s + "”" }

// shortenLeft cuts s to width columns from the left, so the end of a path,
// its file name, stays: "…/dashi/render.go".
func shortenLeft(s string, width int) string {
	if runewidth.StringWidth(s) <= width {
		return s
	}
	r := []rune(s)
	for i := range r {
		if rest := string(r[i:]); runewidth.StringWidth(rest) <= width-1 {
			return "…" + rest
		}
	}
	return "…"
}

// shortenRight cuts s to width columns from the right.
func shortenRight(s string, width int) string {
	return runewidth.Truncate(s, width, "…")
}

// toolLabelText is the phrase the tool row shows for its state. A call that
// has not ended shows the running phrase. A denied call never ran, so it
// shows the running phrase without the trailing "…", such as "Editing
// render.go" next to "denied".
func toolLabelText(b Block) string {
	l := b.Label
	if l == (toolLabel{}) {
		l = newToolLabel(b.Name, nil, "")
	}
	switch b.State {
	case StateOK, StateFailed, StateStopped:
		return l.Done
	case StateDenied:
		return strings.TrimSuffix(l.Running, "…")
	}
	return l.Running
}

// inputLineLimit is the most lines one argument shows in an expanded tool
// row. A file's whole content is seldom worth reading there.
const inputLineLimit = 40

// toolInput is the input of a call as an expanded tool row shows it: a
// shell command after "$ ", and otherwise one "name: value" line per
// argument, in name order. A multi-line value goes on indented lines under
// its name. A path inside dir shows relative to dir.
func toolInput(name string, raw json.RawMessage, dir string) string {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return strings.TrimSpace(string(raw))
	}
	var lines []string
	if cmd, ok := args["command"].(string); ok && name == workspace.ToolBash {
		lines = append(lines, "$ "+limitLines(cmd, "  "))
		delete(args, "command")
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var value string
		switch v := args[k].(type) {
		case string:
			value = v
			if k == "path" || k == "file_path" || k == "dir" {
				value = relPath(dir, v)
			}
		default:
			data, _ := json.Marshal(v)
			value = string(data)
		}
		if strings.Contains(value, "\n") {
			lines = append(lines, k+":", "  "+limitLines(value, "  "))
			continue
		}
		lines = append(lines, k+": "+value)
	}
	return strings.Join(lines, "\n")
}

// limitLines keeps the first inputLineLimit lines of s, indenting every
// line after the first, and says how many it left out.
func limitLines(s, indent string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if n := len(lines); n > inputLineLimit {
		lines = append(lines[:inputLineLimit], fmt.Sprintf("… %d more lines", n-inputLineLimit))
	}
	return strings.Join(lines, "\n"+indent)
}
