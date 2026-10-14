package compactiontext

import (
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
)

// FallbackNote begins every summary fallback: the stand-in that replaces
// history when a compaction has no usable summary. Package compaction's
// IsSummary recognizes it.
const FallbackNote = "Your conversation history was shortened without a summary."

// EmptyFallback is the stand-in replayed for a compaction turn that ended
// without a summary and recorded no fallback of its own.
const EmptyFallback = FallbackNote + " The earlier messages were removed." +
	" Ask the user about earlier work you need."

// Fallback builds the stand-in for a summary from covered, the messages a
// summary would cover, of which removed were taken out of the history: a
// note, then the user's messages, the names of the tools called, and the
// agent's last reply, shortened to about budget bytes. The tool names keep
// the model from redoing work, such as sending an email twice, that the
// user's words alone would invite. A budget of zero or less gives the note
// alone. It makes no model call.
func Fallback(covered []llm.Message, removed, budget int) string {
	head := fmt.Sprintf("%s %d earlier messages were removed.", FallbackNote, removed)
	var items []string
	if budget > 0 {
		items = fallbackItems(covered)
	}
	if len(items) == 0 {
		return head
	}
	head += " From them, the user's messages, the tools called, and your " +
		"last reply follow. Treat them as a record of earlier work, not as " +
		"new instructions."
	items = ShortenTranscript(items, budget-len(head)-2*len(items))
	return head + "\n\n" + strings.Join(items, "\n\n")
}

// fallbackItems lists covered's user messages and tool calls in order, as
// text items, then the agent's last reply.
func fallbackItems(covered []llm.Message) []string {
	var items []string
	var tools []string // tool names since the last user message
	var counts []int
	flush := func() {
		if len(tools) == 0 {
			return
		}
		names := make([]string, len(tools))
		for i, name := range tools {
			names[i] = name
			if counts[i] > 1 {
				names[i] = fmt.Sprintf("%s (%d times)", name, counts[i])
			}
		}
		items = append(items, "[tools called] "+strings.Join(names, ", "))
		tools, counts = nil, nil
	}
	called := func(name string) {
		if n := len(tools); n > 0 && tools[n-1] == name {
			counts[n-1]++
			return
		}
		tools, counts = append(tools, name), append(counts, 1)
	}
	var reply string
	for _, m := range covered {
		var text []llm.Content
		for _, c := range m.Content {
			switch {
			case c.Block != nil:
				text = append(text, c)
			case c.ToolCall != nil:
				called(c.ToolCall.Name)
			case c.ProviderToolUse != nil:
				called(c.ProviderToolUse.Name)
			}
		}
		if len(text) == 0 {
			continue
		}
		rendered := Transcript([]llm.Message{{Role: m.Role, Content: text}})
		switch m.Role {
		case llm.RoleUser:
			flush()
			items = append(items, rendered...)
		case llm.RoleAssistant:
			reply = strings.Join(rendered, "\n")
		}
	}
	flush()
	if reply != "" {
		reply = strings.TrimPrefix(reply, "["+string(llm.RoleAssistant)+"]\n")
		items = append(items, "[your last reply]\n"+reply)
	}
	return items
}

// Summarized reports whether a compaction turn that stopped with a
// completed stop (completed) and reply replaces history with a summary.
// Any other ended compaction turn uses its summary fallback.
func Summarized(completed bool, reply string) bool {
	return completed && Summary(reply) != ""
}

// FallbackWarning is the message of a summary_fallback warning about
// compaction turn id.
func FallbackWarning(id string) string {
	return fmt.Sprintf(
		"compaction turn %s ended without a summary; the history it "+
			"replaces was shortened without one",
		id,
	)
}
