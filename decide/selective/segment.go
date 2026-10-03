package selective

import (
	"slices"

	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/internal/compactionrules"
	"github.com/deepnoodle-ai/dashi/llm"
)

// plan sorts the messages before the tail into classes. Every slice holds
// indexes into the history Start was given.
type plan struct {
	// msgs[i] is message i without reminders that have no sender; a message
	// left empty is removed and has no class.
	msgs  []llm.Message
	sizes []int

	instructions []int // system and developer messages, kept first
	pinned       []int // the user's own words, kept whole and never judged
	summaries    []int // earlier summaries, summarized again
	segments     []segment
}

// segment is a run of older messages that compaction keeps, shortens, or
// drops as a whole.
type segment struct {
	indexes   []int
	whole     []llm.Message // without reasoning
	short     []llm.Message // nil when there is no short form
	size      int
	shortSize int
}

// split sorts messages[:cut] into a plan.
//
//   - System and developer messages are instructions.
//   - A reminder without a sender is removed, as the stock compactor does.
//   - An earlier summary (compaction.IsSummary) is summarized again.
//   - Of named items (Message.Context), only the newest of each name in the
//     whole history is judged, alone; older ones are removed.
//   - A run of consecutive assistant messages and every tool-result message
//     after it is one segment. Segments that share a tool call and its
//     results are merged.
//   - A user message with agent mail (a reminder with a sender) is a
//     segment alone.
//   - A user message of text only is the user's own words, pinned.
//   - Any other user message, such as one with an image, is a segment alone.
func split(messages []llm.Message, cut int) *plan {
	p := &plan{msgs: make([]llm.Message, cut), sizes: make([]int, cut)}
	newest := map[llm.MessageContext]int{}
	for i, m := range messages {
		if m.Context != nil {
			newest[*m.Context] = i
		}
	}
	var cur *segment
	inResults := false
	add := func(i int) {
		p.segments = append(p.segments, segment{indexes: []int{i}})
		cur = &p.segments[len(p.segments)-1]
		inResults = false
	}
	for i := range cut {
		m, ok := compactionrules.WithoutReminders(messages[i])
		if !ok {
			continue
		}
		p.msgs[i], p.sizes[i] = m, compactionrules.Estimate(m)
		run := false
		switch {
		case m.Role == llm.RoleSystem || m.Role == llm.RoleDeveloper:
			p.instructions = append(p.instructions, i)
		case compaction.IsSummary(m):
			p.summaries = append(p.summaries, i)
		case m.Context != nil:
			if newest[*m.Context] == i {
				add(i)
			}
		case m.Role == llm.RoleAssistant:
			if cur == nil || inResults {
				add(i)
			} else {
				cur.indexes = append(cur.indexes, i)
			}
			run = true
		case slices.ContainsFunc(m.Content, isResult):
			if cur == nil {
				add(i)
			} else {
				cur.indexes = append(cur.indexes, i)
			}
			inResults, run = true, true
		case ownWords(m):
			p.pinned = append(p.pinned, i)
		default:
			add(i)
		}
		if !run {
			cur = nil
		}
	}
	p.segments = mergeExchanges(p.segments, p.msgs)
	return p
}

// mergeExchanges merges segments when one holds a result of a tool call in
// another, so each call stays with all of its results.
func mergeExchanges(segs []segment, msgs []llm.Message) []segment {
	parent := make([]int, len(segs))
	for i := range parent {
		parent[i] = i
	}
	var root func(int) int
	root = func(i int) int {
		for parent[i] != i {
			i = parent[i]
		}
		return i
	}
	owner := map[string]int{}
	for i, s := range segs {
		for _, at := range s.indexes {
			for _, c := range msgs[at].Content {
				if c.ToolCall != nil {
					owner[c.ToolCall.ID] = i
				}
			}
		}
	}
	merged := false
	for i, s := range segs {
		for _, at := range s.indexes {
			for _, c := range msgs[at].Content {
				if c.ToolResult == nil {
					continue
				}
				if j, ok := owner[c.ToolResult.CallID]; ok && root(j) != root(i) {
					parent[root(i)] = root(j)
					merged = true
				}
			}
		}
	}
	if !merged {
		return segs
	}
	var out []segment
	at := map[int]int{} // root to its index in out
	for i, s := range segs {
		r := root(i)
		k, ok := at[r]
		if !ok {
			k = len(out)
			at[r] = k
			out = append(out, segment{})
		}
		out[k].indexes = append(out[k].indexes, s.indexes...)
	}
	for k := range out {
		slices.Sort(out[k].indexes)
	}
	slices.SortFunc(out, func(a, b segment) int { return a.indexes[0] - b.indexes[0] })
	return out
}

