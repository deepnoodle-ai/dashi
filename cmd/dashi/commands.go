package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/workspace"
)

type commandEntry struct {
	hidden                   bool
	group                    string
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
		{name: "/tasks", group: "Tasks", usage: "[ID]", description: "list delegated work and spend, or one task's turns and messages", run: func(arg string) { c.taskInput("/tasks " + arg) }, during: func(arg string) { c.taskInput("/tasks " + arg) }},
		{name: "/task", group: "Tasks", usage: "ID [continue|done CALL_ID|discard]", description: "inspect or control the exact recorded child", run: func(arg string) { c.taskInput("/task " + arg) }, during: func(arg string) { c.taskInput("/task " + arg) }},
		{name: "/task-stop", group: "Tasks", usage: "ID", description: "cancel one live child and verify its record", run: func(arg string) { c.taskInput("/task-stop " + arg) }, during: func(arg string) { c.taskInput("/task-stop " + arg) }},
		{name: "/continue", group: "Turn", description: "continue after a model-call limit stop", run: func(string) {
			if !c.eng.Last(c.base).LimitStopped() {
				c.notice("/continue works after a model-call limit stop.")
				return
			}
			c.startRun(c.eng.ContinueLimit)
		}},
		{name: "/done", group: "Turn", usage: "[CALL_ID]", description: "mark a command done after an interrupted run", run: func(arg string) {
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
		{name: "/discard", group: "Turn", description: "discard the open turn", run: func(string) {
			if !c.eng.Last(c.base).Open() {
				c.notice("There is no open turn to discard.")
				return
			}
			c.startRun(c.eng.Discard)
		}},
		{name: "/queue", group: "Turn", usage: "TEXT", description: "hold a prompt for the next turn", run: c.queuedPrompt, during: c.queue},
		{name: "/compact", group: "Turn", description: "summarize history before the next turn", run: func(arg string) {
			if arg != "" {
				c.notice("Usage: /compact")
				return
			}
			c.startRun(func(ctx context.Context) (Result, error) {
				res, err := c.eng.Compact(ctx)
				if errors.Is(err, session.ErrNothingToCompact) {
					c.notice("Nothing needs compaction.")
					return res, nil
				}
				return res, err
			})
		}},
		{name: "/new", group: "Session", description: "start a new session", run: func(string) {
			if !c.keepQueued("/new") {
				c.async(c.newSession)
			}
		}},
		{name: "/model", group: "Model", usage: "[destination] [model]", description: "choose the next-turn destination/model", run: run(c.model)},
		{name: "/sessions", group: "Session", usage: "[QUERY]", description: "find recorded sessions in this workspace", run: run(func(ctx context.Context, arg string) { c.sessions(ctx, arg) })},
		{name: "/rename", group: "Session", usage: "TITLE", description: "name the current saved session", run: c.renameSession},
		{name: "/status", group: "Session", usage: "[ID]", description: "inspect recorded status and own-session spend", run: c.inspectSession, during: c.inspectSession},
		{name: "/quit", group: "Session", aliases: []string{"/q", "/exit"}, description: "save and exit", run: func(string) {
			if !c.keepQueued("/quit") {
				c.cancelInspection()
				c.ui.Quit()
			}
		}, during: func(string) { c.QuitDuringRun("/quit") }},
		{name: "/settings", group: "Model", usage: "[--json]", description: "inspect next-turn and recorded settings", run: run(c.settings)},
		{name: "/mcp", group: "Inspect", description: "inspect MCP servers, sources, and tools", run: c.mcpCommand, during: c.mcpCommand},
		{name: "/resources", group: "Inspect", description: "inspect instruction, skill, and tool sources/scopes", run: func(arg string) {
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
		{name: "/help", group: "Inspect", aliases: []string{"/?"}, description: "show commands, skills, and keys", run: func(string) { c.emit(c.helpBlock()) }, during: func(string) { c.emit(c.helpBlock()) }},
	}
	_, printAttachments := c.ui.(*printUI)
	if _, hasAttachments := c.ui.(attachmentUI); hasAttachments || printAttachments {
		for _, entry := range []struct{ name, usage, description string }{{"/attach", "PATH", "select a frozen file snapshot"}, {"/attachments", "", "inspect numbered attachment selections"}, {"/detach", "N", "remove a selection and its mention tokens"}} {
			action := func(arg string) {
				if u, ok := c.ui.(attachmentUI); ok {
					u.AttachmentCommand(entry.name, arg)
				} else {
					c.notice("%s requires an interactive composer or --plain; dashi run uses --attach.", entry.name)
				}
			}
			command := commandEntry{name: entry.name, group: "Attachments", usage: entry.usage, description: entry.description, run: action}
			if entry.name == "/attachments" {
				command.during = action
			}
			entries = append(entries, command)
		}
	}
	for _, control := range []struct{ name, usage, description string }{
		{"/effort", "LEVEL|omit", "set next-turn reasoning effort"}, {"/thinking", "on|off", "request public reasoning summaries"}, {"/temperature", "N|omit", "set or omit sampling temperature"}, {"/max-tokens", "N|default", "set the next-turn output limit"},
	} {
		entries = append(entries, commandEntry{name: control.name, group: "Model", usage: control.usage, description: control.description, run: func(arg string) { c.async(func(ctx context.Context) { c.generationControl(ctx, control.name, arg) }) }})
	}
	if ui, ok := c.ui.(interface{ TranscriptCommand(string, string) }); ok {
		for _, display := range []struct{ name, usage, description string }{{"/copy", "[N|all]", "copy selection or code blocks"}, {"/scrollback", "[raw]", "inspect complete output in native scrollback"}, {"/mouse", "", "toggle application mouse capture"}} {
			action := func(arg string) { ui.TranscriptCommand(display.name, arg) }
			entries = append(entries, commandEntry{name: display.name, group: "Transcript", usage: display.usage, description: display.description, run: action, during: action})
		}
	}
	// Discovery uses metadata only. Skill text is loaded by the existing handler.
	if c.eng.cfg.Resources != nil {
		if found, err := c.eng.cfg.Resources.discover(); err == nil {
			for _, info := range found.Set.Skills() {
				name := "/" + info.Name
				if namesCommand(entries, name) {
					continue
				}
				usage := info.ArgumentHint
				if usage == "" {
					usage = "[arguments]"
				}
				description := strings.Join(strings.Fields(info.Description), " ")
				if description == "" {
					description = "invoke skill " + info.Name
				}
				entries = append(entries, commandEntry{name: name, group: skillGroup, hidden: !info.UserInvocable, usage: usage, description: description, run: func(arg string) { c.skillCommand(name+" "+arg, false) }})
			}
		}
	}
	return entries
}

// namesCommand reports whether name, such as "/quit", is the name or an
// alias of one of entries, ignoring case.
func namesCommand(entries []commandEntry, name string) bool {
	return slices.ContainsFunc(entries, func(e commandEntry) bool {
		return strings.EqualFold(e.name, name) ||
			slices.ContainsFunc(e.aliases, func(a string) bool { return strings.EqualFold(a, name) })
	})
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
	var items []commandMenuItem
	for _, entry := range c.commands() {
		if entry.hidden {
			continue
		}
		names = append(names, entry.name)
		names = append(names, entry.aliases...)
		items = append(items, commandMenuItem{name: entry.name, aliases: entry.aliases, usage: entry.usage, description: entry.description})
	}
	c.knownNames.store(names, items)
	return names
}

// commandNamesMaxAge is how long the composer trusts its cached command
// names. A skill added on disk is highlighted within about this long.
const commandNamesMaxAge = 2 * time.Second

// commandNameCache holds the names that commandNames last returned and the
// command menu's items. Listing commands discovers skills on disk, which is
// too slow for every render.
type commandNameCache struct {
	mu         sync.Mutex
	names      map[string]bool // lowercase
	items      []commandMenuItem
	loaded     time.Time
	refreshing bool
}

func (k *commandNameCache) store(names []string, items []commandMenuItem) {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[strings.ToLower(name)] = true
	}
	k.mu.Lock()
	k.names, k.items, k.loaded = set, items, time.Now()
	k.mu.Unlock()
}

// knownCommand reports whether name is a command or alias that commandNames
// lists, matched without regard to case as dispatch does. It is cheap enough
// to call on every render: it reads the cache and, when the cache is old,
// refreshes it in the background for a later render.
func (c *Controller) knownCommand(name string) bool {
	k := &c.knownNames
	k.mu.Lock()
	defer k.mu.Unlock()
	c.refreshCommandNamesLocked()
	return k.names[strings.ToLower(name)]
}

// commandMenuItems returns the cached commands the command menu offers. The
// first call lists the commands at once; later calls refresh an old cache in
// the background, as knownCommand does.
func (c *Controller) commandMenuItems() []commandMenuItem {
	k := &c.knownNames
	k.mu.Lock()
	loaded := !k.loaded.IsZero()
	k.mu.Unlock()
	if !loaded {
		c.commandNames()
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	c.refreshCommandNamesLocked()
	return k.items
}

// refreshCommandNamesLocked starts a background refresh when the cache is
// old. The caller holds c.knownNames.mu.
func (c *Controller) refreshCommandNamesLocked() {
	k := &c.knownNames
	if !k.refreshing && time.Since(k.loaded) > commandNamesMaxAge {
		k.refreshing = true
		go func() {
			defer func() {
				k.mu.Lock()
				k.refreshing = false
				k.mu.Unlock()
			}()
			c.commandNames()
		}()
	}
}

// helpGroups orders the built-in command groups in help. Skills follow.
var helpGroups = []string{"Session", "Turn", "Model", "Inspect", "Tasks", "Attachments", "Transcript"}

const skillGroup = "Skills"

func (c *Controller) helpBlock() Block {
	h := c.helpDoc()
	return Block{Kind: KindHelp, Text: h.String(), Help: &h, Done: true}
}

func (c *Controller) commandHelp() string { return c.helpDoc().String() }

func (c *Controller) helpDoc() helpDoc {
	var h helpDoc
	entries := c.commands()
	for _, group := range append(slices.Clone(helpGroups), skillGroup) {
		section := helpSection{Title: group}
		for _, entry := range entries {
			if entry.hidden || entry.group != group {
				continue
			}
			name := strings.Join(append([]string{entry.name}, entry.aliases...), ", ")
			section.Rows = append(section.Rows, helpRow{Name: name, Usage: entry.usage, Text: entry.description, During: entry.during != nil})
		}
		if len(section.Rows) > 0 {
			h.Sections = append(h.Sections, section)
		}
	}
	// The legend closes the last built-in group; skills never run during a turn.
	for i := len(h.Sections) - 1; i >= 0; i-- {
		if h.Sections[i].Title != skillGroup {
			h.Sections[i].Note = "  " + duringMarker + " also works while a turn runs"
			break
		}
	}
	keys := helpSection{Title: "Keys (fullscreen and inline modes)", Rows: composerKeys, Keys: true}
	_, full := c.ui.(*screen)
	_, inline := c.ui.(*inlineScreen)
	if full || inline {
		keys.Title = "Keys"
	}
	if inline {
		keys.Rows = slices.Clone(composerKeys)
		for i := range keys.Rows {
			switch keys.Rows[i].Name {
			case "Ctrl+C":
				keys.Rows[i].Text = "interrupt or press twice to exit"
			case "F2":
				keys.Rows[i].Text = "native terminal selection is already enabled"
			}
		}
		keys.Note = "  Use your terminal's selection and copy controls in inline mode."
	} else if !full {
		keys.Note = "  Selection copying with Ctrl+C and the F2 toggle apply to fullscreen mode.\n  Inline uses your terminal's selection and copy controls; Ctrl+C interrupts or exits."
	}
	h.Sections = append(h.Sections, keys)
	return h
}

var composerKeys = []helpRow{
	{Name: "Enter", Text: "send the draft"},
	{Name: "Shift+Enter", Text: "insert a new line"},
	{Name: "Up/Down", Text: "move between lines, then recall history"},
	{Name: "Tab", Text: "complete; Tab and Shift+Tab cycle choices"},
	{Name: "Esc", Text: "undo completion, stop work, or return to live output"},
	{Name: "Page Up/Down", Text: "scroll the transcript"},
	{Name: "Ctrl+G", Text: "edit the draft in an external editor"},
	{Name: "Ctrl+C", Text: "copy selection; otherwise interrupt or press twice to exit"},
	{Name: "F2", Text: "toggle native selection without changing the draft"},
	{Name: "Ctrl+D", Text: "delete forward; exit on an empty draft"},
	{Name: "text, !text", Text: "while a turn runs: steer it; ! also cuts the model call"},
}

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
			fmt.Fprintln(stdout, workspace.PrintableText(c.commandHelp()))
			fmt.Fprintln(stdout)
			fmt.Fprintln(stdout, workspace.PrintableText("With dashi run, only /help and /quit aliases run. Use --plain or interactive mode for other commands."))
			return true, 0
		case "/quit":
			return true, 0
		default:
			fmt.Fprintln(stderr, workspace.PrintableText(fmt.Sprintf("dashi: %s requires --plain or interactive mode; it was not sent.", entry.name)))
			return true, 2
		}
	}
	fmt.Fprintln(stderr, workspace.PrintableText(fmt.Sprintf("dashi: unknown command %s. Use dashi run /help for available commands.", name)))
	return true, 2
}
