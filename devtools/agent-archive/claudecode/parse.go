package claudecode

import (
	"maps"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Parse reads every current copy of one session and returns each copy's
// facts. A model call is counted once, on the copy of the earliest-started
// agent that has it. The error is only for a copy that cannot be read.
func Parse(copies []sessionfile.Copy) ([]sessionfile.ParsedCopy, error) {
	cls, err := readSession(copies)
	if err != nil {
		return nil, err
	}
	inherited := inheritedUUIDs(cls)
	counted := map[string]bool{} // model calls already given a usage row
	out := make([]sessionfile.ParsedCopy, 0, len(cls))
	for i, cl := range cls {
		p := sessionfile.ParsedCopy{CopyID: cl.ID, SkippedLines: cl.skipped}
		p.Session, p.AppUsage = sessionFacts(cl, inherited[i])
		p.Usage = usageRows(cl, counted)
		p.Session.UsageSource = sessionfile.UsageSourceNone
		if hasUsage(cl) {
			p.Session.UsageSource = sessionfile.UsageSourceMessageUsage
		}
		out = append(out, p)
	}
	return out, nil
}

// sessionFacts returns a copy's sessions columns and, for the root, Claude
// Code's own per-model totals from its last cost-state line. Those include
// calls it never writes to a session file, such as compaction and the
// helpers behind web fetch and web search.
func sessionFacts(cl *copyLines, inherited map[string]bool) (sessionfile.Session, []sessionfile.AppUsage) {
	s := sessionfile.Session{AgentKind: sessionfile.AgentKindRoot}
	var modelUsage map[string]appModelUsage
	if cl.AgentID != "" {
		s.AgentKind = sessionfile.AgentKindSubagent
		if cl.meta.IsFork {
			s.AgentKind = sessionfile.AgentKindFork
		}
		s.ParentAgentID = cl.meta.ParentAgentID
		s.ParentToolCallID = cl.meta.ToolUseID
		s.Title = cl.meta.Description
	}
	var customTitle, aiTitle string
	for _, l := range cl.Lines {
		if l.Timestamp != "" {
			s.EndedAt = l.Timestamp
		}
		if s.Entrypoint == "" {
			s.Entrypoint = l.Entrypoint
		}
		if s.GitBranch == "" {
			s.GitBranch = l.GitBranch
		}
		switch l.Type {
		case "custom-title":
			customTitle = l.CustomTitle
		case "ai-title":
			aiTitle = l.AITitle
		case "pr-link":
			if u := repositoryURL(l.PRURL); u != "" {
				s.GitRemoteURL = u
			}
		case "cost-state":
			if cl.AgentID == "" && l.TotalCostUSD != nil {
				v := *l.TotalCostUSD
				s.AppCostUSD = &v
			}
			if cl.AgentID == "" && l.ModelUsage != nil {
				modelUsage = l.ModelUsage
			}
		}
		if l.skip == sessionfile.SkipDuplicateLine || l.skip == sessionfile.SkipBadLine || inherited[l.UUID] {
			continue
		}
		if l.Type == "system" && l.Subtype == "compact_boundary" {
			s.Compactions++
		}
		if l.Message == nil {
			continue
		}
		for _, b := range blocks(l.Message.Content) {
			switch {
			case l.Type == "assistant" && b.Type == "tool_use":
				s.ToolCalls++
			case l.Type == "user" && b.Type == "tool_result" && b.IsError:
				s.ToolErrors++
			}
		}
		if l.Type == "user" && cl.final[l] && isPrompt(l) {
			s.UserPrompts++
		}
	}
	if cl.AgentID == "" {
		s.Title = customTitle
		if s.Title == "" {
			s.Title = aiTitle
		}
	}
	var apps []sessionfile.AppUsage
	for _, model := range slices.Sorted(maps.Keys(modelUsage)) {
		u := modelUsage[model]
		x := llm.Usage{
			InputTokens:              u.InputTokens,
			CacheReadInputTokens:     u.CacheReadInputTokens,
			CacheCreationInputTokens: u.CacheCreationInputTokens,
			OutputTokens:             u.OutputTokens,
			ReasoningTokens:          u.ThinkingTokens,
		}
		if u.WebSearchRequests > 0 {
			x.Requests = map[string]int{llm.MeterWebSearch: u.WebSearchRequests}
		}
		apps = append(apps, sessionfile.AppUsage{Model: model, Usage: x, CostUSD: u.CostUSD})
	}
	return s, apps
}

// notPrompts are the starts of user text Claude Code writes itself:
// slash-command wrappers, local command output, interrupt markers, and
// background task notifications.
var notPrompts = []string{
	"<command-name>", "<command-message>", "<command-args>",
	"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>",
	"<bash-input>", "<bash-stdout>", "<bash-stderr>",
	"<task-notification>", "[Request interrupted by user",
}

// isPrompt reports whether a user line is text the person typed.
func isPrompt(l *line) bool {
	if l.IsMeta || l.IsCompactSummary {
		return false
	}
	if l.Origin != nil && l.Origin.Kind != "" && l.Origin.Kind != "human" {
		return false
	}
	bs := blocks(l.Message.Content)
	text := ""
	for _, b := range bs {
		switch b.Type {
		case "tool_result":
			return false
		case "text":
			if text == "" {
				text = strings.TrimSpace(b.Text)
			}
		}
	}
	if text == "" {
		return false
	}
	for _, p := range notPrompts {
		if strings.HasPrefix(text, p) {
			return false
		}
	}
	return true
}

// repositoryURL turns a pull request URL into its repository's URL.
func repositoryURL(prURL string) string {
	if i := strings.Index(prURL, "/pull/"); i > 0 {
		return prURL[:i]
	}
	return ""
}

func hasUsage(cl *copyLines) bool {
	for _, l := range cl.Lines {
		if l.Type == "assistant" && l.skip != sessionfile.SkipAPIError && l.Message != nil && l.Message.Usage != nil {
			return true
		}
	}
	return false
}

// modelCall is the lines of one model call in one copy, merged.
type modelCall struct {
	ID    string
	Lines []*line
	Model string // served
	Usage llm.Usage
	Speed string
}

// modelCalls groups a copy's assistant lines by message.id, in order of
// their first line. API error lines are left out.
func modelCalls(cl *copyLines) []*modelCall {
	var out []*modelCall
	byID := map[string]*modelCall{}
	for _, l := range cl.Lines {
		if l.Type != "assistant" || l.Message == nil || l.skip == sessionfile.SkipAPIError ||
			l.skip == sessionfile.SkipDuplicateLine || l.skip == sessionfile.SkipBadLine {
			continue
		}
		id := l.Message.ID
		mc := byID[id]
		if mc == nil || id == "" {
			mc = &modelCall{ID: id}
			out = append(out, mc)
			if id != "" {
				byID[id] = mc
			}
		}
		mc.Lines = append(mc.Lines, l)
		mergeUsage(mc, l.Message)
	}
	return out
}

// mergeUsage folds one line of a model call into it. Token counts of a
// split call can grow, stay, or drop between lines, so each takes its
// maximum. The served model is the last iteration's model that differs
// from message.model, else message.model.
func mergeUsage(mc *modelCall, m *message) {
	served := m.Model
	if u := m.Usage; u != nil {
		for _, it := range u.Iterations {
			if it.Model != "" {
				served = it.Model
			}
		}
		x := &mc.Usage
		x.InputTokens = max(x.InputTokens, u.InputTokens)
		x.OutputTokens = max(x.OutputTokens, u.OutputTokens)
		x.CacheCreationInputTokens = max(x.CacheCreationInputTokens, u.CacheCreationInputTokens)
		x.CacheReadInputTokens = max(x.CacheReadInputTokens, u.CacheReadInputTokens)
		if u.CacheCreation != nil {
			x.CacheCreation1hInputTokens = max(x.CacheCreation1hInputTokens, u.CacheCreation.Ephemeral1h)
		}
		if u.OutputTokensDetails != nil {
			x.ReasoningTokens = max(x.ReasoningTokens, u.OutputTokensDetails.ThinkingTokens)
		}
		if u.ServerToolUse != nil && u.ServerToolUse.WebSearchRequests > x.Requests[llm.MeterWebSearch] {
			if x.Requests == nil {
				x.Requests = map[string]int{}
			}
			x.Requests[llm.MeterWebSearch] = u.ServerToolUse.WebSearchRequests
		}
		if u.Speed != "" {
			mc.Speed = u.Speed
		}
	}
	if served != "" {
		mc.Model = served
	}
}

// usageRows returns one row per model call of the copy not already counted
// on an earlier copy, and marks them counted.
func usageRows(cl *copyLines, counted map[string]bool) []sessionfile.Usage {
	var out []sessionfile.Usage
	for _, mc := range cl.calls {
		if !claimUsage(mc, counted) {
			continue
		}
		out = append(out, sessionfile.Usage{
			Line: mc.Lines[0].N, ModelCallID: mc.ID, Model: mc.Model, Usage: mc.Usage, Speed: mc.Speed,
			CalledAt: sessionfile.CheckTimestamp(mc.Lines[0].Timestamp),
		})
	}
	return out
}

// claimUsage reports whether a copy owns the usage of mc: mc has usage
// and no earlier copy owns it. A call with an id is marked counted. Parse
// and Convert both use it, so they give usage to the same copy.
func claimUsage(mc *modelCall, counted map[string]bool) bool {
	if !hasLineUsage(mc) {
		return false
	}
	if mc.ID != "" {
		if counted[mc.ID] {
			return false
		}
		counted[mc.ID] = true
	}
	return true
}

func hasLineUsage(mc *modelCall) bool {
	for _, l := range mc.Lines {
		if l.Message.Usage != nil {
			return true
		}
	}
	return false
}
