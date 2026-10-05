// Package sessionfile holds the types the claudecode and codex packages
// share: the copies of one session they read, the facts parsing finds, and
// the transcript conversion returns. See design/agent-archive-parse.md.
package sessionfile

import (
	"bufio"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Copy is one current copy of a session file.
type Copy struct {
	// ID is the copy's copy_id.
	ID int64
	// Name is the copy's archive path, for messages.
	Name string
	// AgentID is the agent the copy holds, empty for the root.
	AgentID string
	// StartedAt is the copy's first timestamp (RFC 3339), which orders
	// agents by when they started.
	StartedAt string
	// Open returns the copy's uncompressed lines.
	Open func() (io.ReadCloser, error)
	// Meta is a Claude Code subagent's .meta.json, if archived.
	Meta []byte
	// ForkParent marks a Codex root copy of the session that a root of
	// this session was forked from. It is read only to find the lines the
	// fork repeats, and gets no ParsedCopy.
	ForkParent bool
}

// Agent kinds.
const (
	AgentKindRoot             = "root"
	AgentKindSubagent         = "subagent"
	AgentKindFork             = "fork"
	AgentKindApprovalReviewer = "approval_reviewer"
)

// Usage sources.
const (
	UsageSourceMessageUsage     = "message_usage"
	UsageSourceTokenUsageRecord = "token_usage_record"
	UsageSourceTokenCount       = "token_count"
	UsageSourceNone             = "none"
)

// Session is one copy's phase 2 columns of the sessions table.
type Session struct {
	AgentKind           string
	ParentAgentID       string
	ParentToolCallID    string
	ForkedFromSessionID string
	Entrypoint          string
	GitBranch           string
	GitRemoteURL        string
	Title               string
	EndedAt             string
	UserPrompts         int
	ToolCalls           int
	ToolErrors          int
	Compactions         int
	AppCostUSD          *float64
	UsageSource         string
}

// Usage is one row of the usage table: one model call.
type Usage struct {
	// Line is the 1-based line of the model call in its copy.
	Line        int
	ModelCallID string
	// Model is the served model.
	Model string
	Usage llm.Usage
	// Speed is Claude Code's usage.speed, such as "fast"; empty if unknown.
	Speed string
	// CalledAt is the timestamp of the row's line, as the app wrote it
	// (RFC 3339); empty when the line has none that parses.
	CalledAt string
}

// AppUsage is one row of the app_usage table: the app's own token totals
// and cost for one model over a whole session, which include model calls
// it never writes to a session file.
type AppUsage struct {
	Model   string
	Usage   llm.Usage
	CostUSD float64
}

// ParsedCopy is what parsing found in one copy.
type ParsedCopy struct {
	CopyID       int64
	Session      Session
	Usage        []Usage
	AppUsage     []AppUsage
	SkippedLines map[string]int
}

// CheckTimestamp returns timestamp when it parses as RFC 3339, else empty.
func CheckTimestamp(timestamp string) string {
	if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
		return ""
	}
	return timestamp
}

// LineRef says where a transcript message came from.
type LineRef struct {
	CopyID int64 `json:"copy_id"`
	// Lines are 1-based lines of the uncompressed copy; a model call spans
	// several.
	Lines []int `json:"lines"`
	// Inherited marks a message repeated from the agent this one forked
	// from.
	Inherited bool `json:"inherited,omitempty"`
	// CompactionSummary marks the app's summary at a compaction.
	CompactionSummary bool `json:"compaction_summary,omitempty"`
	// Model is the served model of an assistant message that is not
	// inherited.
	Model string     `json:"model,omitempty"`
	Usage *llm.Usage `json:"usage,omitempty"`
}

// Transcript is one agent's conversation as Dashi messages.
type Transcript struct {
	Messages []llm.Message
	// LineRefs[i] says where Messages[i] came from.
	LineRefs []LineRef
	// SkippedLines maps a reason to a count of lines not converted.
	SkippedLines map[string]int
	// ChangedBlocks maps a reason to a count of content blocks dropped or
	// changed.
	ChangedBlocks map[string]int
}

// Skipped-line reasons. Ignored and unknown lines add the line type.
const (
	SkipIgnored       = "ignored:"
	SkipUnknown       = "unknown:"
	SkipBadLine       = "bad_line"
	SkipRewound       = "rewound"
	SkipDuplicateLine = "duplicate_line"
	SkipAPIError      = "api_error"
)

// Changed-block reasons. Dropped blocks add the block type.
const (
	ChangeDropped        = "dropped:"
	ChangeReminderAsText = "reminder_as_text"
	ChangeMadeUpID       = "made_up_id"
)

// UnknownTypes returns the unknown line types in a skipped-lines report.
func UnknownTypes(skipped map[string]int) []string {
	var out []string
	for k := range skipped {
		if t, ok := strings.CutPrefix(k, SkipUnknown); ok {
			out = append(out, t)
		}
	}
	return out
}

// Lines calls fn with each line of r (1-based) without its line ending.
// A blank line is passed too, so line numbers match the file.
func Lines(r io.Reader, fn func(n int, line []byte)) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			fn(n, trimLineEnd(line))
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func trimLineEnd(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// ReadCopy calls fn with each line of a copy.
func ReadCopy(c Copy, fn func(n int, line []byte)) error {
	r, err := c.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	return Lines(r, fn)
}

// Counts is a reason-to-count report.
type Counts map[string]int

// Add adds one to reason.
func (c Counts) Add(reason string) { c[reason]++ }

// Merge adds every count of other.
func (c Counts) Merge(other map[string]int) {
	for k, v := range other {
		c[k] += v
	}
}

var nameRune = regexp.MustCompile(`[^a-z0-9-]`)

// ReminderName makes a reminder name from a source type: lowercased, other
// characters become "-", and an empty result is "attachment".
func ReminderName(source string) string {
	s := nameRune.ReplaceAllString(strings.ToLower(source), "-")
	s = strings.TrimLeft(s, "-0123456789")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		return "attachment"
	}
	return s
}

// MaxReminderBytes is the largest content llm.Reminder accepts.
const MaxReminderBytes = 64 << 10

// Reminder returns a user message holding a contextual reminder, or user
// text when the reminder would fail validation by size. ok is false when
// the content is empty, so nothing should be added.
func Reminder(name, from, text string, changed Counts) (msg llm.Message, ok bool) {
	if text == "" {
		changed.Add(ChangeDropped + "reminder")
		return llm.Message{}, false
	}
	r := llm.Reminder{Name: ReminderName(name), Tier: llm.ReminderContextual, From: from, Content: text}
	if len(text) > MaxReminderBytes {
		changed.Add(ChangeReminderAsText)
		return llm.Message{Role: llm.RoleUser, Content: []llm.Content{llm.Text(text)}}, true
	}
	if r.Validate() != nil {
		// Only From can still fail; the sender is descriptive, so drop it.
		r.From = ""
		if r.Validate() != nil {
			changed.Add(ChangeDropped + "reminder")
			return llm.Message{}, false
		}
	}
	return llm.ReminderMessage(r), true
}

// Keep returns the content values that pass llm.Content.Validate, counting
// each one dropped under its kind.
func Keep(parts []llm.Content, changed Counts) []llm.Content {
	out := parts[:0]
	for _, p := range parts {
		if err := p.Validate(); err != nil {
			changed.Add(ChangeDropped + string(p.Kind))
			continue
		}
		out = append(out, p)
	}
	return out
}
