package codex

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
	"github.com/deepnoodle-ai/dashi/llm"
)

// guardian is source.subagent.other for an approval reviewer.
const guardian = "guardian"

// Parse reads every current copy of one session and returns each copy's
// facts. A model call with a response id is counted once, on the copy of
// the earliest-started agent that has it. The error is only for a copy
// that cannot be read.
func Parse(copies []sessionfile.Copy) ([]sessionfile.ParsedCopy, error) {
	cls, err := readSession(copies)
	if err != nil {
		return nil, err
	}
	subagents := map[string]bool{} // thread ids of subagent copies
	for _, cl := range cls {
		if cl.isSubagent() {
			subagents[cl.meta.ID] = true
		}
	}
	spawns := spawnCalls(cls)
	counted := map[string]bool{}
	out := make([]sessionfile.ParsedCopy, 0, len(cls))
	for _, cl := range cls {
		p := sessionfile.ParsedCopy{CopyID: cl.ID, SkippedLines: cl.skipped}
		p.Session = sessionFacts(cl)
		if cl.isSubagent() {
			if parent := cl.parentThreadID(); subagents[parent] {
				p.Session.ParentAgentID = parent
			}
			p.Session.ParentToolCallID = spawns[cl.meta.ID]
			if p.Session.ParentToolCallID == "" {
				p.Session.ParentToolCallID = spawns[cl.agentPath()]
			}
		}
		calls, source := modelCalls(cl)
		p.Session.UsageSource = source
		for _, mc := range calls {
			if mc.ResponseID != "" {
				if counted[mc.ResponseID] {
					continue
				}
				counted[mc.ResponseID] = true
			}
			p.Usage = append(p.Usage, sessionfile.Usage{
				Line: mc.Line, ModelCallID: mc.ResponseID, Model: mc.Model, Usage: mc.Usage,
			})
		}
		out = append(out, p)
	}
	return out, nil
}

func sessionFacts(cl *copyLines) sessionfile.Session {
	s := sessionfile.Session{AgentKind: sessionfile.AgentKindRoot, Entrypoint: cl.meta.Originator}
	switch sub := cl.source.Subagent; {
	case sub == nil:
		s.ForkedFromSessionID = cl.meta.ForkedFromID
	case sub.Other == guardian:
		s.AgentKind = sessionfile.AgentKindApprovalReviewer
	case cl.isFork():
		s.AgentKind = sessionfile.AgentKindFork
	default:
		s.AgentKind = sessionfile.AgentKindSubagent
	}
	if g := cl.meta.Git; g != nil {
		s.GitBranch, s.GitRemoteURL = g.Branch, g.RepositoryURL
	}
	userItems, userMessages := 0, 0
	hasUserItems := false
	for _, l := range cl.Lines {
		if l.Timestamp != "" {
			s.EndedAt = l.Timestamp
		}
		if l.Type == "event_msg" && l.item.Type == "item_completed" && l.item.Item != nil && l.item.Item.Type == "UserMessage" {
			hasUserItems = true
			if !l.inherited {
				userItems++
			}
		}
		if l.inherited {
			continue
		}
		switch {
		case l.Type == "compacted":
			s.Compactions++
		case l.Type != "response_item":
		case isCall(l.item.Type) && l.item.Type != "web_search_call":
			s.ToolCalls++
		case isOutput(l.item.Type):
			if isToolError(l.item.Output) {
				s.ToolErrors++
			}
		case l.item.Type == "message" && l.item.Role == "user" && isPrompt(l.item.Content):
			userMessages++
		}
	}
	s.UserPrompts = userMessages
	if hasUserItems {
		s.UserPrompts = userItems
	}
	return s
}

// injected matches the start of user text Codex adds itself, such as
// <environment_context> or AGENTS.md instructions.
var injected = regexp.MustCompile(`^(<[a-z_]+>|# AGENTS\.md instructions)`)

// isPrompt reports whether a user message holds text that Codex did not
// inject.
func isPrompt(raw json.RawMessage) bool {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return false
	}
	for _, p := range parts {
		t := strings.TrimSpace(p.Text)
		if p.Type == "input_text" && t != "" && !injected.MatchString(t) {
			return true
		}
	}
	return false
}

var (
	exitCodeText = regexp.MustCompile(`(?m)^(?:Process exited with code|Exit code:) (-?\d+)`)
	// exitCodeKey matches an exit_code key in JSON text, but not one
	// escaped inside a JSON string.
	exitCodeKey = regexp.MustCompile(`(?:^|[^\\])"exit_code":\s*(-?\d+)`)
)

// isToolError reports whether a tool output records a failure. A code
// mode script's output starts "Script completed" or "Script failed" and
// holds what the script printed, often exec results with exit_code keys,
// several when it ran commands in parallel; any nonzero one is an error.
// Other outputs have one exit code: "Exit code: N" or "Process exited with
// code N" in the text, or metadata.exit_code in JSON text.
func isToolError(raw json.RawMessage) bool {
	text := outputText(raw)
	switch {
	case strings.HasPrefix(text, "Script failed"), strings.HasPrefix(text, "exec_command failed"):
		return true
	case strings.HasPrefix(text, "Script completed"):
		for _, m := range exitCodeKey.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n != 0 {
				return true
			}
		}
		return false
	}
	code, ok := exitCode(text)
	return ok && code != 0
}

