package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Provider facts recorded on converted assistant messages and provider
// tool use.
const (
	provider = "openai"
	api      = "openai.responses"
	// scope stands in for the endpoint and credential namespace, which
	// session files do not record.
	scope = "codex"
)

// agentMessageReminder names the reminder made from an agent_message item.
const agentMessageReminder = "agent-message"

// Convert returns one agent's lines of a session as Dashi messages; agentID
// is empty for the root. An agent whose session spans two files is
// converted in time order. The error is only for a copy that cannot be
// read or an agent with no copy.
func Convert(copies []sessionfile.Copy, agentID string) (sessionfile.Transcript, error) {
	t := sessionfile.Transcript{SkippedLines: map[string]int{}, ChangedBlocks: map[string]int{}}
	if !slices.ContainsFunc(copies, func(c sessionfile.Copy) bool { return c.AgentID == agentID && !c.ForkParent }) {
		return t, fmt.Errorf("no copy of agent %q in this session", agentID)
	}
	// Every copy is read: a fork's parent shows which lines it repeats.
	cls, err := readSession(copies)
	if err != nil {
		return t, err
	}
	cv := &converter{t: &t, changed: sessionfile.Counts(t.ChangedBlocks), awaiting: -1}
	for _, cl := range cls {
		if cl.AgentID != agentID {
			continue
		}
		cv.convert(cl)
		sessionfile.Counts(t.SkippedLines).Merge(cl.skipped)
	}
	return t, nil
}

type converter struct {
	t       *sessionfile.Transcript
	changed sessionfile.Counts

	// The assistant message being built from adjacent items of one reply.
	reply    *llm.Message
	replyRef sessionfile.LineRef
	model    string
	// awaiting is the index of the last reply added since a token line, or
	// -1. Codex can write a reply's token line after its tool outputs.
	awaiting int
}

func (cv *converter) add(m llm.Message, ref sessionfile.LineRef) {
	m.Content = sessionfile.Keep(m.Content, cv.changed)
	if len(m.Content) == 0 && !ref.CompactionSummary {
		return
	}
	cv.t.Messages = append(cv.t.Messages, m)
	cv.t.LineRefs = append(cv.t.LineRefs, ref)
}

// flush adds the assistant message being built, with usage when a token
// line ended it.
func (cv *converter) flush(usage *llm.Usage, responseID, model string) {
	if cv.reply == nil {
		return
	}
	m, ref := *cv.reply, cv.replyRef
	cv.reply = nil
	if model == "" {
		model = cv.model
	}
	m.Source = &llm.MessageSource{
		Requested:  llm.ModelRef{Provider: provider, Model: model},
		Served:     llm.ModelRef{Provider: provider, Model: model},
		API:        api,
		ResponseID: responseID,
	}
	if !ref.Inherited {
		ref.Model = model
		ref.Usage = usage
	}
	n := len(cv.t.Messages)
	cv.add(m, ref)
	if len(cv.t.Messages) > n && usage == nil && !ref.Inherited {
		cv.awaiting = n
	}
}

// tokenLine ends a reply. A token line with usage gives it to the reply
// being built, else to the last reply added since the previous token line.
func (cv *converter) tokenLine(usage *llm.Usage, responseID, model string) {
	switch {
	case cv.reply != nil:
		cv.flush(usage, responseID, model)
	case usage != nil && cv.awaiting >= 0:
		if model == "" {
			model = cv.model
		}
		ref := &cv.t.LineRefs[cv.awaiting]
		ref.Usage, ref.Model = usage, model
		src := *cv.t.Messages[cv.awaiting].Source
		src.Requested.Model, src.Served.Model, src.ResponseID = model, model, responseID
		cv.t.Messages[cv.awaiting].Source = &src
	}
	cv.awaiting = -1
}

// isTokenLine reports whether a line is a token line of the copy's usage
// source; one ends a reply.
func isTokenLine(l *line, source string) bool {
	switch source {
	case sessionfile.UsageSourceTokenUsageRecord:
		return l.Type == "token_usage_record"
	case sessionfile.UsageSourceTokenCount:
		return l.Type == "event_msg" && l.item.Type == "token_count" && l.item.Info != nil
	}
	return false
}

