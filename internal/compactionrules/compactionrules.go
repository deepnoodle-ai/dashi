// Package compactionrules holds the rules the stock compactor uses to size,
// clean, and cut history: the token estimate, the reminder filter, and the
// tail cut. It is internal so that these rules do not become public
// compaction API.
package compactionrules

import (
	"encoding/json"
	"math"
	"slices"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/contentsrc"
	"github.com/deepnoodle-ai/dashi/llm"
)

// BytesPerToken converts encoded size to a token estimate. The estimate is
// rough and only guides where to cut.
const BytesPerToken = 4

// Estimate returns a rough token count for one message: its encoded size,
// plus the inline size of media it references, over BytesPerToken. It
// saturates at math.MaxInt instead of overflowing.
func Estimate(m llm.Message) int {
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
			wire := ReferenceWireSize(src.Ref.Size)
			if wire > math.MaxInt-size {
				return math.MaxInt
			}
			size += wire
		}
	}
	return size/BytesPerToken + 1
}

// EstimateAll returns the sum of Estimate over messages, saturating at
// math.MaxInt.
func EstimateAll(messages []llm.Message) int {
	n := 0
	for _, m := range messages {
		e := Estimate(m)
		if e > math.MaxInt-n {
			return math.MaxInt
		}
		n += e
	}
	return n
}

// ReferenceWireSize estimates the inline base64 bytes of a declared
// reference. It never loads a reference or claims provider-specific media
// token fit.
func ReferenceWireSize(size int64) int {
	groups := size / 3
	if size%3 != 0 {
		groups++
	}
	if groups > int64(math.MaxInt/4) {
		return math.MaxInt
	}
	return int(groups) * 4
}

// WithoutReminders returns m without reminder content that has no sender.
// The host supplies those reminders again on each Turn, so compaction
// removes them. A reminder with a sender (From) is agent mail, such as a
// subagent's reply, which nobody supplies again, so it stays. It returns
// false when nothing is left. It never changes m's content in place.
func WithoutReminders(m llm.Message) (llm.Message, bool) {
	drop := func(c llm.Content) bool {
		return c.Kind == llm.ContentReminder && (c.Reminder == nil || c.Reminder.From == "")
	}
	if !slices.ContainsFunc(m.Content, drop) {
		return m, len(m.Content) > 0
	}
	kept := make([]llm.Content, 0, len(m.Content))
	for _, c := range m.Content {
		if !drop(c) {
			kept = append(kept, c)
		}
	}
	m.Content = kept
	return m, len(kept) > 0
}

// Cut returns the index of the first message of the tail that compaction
// keeps word for word: the earliest cut after the first start whose tail
// fits in budget tokens, where a start is a user message other than a tool
// result, and the tail keeps every tool result with its call. It returns
// len(messages) to keep no tail.
func Cut(messages []llm.Message, budget int) int {
	sizes := make([]int, len(messages)+1)
	for i := len(messages) - 1; i >= 0; i-- {
		sizes[i] = sizes[i+1] + Estimate(messages[i])
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

// CanCut reports whether a tail may start at messages[at]: the message is a
// user message other than a tool result, and the tail keeps every tool
// result with its call. Unlike Cut, it allows the first start.
func CanCut(messages []llm.Message, at int) bool {
	return at >= 0 && at < len(messages) && isStart(messages[at]) &&
		!splitsExchange(messages[at:])
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