// exitCode reads the exit code in a tool output's text.
func exitCode(text string) (int, bool) {
	if strings.HasPrefix(text, "{") {
		var j struct {
			Metadata *struct {
				ExitCode *int `json:"exit_code"`
			} `json:"metadata"`
		}
		if json.Unmarshal([]byte(text), &j) == nil && j.Metadata != nil && j.Metadata.ExitCode != nil {
			return *j.Metadata.ExitCode, true
		}
	}
	if m := exitCodeText.FindStringSubmatch(text); m != nil {
		n, err := strconv.Atoi(m[1])
		return n, err == nil
	}
	return 0, false
}

// outputText returns an output's text: the string, or its text parts
// joined.
func outputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "")
}

// spawnCalls maps a spawned agent's thread id (older output) or agent path
// (task_name) to the id of the call that started it.
func spawnCalls(cls []*copyLines) map[string]string {
	out := map[string]string{}
	for _, cl := range cls {
		for _, l := range cl.Lines {
			if l.inherited || l.Type != "response_item" || !isOutput(l.item.Type) || l.item.CallID == "" {
				continue
			}
			text := outputText(l.item.Output)
			if !strings.HasPrefix(text, "{") {
				continue
			}
			var j struct {
				AgentID  string `json:"agent_id"`
				TaskName string `json:"task_name"`
			}
			if json.Unmarshal([]byte(text), &j) != nil {
				continue
			}
			for _, k := range []string{j.AgentID, j.TaskName} {
				if k != "" && out[k] == "" {
					out[k] = l.item.CallID
				}
			}
		}
	}
	return out
}

// modelCall is the usage of one model call.
type modelCall struct {
	Line       int // the token line
	ResponseID string
	Model      string
	Usage      llm.Usage
}

// modelCalls returns a copy's model calls and where their usage came from:
// token_usage_record lines when the copy has any, else token_count lines
// whose total changed. Inherited lines are left out.
func modelCalls(cl *copyLines) ([]modelCall, string) {
	source := sessionfile.UsageSourceNone
	for _, l := range cl.Lines {
		if l.Type == "token_usage_record" {
			source = sessionfile.UsageSourceTokenUsageRecord
			break
		}
		if source == sessionfile.UsageSourceNone && l.Type == "event_msg" && l.item.Type == "token_count" &&
			l.item.Info != nil && l.item.Info.Last != nil {
			source = sessionfile.UsageSourceTokenCount
		}
	}
	var out []modelCall
	model := ""
	searches := 0
	var lastTotal *tokenUsage
	for _, l := range cl.Lines {
		switch {
		case l.Type == "turn_context" && l.item.Model != "":
			model = l.item.Model
		case l.Type == "event_msg" && l.item.ThreadSettings != nil && l.item.ThreadSettings.Model != "" && model == "":
			model = l.item.ThreadSettings.Model
		case l.Type == "response_item" && l.item.Type == "web_search_call" && !l.inherited:
			searches++
		}
		if l.inherited {
			continue
		}
		var u *tokenUsage
		var id string
		switch {
		case source == sessionfile.UsageSourceTokenUsageRecord && l.Type == "token_usage_record":
			u, id = l.item.Usage, l.item.ResponseID
		case source == sessionfile.UsageSourceTokenCount && l.Type == "event_msg" && l.item.Type == "token_count" &&
			l.item.Info != nil && l.item.Info.Last != nil:
			if lastTotal != nil && l.item.Info.Total != nil && *lastTotal == *l.item.Info.Total {
				continue // a repeat
			}
			lastTotal = l.item.Info.Total
			u = l.item.Info.Last
		}
		if u == nil {
			continue
		}
		mc := modelCall{Line: l.N, ResponseID: id, Model: model, Usage: toUsage(*u, searches)}
		searches = 0
		out = append(out, mc)
	}
	// A token line can come before any line names the model; it gets the
	// first model named later.
	first := firstModel(cl)
	for i := range out {
		if out[i].Model != "" {
			break
		}
		out[i].Model = first
	}
	return out, source
}

// firstModel returns the first model a copy names, in turn_context or
// thread_settings_applied.
func firstModel(cl *copyLines) string {
	for _, l := range cl.Lines {
		switch {
		case l.Type == "turn_context" && l.item.Model != "":
			return l.item.Model
		case l.Type == "event_msg" && l.item.ThreadSettings != nil && l.item.ThreadSettings.Model != "":
			return l.item.ThreadSettings.Model
		}
	}
	return ""
}

// toUsage maps Codex token counts to llm.Usage. Codex reports cached input
// inside input_tokens and never reports cache writes.
func toUsage(u tokenUsage, searches int) llm.Usage {
	x := llm.Usage{
		InputTokens:                         max(0, u.InputTokens-u.CachedInputTokens),
		CacheReadInputTokens:                u.CachedInputTokens,
		OutputTokens:                        u.OutputTokens,
		ReasoningTokens:                     u.ReasoningOutputTokens,
		CacheCreationInputTokensUnavailable: true,
	}
	if searches > 0 {
		x.Requests = map[string]int{llm.MeterWebSearch: searches}
	}
	return x
}
