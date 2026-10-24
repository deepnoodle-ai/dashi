package claudecode

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Provider facts recorded on converted assistant messages.
const (
	provider = "anthropic"
	api      = "anthropic.messages"
)

// metaReminderName names the reminder made from a user line with isMeta.
const metaReminderName = "claude-code-meta"

// Convert returns one agent's lines of a session as Dashi messages; agentID
// is empty for the root. copies are every current copy of the session; an
// agent with several copies is converted in start order. The error is only
// for a copy that cannot be read or an agent with no copy.
func Convert(copies []sessionfile.Copy, agentID string) (sessionfile.Transcript, error) {
	t := sessionfile.Transcript{SkippedLines: map[string]int{}, ChangedBlocks: map[string]int{}}
	cls, err := readSession(copies)
	if err != nil {
		return t, err
	}
	if !slices.ContainsFunc(cls, func(cl *copyLines) bool { return cl.AgentID == agentID }) {
		return t, fmt.Errorf("no copy of agent %q in this session", agentID)
	}
	inherited := inheritedUUIDs(cls)
	// Usage of a model call goes to the earliest-started copy that has it,
	// as in Parse.
	counted := map[string]bool{}
	cv := &converter{t: &t, changed: sessionfile.Counts(t.ChangedBlocks), counted: counted}
	claimed := 0
	for idx, cl := range cls {
		if cl.AgentID != agentID {
			continue
		}
		for ; claimed < idx; claimed++ {
			for _, mc := range cls[claimed].calls {
				claimUsage(mc, counted)
			}
		}
		cv.forkContext(cls, idx)
		cv.convert(cl, cl.final, inherited[idx], false)
		sessionfile.Counts(t.SkippedLines).Merge(cl.skipped)
	}
	return t, nil
}

// forkContext adds the parent's conversation that a fork by reference
// starts with. The parent is looked up in every other copy, so a parent
// without a start time is found too. When several copies of the agent
// fork, a parent line is added only the first time. A copy that forks at a
// later parent line gets the parent lines it adds just before its own
// messages, after the earlier copies: the transcript goes copy by copy in
// start order, and each copy follows the context it started with.
func (cv *converter) forkContext(cls []*copyLines, idx int) {
	for _, l := range cls[idx].Lines {
		if l.Type == "fork-context-ref" && l.ParentLastUUID != "" {
			for i, p := range cls {
				if i == idx {
					continue
				}
				if last := p.byUUID[l.ParentLastUUID]; last != nil {
					// Lines written after the fork point were not in its context.
					path := finalPath(p, last)
					for x := range path {
						if x.N > last.N || cv.forked[x] {
							delete(path, x)
						}
					}
					if cv.forked == nil {
						cv.forked = map[*line]bool{}
					}
					for x := range path {
						cv.forked[x] = true
					}
					cv.convert(p, path, nil, true)
					break
				}
			}
			break
		}
	}
}

type converter struct {
	t       *sessionfile.Transcript
	changed sessionfile.Counts
	// counted holds model calls whose usage an earlier copy has.
	counted map[string]bool
	// forked holds the parent lines already added as fork context.
	forked map[*line]bool
}

func (cv *converter) add(m llm.Message, ref sessionfile.LineRef) {
	m.Content = sessionfile.Keep(m.Content, cv.changed)
	if len(m.Content) == 0 {
		return
	}
	cv.t.Messages = append(cv.t.Messages, m)
	cv.t.LineRefs = append(cv.t.LineRefs, ref)
}

// convert adds the lines of cl on path, in file order. A line is inherited
// when all is set or its uuid is in inherited.
func (cv *converter) convert(cl *copyLines, path map[*line]bool, inherited map[string]bool, all bool) {
	calls := map[string]*modelCall{}
	for _, mc := range cl.calls {
		if mc.ID != "" {
			calls[mc.ID] = mc
		}
	}
	done := map[string]bool{} // model calls already added
	for _, l := range cl.Lines {
		if !path[l] || (l.skip != "" && l.skip != sessionfile.SkipRewound) {
			continue
		}
		ref := sessionfile.LineRef{CopyID: cl.ID, Lines: []int{l.N}, Inherited: all || inherited[l.UUID]}
		switch l.Type {
		case "user":
			m := cv.user(l)
			ref.CompactionSummary = l.IsCompactSummary
			cv.add(m, ref)
		case "attachment":
			text := renderedText(l)
			if m, ok := sessionfile.Reminder(l.Attachment.typ(), "", text, cv.changed); ok {
				cv.add(m, ref)
			}
		case "assistant":
			id := l.Message.ID
			if id != "" && done[id] {
				continue
			}
			mc := calls[id]
			lines := []*line{l}
			if mc != nil {
				done[id] = true
				lines = slices.DeleteFunc(slices.Clone(mc.Lines), func(x *line) bool { return !path[x] })
			}
			m, served := cv.assistant(lines)
			ref.Lines = ref.Lines[:0]
			for _, x := range lines {
				ref.Lines = append(ref.Lines, x.N)
			}
			if !ref.Inherited && mc != nil {
				ref.Model = served
				if hasLineUsage(mc) && !cv.counted[mc.ID] {
					u := mc.Usage
					ref.Usage = &u
				}
			}
			cv.add(m, ref)
		}
	}
}

