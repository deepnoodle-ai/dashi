// Package codex parses Codex session files and converts an agent's lines
// to Dashi messages. See design/agent-archive-parse.md.
package codex

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
)

// line is one line of a Codex session file.
type line struct {
	N         int             // 1-based line number
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Ordinal   *int            `json:"ordinal"`
	Payload   json.RawMessage `json:"payload"`

	item item // the payload's common fields
	// inherited marks a line of a fork's prefix, repeated from its parent.
	inherited bool
	skip      string
}

// item holds the payload fields parsing and conversion read, for every
// line type.
type item struct {
	Type string `json:"type"`
	// message, agent_message
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Author  string          `json:"author"`
	// reasoning
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
	EncryptedContent string `json:"encrypted_content"`
	// *_call, *_output
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	Action    json.RawMessage `json:"action"`
	Status    string          `json:"status"`
	Output    json.RawMessage `json:"output"`
	Tools     json.RawMessage `json:"tools"`
	// compacted
	Message string `json:"message"`
	// turn_context, thread_settings_applied
	Model          string `json:"model"`
	ThreadSettings *struct {
		Model string `json:"model"`
	} `json:"thread_settings"`
	// token_usage_record
	ResponseID string      `json:"response_id"`
	Usage      *tokenUsage `json:"usage"`
	// token_count
	Info *struct {
		Total *tokenUsage `json:"total_token_usage"`
		Last  *tokenUsage `json:"last_token_usage"`
	} `json:"info"`
	// item_completed
	Item *struct {
		Type string `json:"type"`
	} `json:"item"`
}

type tokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// sessionMeta is the payload of a file's own session_meta line, its first.
type sessionMeta struct {
	ID             string `json:"id"`
	ForkedFromID   string `json:"forked_from_id"`
	ParentThreadID string `json:"parent_thread_id"`
	Originator     string `json:"originator"`
	AgentPath      string `json:"agent_path"`
	StartOrdinal   *int   `json:"subagent_history_start_ordinal"`
	Source         json.RawMessage
	Git            *struct {
		Branch        string `json:"branch"`
		RepositoryURL string `json:"repository_url"`
	} `json:"git"`
}

type subagentSource struct {
	Subagent *struct {
		Other       string `json:"other"`
		ThreadSpawn *struct {
			ParentThreadID string `json:"parent_thread_id"`
			AgentPath      string `json:"agent_path"`
		} `json:"thread_spawn"`
	} `json:"subagent"`
}

// Line types that hold no conversation. realtime_item marks a voice
// session starting and closing, without usage.
var ignoredTypes = map[string]bool{
	"session_meta": true, "event_msg": true, "turn_context": true,
	"world_state": true, "inter_agent_communication_metadata": true,
	"token_usage_record": true, "realtime_item": true,
}

// Response item types conversion knows. compaction is known but dropped.
var knownItems = map[string]bool{
	"message": true, "reasoning": true, "agent_message": true, "compaction": true,
	"function_call": true, "custom_tool_call": true, "tool_search_call": true, "web_search_call": true,
	"function_call_output": true, "custom_tool_call_output": true, "tool_search_output": true,
}

// copyLines is one copy read and classified.
type copyLines struct {
	sessionfile.Copy
	Lines   []*line
	meta    sessionMeta
	source  subagentSource
	skipped sessionfile.Counts
}

// readCopy reads every line of a copy and marks a fork's inherited prefix.
func readCopy(c sessionfile.Copy) (*copyLines, error) {
	cl := &copyLines{Copy: c, skipped: sessionfile.Counts{}}
	seenMeta := false
	err := sessionfile.ReadCopy(c, func(n int, b []byte) {
		if len(strings.TrimSpace(string(b))) == 0 {
			return
		}
		l := &line{N: n}
		if json.Unmarshal(b, l) != nil {
			l.skip = sessionfile.SkipBadLine
			cl.Lines = append(cl.Lines, l)
			return
		}
		if len(l.Payload) > 0 && l.Payload[0] == '{' {
			_ = json.Unmarshal(l.Payload, &l.item)
		}
		switch {
		case l.Type == "":
			l.skip = sessionfile.SkipBadLine
		case l.Type == "session_meta" && !seenMeta:
			seenMeta = true
			_ = json.Unmarshal(l.Payload, &cl.meta)
			var raw struct {
				Source json.RawMessage `json:"source"`
			}
			_ = json.Unmarshal(l.Payload, &raw)
			cl.meta.Source = raw.Source
			_ = json.Unmarshal(raw.Source, &cl.source)
			l.skip = sessionfile.SkipIgnored + l.Type
		case ignoredTypes[l.Type]:
			l.skip = sessionfile.SkipIgnored + l.Type
		case l.Type == "compacted":
		case l.Type == "response_item":
			if !knownItems[l.item.Type] {
				l.skip = sessionfile.SkipUnknown + l.item.Type
			}
		default:
			l.skip = sessionfile.SkipUnknown + l.Type
		}
		cl.Lines = append(cl.Lines, l)
	})
	if err != nil {
		return nil, err
	}
	// A fork records its parent's line count as its start ordinal; lines
	// below it repeat the parent's.
	if cl.hasStartOrdinal() {
		for _, l := range cl.Lines {
			if l.Ordinal != nil && *l.Ordinal < *cl.meta.StartOrdinal && l.N > 1 {
				l.inherited = true
			}
		}
	}
	for _, l := range cl.Lines {
		if l.skip != "" {
			cl.skipped.Add(l.skip)
		}
	}
	return cl, nil
}

