package compaction

import (
	"encoding/json"
	"math"
	"slices"

	"github.com/deepnoodle-ai/dashi/internal/compactiontext"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/contentsrc"
	"github.com/deepnoodle-ai/dashi/llm"
)

// estimate returns a rough token count for one message: its encoded size,
// plus the inline size of media it references, over
// compactiontext.BytesPerToken. It saturates at math.MaxInt instead of
// overflowing.
func estimate(m llm.Message) int {
	m.Context = nil
	b, _ := json.Marshal(m)
	size := len(b)
	for _, c := range m.Content {
		var blocks []content.Block
		if c.Block != nil {
			blocks = append(blocks, *c.Block)
		}
		if c.ToolResult != nil {
			blocks = append(blocks, c.ToolResult.Output.Content...)
		}
		if c.ProviderToolUse != nil {
			blocks = append(blocks, c.ProviderToolUse.Output...)
		}
		if c.ProviderToolResult != nil {
			blocks = append(blocks, c.ProviderToolResult.Output...)
		}
		for _, src := range contentsrc.Blocks(blocks) {
			if src.Ref == nil || src.Ref.Size <= 0 {
				continue
			}
			wire := referenceWireSize(src.Ref.Size)
			if wire > math.MaxInt-size {
				return math.MaxInt
			}
			size += wire
		}
	}
	return size/compactiontext.BytesPerToken + 1
}

// estimateAll returns the estimate of messages, saturating at math.MaxInt.
func estimateAll(messages []llm.Message) int {
	total := 0
	for _, m := range messages {
		n := estimate(m)
		if n > math.MaxInt-total {
			return math.MaxInt
		}
		total += n
	}
	return total
}

// referenceWireSize estimates the inline base64 bytes of a declared
// reference. It never loads a reference or claims provider-specific media
// token fit.
func referenceWireSize(size int64) int {
	groups := size / 3
	if size%3 != 0 {
		groups++
	}
	if groups > int64(math.MaxInt/4) {
		return math.MaxInt
	}
	return int(groups) * 4
}

// withoutReminders copies messages and removes reminder content that has no
// sender. The host supplies those reminders again on each Turn, so
// compaction removes them. A reminder with a sender (From) is agent mail,
// such as a subagent's reply, which nobody supplies again, so it stays. A
// message left with no content is dropped. It never changes a message's
// content in place.
func withoutReminders(messages []llm.Message) []llm.Message {
	drop := func(c llm.Content) bool {
		return c.Kind == llm.ContentReminder && (c.Reminder == nil || c.Reminder.From == "")
	}
	out := make([]llm.Message, 0, len(messages))
	for _, m := range messages {
		if slices.ContainsFunc(m.Content, drop) {
			kept := make([]llm.Content, 0, len(m.Content))
			for _, c := range m.Content {
				if !drop(c) {
					kept = append(kept, c)
				}
			}
			m.Content = kept
		}
		if len(m.Content) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// tailStart returns the index of the first message of the tail that
// compaction keeps word for word: the earliest cut after the first start
// whose tail fits in budget tokens, where a start is a user message other
// than a tool result, and the tail keeps every tool result with its call. It
// returns len(messages) to keep no tail.
func tailStart(messages []llm.Message, budget int) int {
	sizes := make([]int, len(messages)+1)
	for i := len(messages) - 1; i >= 0; i-- {
		sizes[i] = sizes[i+1] + estimate(messages[i])
	}
	var starts []int
	for i := range messages {
		if isStart(messages[i]) {
			starts = append(starts, i)
		}
	}
	for _, c := range starts[min(1, len(starts)):] {
		if sizes[c] <= budget && !splitsExchange(messages[c:]) {
			return c
		}
	}
	return len(messages)
}

func isStart(m llm.Message) bool {
	return m.Role == llm.RoleUser && !slices.ContainsFunc(m.Content, func(c llm.Content) bool {
		return c.ToolResult != nil
	})
}

// splitsExchange reports whether tail has a tool result whose call is not in
// tail. Such a tail is not valid history for a model.
func splitsExchange(tail []llm.Message) bool {
	calls := map[string]bool{}
	for _, m := range tail {
		for _, c := range m.Content {
			switch {
			case c.ToolCall != nil:
				calls[c.ToolCall.ID] = true
			case c.ToolResult != nil && !calls[c.ToolResult.CallID]:
				return true
			}
		}
	}
	return false
}
