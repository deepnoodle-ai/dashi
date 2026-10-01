package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	dashisession "github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/workspace"
)

type commandEntry struct {
	hidden                   bool
	name, usage, description string
	aliases                  []string
	run                      func(string)
	during                   func(string)
}

// commands is the inventory shared by dispatch, help, and completion.
func (c *Controller) commands() []commandEntry {
	run := func(fn func(context.Context, string)) func(string) {
		return func(arg string) { c.async(func(ctx context.Context) { fn(ctx, arg) }) }
	}
	entries := []commandEntry{
		{name: "/tasks", description: "list delegated work and separate parent/child spend", run: func(arg string) { c.taskInput("/tasks " + arg) }, during: func(arg string) { c.taskInput("/tasks " + arg) }},
		{name: "/task", usage: "ID [continue|done CALL_ID|discard]", description: "inspect or control the exact recorded child", run: func(arg string) { c.taskInput("/task " + arg) }, during: func(arg string) { c.taskInput("/task " + arg) }},
		{name: "/task-stop", usage: "ID", description: "cancel one live child and verify its record", run: func(arg string) { c.taskInput("/task-stop " + arg) }, during: func(arg string) { c.taskInput("/task-stop " + arg) }},
		{name: "/compact", description: "summarize history before the next turn", run: func(arg string) {
			if arg != "" {
				c.notice("Usage: /compact")
				return
			}
			c.startRun(func(ctx context.Context) (Result, error) {
				res, err := c.eng.Compact(ctx)
				if errors.Is(err, dashisession.ErrNothingToCompact) {
					c.notice("Nothing needs compaction.")
					return res, nil
				}
				return res, err
			})
		}},
		{name: "/quit", aliases: []string{"/q", "/exit"}, description: "save and exit", run: func(string) {
			if !c.keepQueued("/quit") {
				c.cancelInspection()
				c.ui.Quit()
			}
		}, during: func(string) { c.QuitDuringRun("/quit") }},
		{name: "/continue", description: "continue after a model-call limit stop", run: func(string) {
			if !c.eng.Last(c.base).LimitStopped() {
				c.notice("/continue works after a model-call limit stop.")
				return
			}
			c.startRun(c.eng.ContinueLimit)
		}},
		{name: "/done", usage: "[CALL_ID]", description: "mark a command done after an interrupted run", run: func(arg string) {
			id, _ := c.eng.Last(c.base).OpenCall()
			if arg != "" && arg != id {
				c.notice("/done must name the current unresolved call %s; no outcome was recorded.", id)
				return
			}
			if id == "" {
				c.notice("/done marks a command done after dashi stopped while it ran.")
				return
			}
			c.startRun(c.eng.MarkDone)
		}},
		{name: "/discard", description: "discard the open turn", run: func(string) {
			if !c.eng.Last(c.base).Open() {
				c.notice("There is no open turn to discard.")
				return
			}
			c.startRun(c.eng.Discard)
		}},
		{name: "/queue", usage: "TEXT", description: "hold a prompt for the next turn", run: c.queuedPrompt, during: c.queue},
		{name: "/new", description: "start a new session", run: func(string) {
			if !c.keepQueued("/new") {
				c.async(c.newSession)
			}
		}},
		{name: "/model", usage: "[destination] [model]", description: "choose the next-turn destination/model", run: run(c.model)},
		{name: "/sessions", usage: "[QUERY]", description: "find recorded sessions in this workspace", run: run(func(ctx context.Context, arg string) { c.sessions(ctx, arg) })},
		{name: "/rename", usage: "TITLE", description: "name the current saved session", run: c.renameSession},
		{name: "/status", usage: "[ID]", description: "inspect recorded status and own-session spend", run: c.inspectSession, during: c.inspectSession},
		{name: "/settings", usage: "[--json]", description: "inspect next-turn and recorded settings", run: run(c.settings)},
		{name: "/mcp", description: "inspect MCP servers, sources, and tools", run: c.mcpCommand, during: c.mcpCommand},
		{name: "/resources", description: "inspect instruction, skill, and tool sources/scopes", run: func(arg string) {
			if arg != "" {
				c.notice("Usage: /resources")
				return
			}
			c.async(c.resources)
		}, during: func(arg string) {
			if arg != "" {
				c.notice("Usage: /resources")
				return
			}
			c.goWork(func() { c.resources(c.base) })
		}},
		{name: "/help", aliases: []string{"/?"}, description: "show commands and composer controls", run: func(string) { c.notice("%s", c.commandHelp()) }, during: func(string) { c.notice("%s", c.commandHelp()) }},
	}
	_, printAttachments := c.ui.(*printUI)
	if _, hasAttachments := c.ui.(attachmentUI); hasAttachments || printAttachments {
		for _, entry := range []struct{ name, usage, description string }{{"/attach", "PATH", "select a frozen file snapshot"}, {"/attachments", "", "inspect numbered attachment selections"}, {"/detach", "N", "remove a selection and its mention tokens"}} {
			action := func(arg string) {
				if u, ok := c.ui.(attachmentUI); ok {
					u.AttachmentCommand(entry.name, arg)
				} else {
					c.notice("%s requires an interactive composer or --plain; -p uses --attach.", entry.name)
				}
			}
			command := commandEntry{name: entry.name, usage: entry.usage, description: entry.description, run: action}
			if entry.name == "/attachments" {
				command.during = action
			}
			entries = append(entries, command)
		}
	}
	for _, control := range []struct{ name, usage, description string }{
		{"/effort", "LEVEL|omit", "set next-turn reasoning effort"}, {"/thinking", "on|off", "request public reasoning summaries"}, {"/temperature", "N|omit", "set or omit sampling temperature"}, {"/max-tokens", "N|default", "set the next-turn output limit"},
	} {
		entries = append(entries, commandEntry{name: control.name, usage: control.usage, description: control.description, run: func(arg string) { c.async(func(ctx context.Context) { c.generationControl(ctx, control.name, arg) }) }})
	}
	if ui, ok := c.ui.(interface{ TranscriptCommand(string, string) }); ok {
		for _, display := range []struct{ name, usage, description string }{{"/copy", "[N|all]", "copy selection or code blocks"}, {"/scrollback", "[raw]", "inspect complete output in native scrollback"}, {"/mouse", "", "toggle application mouse capture"}} {
			action := func(arg string) { ui.TranscriptCommand(display.name, arg) }
			entries = append(entries, commandEntry{name: display.name, usage: display.usage, description: display.description, run: action, during: action})
		}
	}
	// Discovery uses metadata only. Skill text is loaded by the existing handler.
	if c.eng.cfg.Resources != nil {
		if found, err := c.eng.cfg.Resources.discover(); err == nil {
			for _, info := range found.Set.Skills() {
				name := "/" + info.Name
				if slices.ContainsFunc(entries, func(e commandEntry) bool { return strings.EqualFold(e.name, name) }) {
					continue
				}
				entries = append(entries, commandEntry{name: name, hidden: !info.UserInvocable, usage: "[arguments]", description: "invoke skill " + info.Name, run: func(arg string) { c.skillCommand(name+" "+arg, false) }})
			}
		}
	}
	return entries
}

