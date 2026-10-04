// Package claudecode parses Claude Code session files and converts an
// agent's lines to Dashi messages. See design/agent-archive-parse.md.
package claudecode

import (
	"encoding/json"
	"slices"

	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
)

// line is one line of a Claude Code session file, with the fields parsing
// and conversion read.
type line struct {
	N    int    // 1-based line number
	Type string `json:"type"`

	UUID              string `json:"uuid"`
	ParentUUID        string `json:"parentUuid"`
	LogicalParentUUID string `json:"logicalParentUuid"`
	IsSidechain       bool   `json:"isSidechain"`
	IsMeta            bool   `json:"isMeta"`
	IsCompactSummary  bool   `json:"isCompactSummary"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Subtype           string `json:"subtype"`
	Timestamp         string `json:"timestamp"`
	Entrypoint        string `json:"entrypoint"`
	GitBranch         string `json:"gitBranch"`
	Origin            *struct {
		Kind string `json:"kind"`
	} `json:"origin"`

	Message    *message    `json:"message"`
	Attachment *attachment `json:"attachment"`
	Rendered   []struct {
		Content string `json:"content"`
	} `json:"rendered"`

	// Metadata lines.
	CustomTitle    string   `json:"customTitle"`
	AITitle        string   `json:"aiTitle"`
	PRURL          string   `json:"prUrl"`
	TotalCostUSD   *float64 `json:"totalCostUSD"`
	ParentLastUUID string   `json:"parentLastUuid"`

	// skip is why the line is not converted; empty if it is.
	skip string
}

type message struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *usage          `json:"usage"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreation            *struct {
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	OutputTokensDetails *struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
	ServerToolUse *struct {
		WebSearchRequests int `json:"web_search_requests"`
	} `json:"server_tool_use"`
	Speed      string `json:"speed"`
	Iterations []struct {
		Model string `json:"model"`
	} `json:"iterations"`
}

type attachment struct {
	Type string `json:"type"`
}

// block is one content block of a user or assistant message.
type block struct {
	Type string `json:"type"`
	// text
	Text string `json:"text"`
	// thinking
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	// image, document
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
	Title string `json:"title"`
}

// Line types that hold no conversation. Any other type that is not user,
// assistant, or attachment is unknown.
var ignoredTypes = map[string]bool{
	"system": true, "summary": true, "progress": true,
	"last-prompt": true, "atis-latch": true, "mode": true, "ai-title": true,
	"permission-mode": true, "pr-link": true, "bridge-session": true,
	"queue-operation": true, "file-history-snapshot": true,
	"file-history-delta": true, "worktree-state": true, "frame-link": true,
	"cost-state": true, "custom-title": true, "relocated": true,
	"agent-name": true, "dev-mods": true, "artifact-comment-monitor": true,
	"artifact-autoreact-ledger": true, "fork-context-ref": true,
}

// syntheticModel is the model Claude Code records on its own error lines.
const syntheticModel = "<synthetic>"

// copyLines is one copy read and classified.
type copyLines struct {
	sessionfile.Copy
	Lines []*line
	// byUUID maps a uuid to its first line.
	byUUID map[string]*line
	// final is the set of lines on the final path.
	final map[*line]bool
	meta  meta
	// skipped counts lines not converted, by reason.
	skipped sessionfile.Counts
}

// meta is a subagent's .meta.json.
type meta struct {
	AgentType     string `json:"agentType"`
	Description   string `json:"description"`
	ToolUseID     string `json:"toolUseId"`
	ParentAgentID string `json:"parentAgentId"`
	IsFork        bool   `json:"isFork"`
}

// readCopy reads every line of a copy, marks duplicates, error lines,
// and lines that hold no conversation, and finds the final path.
func readCopy(c sessionfile.Copy) (*copyLines, error) {
	cl := &copyLines{Copy: c, byUUID: map[string]*line{}, skipped: sessionfile.Counts{}}
	if len(c.Meta) > 0 {
		_ = json.Unmarshal(c.Meta, &cl.meta) // a bad meta file leaves the facts empty
	}
	err := sessionfile.ReadCopy(c, func(n int, b []byte) {
		if len(trimSpace(b)) == 0 {
			return
		}
		l := &line{N: n}
		if json.Unmarshal(b, l) != nil {
			l.skip = sessionfile.SkipBadLine
			cl.Lines = append(cl.Lines, l)
			return
		}
		switch {
		case l.UUID != "" && cl.byUUID[l.UUID] != nil:
			l.skip = sessionfile.SkipDuplicateLine
		case l.Type == "user" || l.Type == "assistant":
			if l.Message == nil {
				l.skip = sessionfile.SkipBadLine
			} else if l.Type == "assistant" && (l.IsAPIErrorMessage || l.Message.Model == syntheticModel) {
				l.skip = sessionfile.SkipAPIError
			}
		case l.Type == "attachment":
			if len(l.Rendered) == 0 {
				l.skip = sessionfile.SkipIgnored + l.Type
			}
		case l.Type == "":
			l.skip = sessionfile.SkipBadLine
		case ignoredTypes[l.Type]:
			l.skip = sessionfile.SkipIgnored + l.Type
		default:
			l.skip = sessionfile.SkipUnknown + l.Type
		}
		if l.UUID != "" && cl.byUUID[l.UUID] == nil {
			cl.byUUID[l.UUID] = l
		}
		cl.Lines = append(cl.Lines, l)
	})
	if err != nil {
		return nil, err
	}
	cl.final = finalPath(cl, lastLine(cl))
	for _, l := range cl.Lines {
		if l.skip == "" && !cl.final[l] {
			l.skip = sessionfile.SkipRewound
		}
		if l.skip != "" {
			cl.skipped.Add(l.skip)
		}
	}
	return cl, nil
}

// lastLine is the line the conversation ended on: the last line in the
// uuid tree, skipping sidechain lines in a root file.
func lastLine(cl *copyLines) *line {
	for _, l := range slices.Backward(cl.Lines) {
		if l.UUID == "" || l.skip == sessionfile.SkipDuplicateLine || l.skip == sessionfile.SkipBadLine {
			continue
		}
		if cl.AgentID == "" && l.IsSidechain {
			continue
		}
		return l
	}
	return nil
}

// finalPath returns the lines a conversation ending at last is made of:
// the walk up parentUuid (crossing a compact_boundary by
// logicalParentUuid), plus every line of its model calls and every tool
// result answering its calls, until none are added.
func finalPath(cl *copyLines, last *line) map[*line]bool {
	path := map[*line]bool{}
	for l := last; l != nil && !path[l]; {
		path[l] = true
		parent := l.ParentUUID
		if parent == "" {
			parent = l.LogicalParentUUID
		}
		l = cl.byUUID[parent]
	}
	if len(path) == 0 {
		return path
	}
	// Index model calls and tool results once.
	byCall := map[string][]*line{}
	byResult := map[string][]*line{}
	for _, l := range cl.Lines {
		if l.Message == nil || l.skip == sessionfile.SkipDuplicateLine || l.skip == sessionfile.SkipBadLine {
			continue
		}
		if l.Type == "assistant" && l.Message.ID != "" {
			byCall[l.Message.ID] = append(byCall[l.Message.ID], l)
		}
		if l.Type == "user" {
			for _, b := range blocks(l.Message.Content) {
				if b.Type == "tool_result" && b.ToolUseID != "" {
					byResult[b.ToolUseID] = append(byResult[b.ToolUseID], l)
				}
			}
		}
	}
	queue := make([]*line, 0, len(path))
	for l := range path {
		queue = append(queue, l)
	}
	add := func(ls []*line) {
		for _, x := range ls {
			if !path[x] {
				path[x] = true
				queue = append(queue, x)
			}
		}
	}
	for len(queue) > 0 {
		l := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if l.Type != "assistant" || l.Message == nil {
			continue
		}
		if l.Message.ID != "" {
			add(byCall[l.Message.ID])
		}
		for _, b := range blocks(l.Message.Content) {
			if b.Type == "tool_use" && b.ID != "" {
				add(byResult[b.ID])
			}
		}
	}
	return path
}

// blocks decodes message content: a string is one text block.
func blocks(raw json.RawMessage) []block {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		return []block{{Type: "text", Text: s}}
	}
	var bs []block
	if json.Unmarshal(raw, &bs) != nil {
		return nil
	}
	return bs
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

// readSession reads every copy of a session, oldest agent first.
func readSession(copies []sessionfile.Copy) ([]*copyLines, error) {
	sorted := slices.Clone(copies)
	slices.SortStableFunc(sorted, compareStart)
	out := make([]*copyLines, 0, len(sorted))
	for _, c := range sorted {
		cl, err := readCopy(c)
		if err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, nil
}

// compareStart orders copies by start time, the root first on a tie, then
// by copy id.
func compareStart(a, b sessionfile.Copy) int {
	switch {
	case a.StartedAt != b.StartedAt && a.StartedAt != "" && b.StartedAt != "":
		if a.StartedAt < b.StartedAt {
			return -1
		}
		return 1
	case (a.AgentID == "") != (b.AgentID == ""):
		if a.AgentID == "" {
			return -1
		}
		return 1
	case a.ID < b.ID:
		return -1
	case a.ID > b.ID:
		return 1
	}
	return 0
}

// inheritedUUIDs returns, for each copy, the uuids of its lines seen in a
// copy of an agent that started earlier. copies are in start order.
func inheritedUUIDs(copies []*copyLines) []map[string]bool {
	seen := map[string]string{} // uuid -> agent that has it first
	out := make([]map[string]bool, len(copies))
	for i, cl := range copies {
		out[i] = map[string]bool{}
		for u := range cl.byUUID {
			if a, ok := seen[u]; ok && a != cl.AgentID {
				out[i][u] = true
			}
		}
		for u := range cl.byUUID {
			if _, ok := seen[u]; !ok {
				seen[u] = cl.AgentID
			}
		}
	}
	return out
}