func (cv *converter) convert(cl *copyLines) {
	calls, source := modelCalls(cl)
	usageAt := map[int]modelCall{}
	for _, mc := range calls {
		usageAt[mc.Line] = mc
	}
	for _, l := range cl.Lines {
		if l.Type == "turn_context" && l.item.Model != "" {
			cv.model = l.item.Model
		}
		if mc, ok := usageAt[l.N]; ok {
			u := mc.Usage
			cv.tokenLine(&u, mc.ResponseID, mc.Model)
			continue
		}
		if isTokenLine(l, source) {
			cv.tokenLine(nil, l.item.ResponseID, "") // an inherited or repeated token line still ends a reply
			continue
		}
		if l.skip != "" {
			continue
		}
		ref := sessionfile.LineRef{CopyID: cl.ID, Lines: []int{l.N}, Inherited: l.inherited}
		if l.Type == "compacted" {
			cv.flush(nil, "", "")
			m := llm.Message{Role: llm.RoleUser, Content: []llm.Content{}}
			if l.item.Message != "" {
				m.Content = []llm.Content{llm.Text(l.item.Message)}
			}
			ref.CompactionSummary = true
			cv.add(m, ref)
			continue
		}
		it := l.item
		switch {
		case it.Type == "reasoning" || (it.Type == "message" && it.Role == "assistant") || isCall(it.Type):
			cv.replyItem(l, ref)
		case it.Type == "compaction":
			cv.changed.Add(sessionfile.ChangeDropped + "compaction")
		case it.Type == "message":
			cv.flush(nil, "", "")
			cv.add(cv.message(it), ref)
		case it.Type == "agent_message":
			cv.flush(nil, "", "")
			text := cv.texts(it.Content)
			if m, ok := sessionfile.Reminder(agentMessageReminder, it.Author, text, cv.changed); ok {
				cv.add(m, ref)
			}
		case isOutput(it.Type):
			cv.flush(nil, "", "")
			cv.add(cv.output(it), ref)
		}
	}
	cv.flush(nil, "", "")
}

// replyItem adds a reasoning, assistant message, or call item to the reply
// being built.
func (cv *converter) replyItem(l *line, ref sessionfile.LineRef) {
	if cv.reply != nil && cv.replyRef.Inherited != ref.Inherited {
		cv.flush(nil, "", "")
	}
	if cv.reply == nil {
		cv.reply = &llm.Message{Role: llm.RoleAssistant}
		cv.replyRef = ref
	} else {
		cv.replyRef.Lines = append(cv.replyRef.Lines, l.N)
	}
	it := l.item
	m := cv.reply
	switch it.Type {
	case "reasoning":
		var texts []string
		for _, s := range it.Summary {
			if s.Text != "" {
				texts = append(texts, s.Text)
			}
		}
		if len(texts) == 0 {
			cv.changed.Add(sessionfile.ChangeDropped + "reasoning")
			return
		}
		if it.EncryptedContent != "" {
			cv.changed.Add(sessionfile.ChangeDropped + "encrypted_content")
		}
		m.Content = append(m.Content, llm.Content{Kind: llm.ContentReasoning, Reasoning: &llm.Reasoning{Text: strings.Join(texts, "\n\n")}})
	case "message":
		m.Content = append(m.Content, cv.parts(it.Content)...)
	case "web_search_call":
		id := it.ID
		if id == "" {
			id = "ws_line_" + strconv.Itoa(l.N)
			cv.changed.Add(sessionfile.ChangeMadeUpID)
		}
		m.Content = append(m.Content, llm.Content{Kind: llm.ContentProviderToolUse, ProviderToolUse: &llm.ProviderToolUse{
			ID: id, Name: "web_search", Type: it.Type, Status: it.Status, Input: it.Action,
			Origin: content.Origin{Provider: provider, API: api, Model: cv.modelOr(), Scope: scope},
		}})
	default:
		m.Content = append(m.Content, llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{
			ID: it.CallID, Name: toolName(it), Args: callArgs(it),
		}})
	}
}

func (cv *converter) modelOr() string {
	if cv.model == "" {
		return "unknown"
	}
	return cv.model
}