// measure fills the segment's whole text, short form, and sizes.
func (s *segment) measure(p *plan, shortForm func([]llm.Message) ([]llm.Message, bool)) {
	s.whole = nil
	for _, i := range s.indexes {
		if m, ok := withoutReasoning(p.msgs[i]); ok {
			s.whole = append(s.whole, m)
		}
	}
	s.size = compactionrules.EstimateAll(s.whole)
	short, ok := shortForm(cloneMessages(s.whole))
	if !ok {
		return
	}
	var cleaned []llm.Message
	for _, m := range short {
		if m, ok := withoutReasoning(m); ok {
			cleaned = append(cleaned, m)
		}
	}
	if size := compactionrules.EstimateAll(cleaned); size < s.size {
		s.short, s.shortSize = cleaned, size
	}
}

// keep returns the replacement history's kept messages and where the
// summary goes: the instructions, then the pinned messages and the kept
// segments in history order, then the summary, then the tail.
func (p *plan) keep(messages []llm.Message, cut int, actions []Action) ([]llm.Message, int) {
	type piece struct {
		at   int
		msgs []llm.Message
	}
	var pieces []piece
	for _, i := range p.pinned {
		pieces = append(pieces, piece{i, []llm.Message{p.msgs[i]}})
	}
	for k, s := range p.segments {
		switch actions[k] {
		case ActionKeep:
			pieces = append(pieces, piece{s.indexes[0], s.whole})
		case ActionShort:
			pieces = append(pieces, piece{s.indexes[0], s.short})
		}
	}
	slices.SortStableFunc(pieces, func(a, b piece) int { return a.at - b.at })
	var keep []llm.Message
	for _, i := range p.instructions {
		keep = append(keep, p.msgs[i])
	}
	for _, pc := range pieces {
		keep = append(keep, pc.msgs...)
	}
	at := len(keep)
	return append(keep, messages[cut:]...), at
}

// keptMark starts the copy of the user's own words in the summary request,
// so the summarizer knows the task without restating it.
const keptMark = "[kept word for word]"

// summaryInput returns what the summary covers, in history order: the
// dropped segments, the full text of shortened ones, and earlier summaries,
// with the user's own words marked as already kept.
func (p *plan) summaryInput(actions []Action) []llm.Message {
	var at []int
	at = append(at, p.pinned...)
	at = append(at, p.summaries...)
	for k, s := range p.segments {
		if actions[k] != ActionKeep {
			at = append(at, s.indexes...)
		}
	}
	slices.Sort(at)
	out := make([]llm.Message, 0, len(at))
	for _, i := range at {
		m := p.msgs[i]
		if slices.Contains(p.pinned, i) {
			parts := append([]llm.Content{llm.Text(keptMark)}, m.Content...)
			m = llm.Message{Role: llm.RoleUser, Content: parts}
		}
		out = append(out, m)
	}
	return out
}

// ownWords reports whether m is the user's own words: a user message of
// text only, with no named Context, that is not a summary.
func ownWords(m llm.Message) bool {
	if m.Role != llm.RoleUser || m.Context != nil || len(m.Content) == 0 ||
		compaction.IsSummary(m) {
		return false
	}
	for _, c := range m.Content {
		if c.Kind != llm.ContentBlock || c.Block == nil || c.Block.Text == nil {
			return false
		}
	}
	return true
}

func isResult(c llm.Content) bool { return c.ToolResult != nil }

// withoutReasoning returns m without reasoning and opaque provider content,
// which a kept older message loses, as the stock summary does, so a later
// model switch cannot make a provider reject it. It returns false when
// nothing is left.
func withoutReasoning(m llm.Message) (llm.Message, bool) {
	return without(m, func(c llm.Content) bool {
		return c.Kind == llm.ContentReasoning || c.Kind == llm.ContentOpaque
	})
}

func without(m llm.Message, drop func(llm.Content) bool) (llm.Message, bool) {
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

func cloneMessages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m.Clone()
	}
	return out
}