// hasStartOrdinal reports whether the file is a subagent fork with a usable
// start ordinal. A start of zero, absent, or not below the file's own line
// count marks no prefix.
func (cl *copyLines) hasStartOrdinal() bool {
	start := cl.meta.StartOrdinal
	return start != nil && cl.isFork() && *start > 0 && *start < len(cl.Lines)
}

// tokenKey identifies a token line across files: a token_usage_record's
// response id, or a token_count's cumulative total. The zero key is not a
// token line.
type tokenKey struct {
	responseID string
	total      tokenUsage
}

func (l *line) tokenKey() tokenKey {
	switch {
	case l.Type == "token_usage_record" && l.item.ResponseID != "":
		return tokenKey{responseID: l.item.ResponseID}
	case l.Type == "event_msg" && l.item.Type == "token_count" && l.item.Info != nil && l.item.Info.Total != nil:
		return tokenKey{total: *l.item.Info.Total}
	}
	return tokenKey{}
}

// markInherited marks the lines a fork repeats from its parent when its
// file gives no usable start ordinal: a subagent fork from Codex 0.140 to
// 0.148, or a forked root. The repeat is the fork's token lines, from its
// first, that follow the parent's token lines in order with none skipped;
// the first that does not ends it. It runs through the turn of the last
// one, up to the next task_started. A token line that repeats the one
// before it is not matched: a fork's first own turn can start with one.
// all is every copy read, including fork parents.
func markInherited(cl *copyLines, all []*copyLines) {
	if cl.meta.ForkedFromID == "" || cl.hasStartOrdinal() {
		return
	}
	last := -1
	for _, p := range all {
		if p != cl && p.meta.ID == cl.meta.ForkedFromID {
			last = max(last, copiedPrefixEnd(cl, p))
		}
	}
	if last < 0 {
		return
	}
	for i, l := range cl.Lines {
		if i > last && l.Type == "event_msg" && l.item.Type == "task_started" {
			break
		}
		if l.N > 1 {
			l.inherited = true
		}
	}
}

// copiedPrefixEnd returns the index of the fork's last token line in its
// run that repeats the parent's token lines in order, or -1 when the fork's
// first token line is not one of the parent's.
func copiedPrefixEnd(fork, parent *copyLines) int {
	var keys []tokenKey
	for _, l := range parent.Lines {
		if k := l.tokenKey(); k != (tokenKey{}) && (len(keys) == 0 || k != keys[len(keys)-1]) {
			keys = append(keys, k)
		}
	}
	last, next := -1, 0 // next is the index of the parent key to match
	var prev tokenKey
	for i, l := range fork.Lines {
		k := l.tokenKey()
		if k == (tokenKey{}) || k == prev {
			continue
		}
		prev = k
		j := next
		if last < 0 {
			j = slices.Index(keys, k)
		}
		if j < 0 || j >= len(keys) || keys[j] != k {
			return last
		}
		last, next = i, j+1
	}
	return last
}

// isSubagent reports whether the file is a subagent's.
func (cl *copyLines) isSubagent() bool { return cl.source.Subagent != nil }

// isFork reports whether the file is a subagent forked from its parent.
func (cl *copyLines) isFork() bool { return cl.isSubagent() && cl.meta.ForkedFromID != "" }

// parentThreadID is the thread that started a subagent.
func (cl *copyLines) parentThreadID() string {
	if s := cl.source.Subagent; s != nil && s.ThreadSpawn != nil && s.ThreadSpawn.ParentThreadID != "" {
		return s.ThreadSpawn.ParentThreadID
	}
	return cl.meta.ParentThreadID
}

func (cl *copyLines) agentPath() string {
	if s := cl.source.Subagent; s != nil && s.ThreadSpawn != nil && s.ThreadSpawn.AgentPath != "" {
		return s.ThreadSpawn.AgentPath
	}
	return cl.meta.AgentPath
}

// readSession reads every copy of a session, oldest first, and marks each
// fork's repeated lines. Fork parents are read for that, then left out.
func readSession(copies []sessionfile.Copy) ([]*copyLines, error) {
	sorted := slices.Clone(copies)
	slices.SortStableFunc(sorted, func(a, b sessionfile.Copy) int {
		switch {
		case a.StartedAt < b.StartedAt:
			return -1
		case a.StartedAt > b.StartedAt:
			return 1
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	all := make([]*copyLines, 0, len(sorted))
	for _, c := range sorted {
		cl, err := readCopy(c)
		if err != nil {
			return nil, err
		}
		all = append(all, cl)
	}
	out := make([]*copyLines, 0, len(all))
	for _, cl := range all {
		if !cl.ForkParent {
			markInherited(cl, all)
			out = append(out, cl)
		}
	}
	return out, nil
}

// isCall reports whether a response item type is a call the model made.
func isCall(t string) bool { return strings.HasSuffix(t, "_call") }

// isOutput reports whether a response item type is a call's output.
func isOutput(t string) bool { return strings.HasSuffix(t, "_output") }