// toolName joins an MCP namespace and a tool name with "__".
func toolName(it item) string {
	switch {
	case it.Type == "tool_search_call":
		return "tool_search"
	case it.Namespace == "":
		return it.Name
	}
	return strings.TrimRight(it.Namespace, "_") + "__" + strings.TrimLeft(it.Name, "_")
}

// callArgs returns a call's arguments as JSON: a function call's argument
// text, a custom tool's free-form input as a JSON string, or a tool
// search's argument object.
func callArgs(it item) json.RawMessage {
	switch it.Type {
	case "custom_tool_call":
		var s string
		if json.Unmarshal(it.Input, &s) == nil {
			b, _ := json.Marshal(s)
			return b
		}
		return it.Input
	case "function_call":
		var s string
		if json.Unmarshal(it.Arguments, &s) == nil {
			return json.RawMessage(s)
		}
	}
	return it.Arguments
}

// message converts a user, developer, or system message.
func (cv *converter) message(it item) llm.Message {
	role := llm.RoleUser
	switch it.Role {
	case "developer":
		role = llm.RoleDeveloper
	case "system":
		role = llm.RoleSystem
	}
	return llm.Message{Role: role, Content: cv.parts(it.Content)}
}

type part struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

// parts converts message content: text parts and images.
func (cv *converter) parts(raw json.RawMessage) []llm.Content {
	var ps []part
	if json.Unmarshal(raw, &ps) != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return []llm.Content{llm.Text(s)}
		}
		return nil
	}
	var out []llm.Content
	for _, p := range ps {
		switch p.Type {
		case "input_text", "output_text", "text":
			out = append(out, llm.Text(p.Text))
		case "input_image":
			if b, ok := cv.image(p.ImageURL); ok {
				out = append(out, llm.Block(b))
			}
		default:
			cv.changed.Add(sessionfile.ChangeDropped + p.Type)
		}
	}
	return out
}

// texts joins the text parts of agent_message content, dropping others.
func (cv *converter) texts(raw json.RawMessage) string {
	var ps []part
	_ = json.Unmarshal(raw, &ps)
	var texts []string
	for _, p := range ps {
		if p.Type == "input_text" || p.Type == "output_text" || p.Type == "text" {
			texts = append(texts, p.Text)
		} else {
			cv.changed.Add(sessionfile.ChangeDropped + p.Type)
		}
	}
	return strings.Join(texts, "\n")
}

// image converts a data URL or a URL.
func (cv *converter) image(u string) (content.Block, bool) {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, ok := strings.Cut(rest, ",")
		mime, isBase64 := strings.CutSuffix(meta, ";base64")
		if ok && isBase64 {
			if b, err := base64.StdEncoding.DecodeString(data); err == nil && len(b) > 0 {
				return content.ImageBlock(content.Source{Data: b, MIMEType: mime}), true
			}
		}
		cv.changed.Add(sessionfile.ChangeDropped + "input_image")
		return content.Block{}, false
	}
	if u == "" {
		cv.changed.Add(sessionfile.ChangeDropped + "input_image")
		return content.Block{}, false
	}
	return content.ImageBlock(content.Source{URL: u}), true
}

// output converts a call's output to a tool result, an error when the
// output records a failure.
func (cv *converter) output(it item) llm.Message {
	out := tool.Output{Content: []content.Block{}}
	raw := it.Output
	if it.Type == "tool_search_output" && (len(raw) == 0 || string(raw) == "null") {
		raw = nil
		if len(it.Tools) > 0 {
			b, _ := json.Marshal(string(it.Tools))
			raw = b
		}
	}
	var s string
	if len(raw) > 0 && json.Unmarshal(raw, &s) == nil {
		out.Content = append(out.Content, content.TextBlock(s))
	} else {
		for _, p := range cv.parts(raw) {
			out.Content = append(out.Content, *p.Block)
		}
	}
	return llm.Message{Role: llm.RoleUser, Content: []llm.Content{{
		Kind:       llm.ContentToolResult,
		ToolResult: &llm.ToolResult{CallID: it.CallID, Output: out, IsError: isToolError(it.Output)},
	}}}
}