func splitCommand(line string) (name, arg string) {
	line = strings.TrimSpace(line)
	i := strings.IndexFunc(line, unicode.IsSpace)
	if i < 0 {
		return strings.ToLower(line), ""
	}
	return strings.ToLower(line[:i]), strings.TrimSpace(line[i:])
}

func (c *Controller) command(line string) { c.dispatchCommand(line, false) }
func (c *Controller) duringTurnCommand(line string) bool {
	if !strings.HasPrefix(line, "/") {
		return false
	}
	c.dispatchCommand(line, true)
	return true
}
func (c *Controller) dispatchCommand(line string, running bool) {
	name, arg := splitCommand(line)
	for _, entry := range c.commands() {
		if entry.name != name && !slices.Contains(entry.aliases, name) && !strings.EqualFold(entry.name, name) {
			continue
		}
		if running {
			if entry.hidden {
				c.skillCommand(line, true)
				return
			}
			if entry.during != nil {
				entry.during(arg)
			} else {
				c.notice("%s must wait until the turn ends. Submit it again then, or press Ctrl-C to stop the turn.", entry.name)
			}
		} else {
			entry.run(arg)
		}
		return
	}
	candidates := rankCompletion(name, c.commandNames())
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}
	hint := "Type /help."
	if len(candidates) > 0 {
		hint = "Available matches: " + strings.Join(candidates, ", ") + ". Type /help."
	}
	c.notice("Unknown command %s. %s", name, hint)
}
func (c *Controller) commandNames() []string {
	var names []string
	for _, entry := range c.commands() {
		if entry.hidden {
			continue
		}
		names = append(names, entry.name)
		names = append(names, entry.aliases...)
	}
	return names
}
func (c *Controller) commandHelp() string {
	var out strings.Builder
	out.WriteString("Commands:\n")
	for _, entry := range c.commands() {
		if entry.hidden {
			continue
		}
		aliases := ""
		if len(entry.aliases) > 0 {
			aliases = " (" + strings.Join(entry.aliases, ", ") + ")"
		}
		availability := "idle; submit again after a turn"
		if entry.during != nil {
			availability = "idle or during a turn"
		}
		fmt.Fprintf(&out, "  %s %s%s — %s [%s]\n", entry.name, entry.usage, aliases, entry.description, availability)
	}
	out.WriteString("\nTask costs: recorded parent caps exclude children. Explore has 24 attempts; General has 32. Children have no monetary cap.\n")
	out.WriteString(composerKeyboardHelp)
	if _, ok := c.ui.(*screen); !ok {
		if _, inline := c.ui.(*inlineScreen); !inline {
			out.WriteString("\nExternal editing requires supported fullscreen/inline terminal handoff.\n")
		}
	}
	return out.String()
}