func (a *attachment) typ() string {
	if a == nil {
		return ""
	}
	return a.Type
}

func renderedText(l *line) string {
	parts := make([]string, 0, len(l.Rendered))
	for _, r := range l.Rendered {
		if r.Content != "" {
			parts = append(parts, r.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// user converts a user line.
func (cv *converter) user(l *line) llm.Message {
	bs := blocks(l.Message.Content)
	if l.IsMeta {
		var texts []string
		for _, b := range bs {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			} else {
				cv.changed.Add(sessionfile.ChangeDropped + b.Type)
			}
		}
		m, _ := sessionfile.Reminder(metaReminderName, "", strings.Join(texts, "\n"), cv.changed)
		return m
	}
	m := llm.Message{Role: llm.RoleUser}
	for _, b := range bs {
		switch b.Type {
		case "text":
			m.Content = append(m.Content, llm.Text(b.Text))
		case "image", "document":
			if blk, ok := cv.media(b); ok {
				m.Content = append(m.Content, llm.Block(blk))
			}
		case "tool_result":
			m.Content = append(m.Content, llm.Content{Kind: llm.ContentToolResult, ToolResult: &llm.ToolResult{
				CallID: b.ToolUseID, Output: cv.toolOutput(b.Content), IsError: b.IsError,
			}})
		default:
			cv.changed.Add(sessionfile.ChangeDropped + b.Type)
		}
	}
	return m
}

// toolOutput converts tool_result content: a string, or a list of text,
// image, and document blocks. tool_reference blocks are dropped.
func (cv *converter) toolOutput(raw json.RawMessage) tool.Output {
	out := tool.Output{Content: []content.Block{}}
	for _, b := range blocks(raw) {
		switch b.Type {
		case "text":
			out.Content = append(out.Content, content.TextBlock(b.Text))
		case "image", "document":
			if blk, ok := cv.media(b); ok {
				out.Content = append(out.Content, blk)
			}
		default:
			cv.changed.Add(sessionfile.ChangeDropped + b.Type)
		}
	}
	return out
}

// media converts a base64 or URL image or document block.
func (cv *converter) media(b block) (content.Block, bool) {
	if b.Source == nil {
		cv.changed.Add(sessionfile.ChangeDropped + b.Type)
		return content.Block{}, false
	}
	var src content.Source
	switch b.Source.Type {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(b.Source.Data)
		if err != nil || len(data) == 0 {
			cv.changed.Add(sessionfile.ChangeDropped + b.Type)
			return content.Block{}, false
		}
		src = content.Source{Data: data, MIMEType: b.Source.MediaType}
	case "url":
		src = content.Source{URL: b.Source.URL}
	default:
		cv.changed.Add(sessionfile.ChangeDropped + b.Type)
		return content.Block{}, false
	}
	if b.Type == "document" {
		return content.DocumentBlock(src, b.Title), true
	}
	return content.ImageBlock(src), true
}

// assistant merges the lines of one model call into one message and returns
// it with the served model.
func (cv *converter) assistant(lines []*line) (llm.Message, string) {
	m := llm.Message{Role: llm.RoleAssistant}
	first := lines[0].Message
	served := first.Model
	for _, l := range lines {
		if u := l.Message.Usage; u != nil {
			for _, it := range u.Iterations {
				if it.Model != "" {
					served = it.Model
				}
			}
		}
		for _, b := range blocks(l.Message.Content) {
			switch b.Type {
			case "text":
				m.Content = append(m.Content, llm.Text(b.Text))
			case "thinking":
				if b.Thinking == "" {
					cv.changed.Add(sessionfile.ChangeDropped + "thinking")
					continue
				}
				if b.Signature != "" {
					cv.changed.Add(sessionfile.ChangeDropped + "signature")
				}
				m.Content = append(m.Content, llm.Content{Kind: llm.ContentReasoning, Reasoning: &llm.Reasoning{Text: b.Thinking}})
			case "tool_use":
				args := b.Input
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				m.Content = append(m.Content, llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{
					ID: b.ID, Name: b.Name, Args: args,
				}})
			default:
				cv.changed.Add(sessionfile.ChangeDropped + b.Type)
			}
		}
	}
	m.Source = &llm.MessageSource{
		Requested:  llm.ModelRef{Provider: provider, Model: first.Model},
		Served:     llm.ModelRef{Provider: provider, Model: served},
		API:        api,
		ResponseID: first.ID,
	}
	return m, served
}