const composerKeyboardHelp = `
Fullscreen/inline composer controls:
  Enter / Shift+Enter: submit displayed draft / insert newline.
  Up / Down: move through visual rows, then recall history; past newest restores the draft.
  Tab: start completion. Tab/Down and Shift+Tab/Up cycle candidates.
  Escape: restore pre-completion draft while cycling; otherwise use existing work, selection, and transcript actions. Dialogs retain cancellation.
  Ctrl+G: open external editor while idle with supported handoff; while running, press again after the turn ends.
  Ctrl+C: interrupt running work; idle exit requires an unchanged repeat within 2 seconds. Dialog controls retain precedence.
  Ctrl+D: delete at cursor in nonempty draft; empty draft requests exit with pending/queued-loss protection.
While a turn runs, text is a steer. Start it with ! to cut the model call.
/queue TEXT holds text as the next prompt. Input history is recall, not recovery storage.
`

// Print mode has no interactive command loop. Never turn unknown slash input
// into a provider request. Help and quit remain useful without starting work.
func (c *Controller) printCommand(line string, stdout, stderr io.Writer) (bool, int) {
	if !strings.HasPrefix(strings.TrimSpace(line), "/") {
		return false, 0
	}
	name, _ := splitCommand(line)
	for _, entry := range c.commands() {
		if !strings.EqualFold(entry.name, name) && !slices.Contains(entry.aliases, name) {
			continue
		}
		switch entry.name {
		case "/help":
			fmt.Fprint(stdout, workspace.PrintableText(c.commandHelp()))
			fmt.Fprintln(stdout, workspace.PrintableText("With -p, only /help and /quit aliases run. Use --plain or interactive mode for other commands."))
			return true, 0
		case "/quit":
			return true, 0
		default:
			fmt.Fprintln(stderr, workspace.PrintableText(fmt.Sprintf("dashi: %s requires --plain or interactive mode; it was not sent.", entry.name)))
			return true, 2
		}
	}
	fmt.Fprintln(stderr, workspace.PrintableText(fmt.Sprintf("dashi: unknown command %s. Use -p /help for available commands.", name)))
	return true, 2
}
