// Package selective is a session.Compactor that keeps the older messages
// still needed word for word and summarizes only the rest.
//
// The stock compactor (compaction.Compactor) keeps a recent tail and
// summarizes everything older, so a detail the agent needs later, such as an
// error string or a path, can be lost. This Compactor wraps it: it cuts the
// tail later, asks a decision model through Decide whether each older
// segment is still needed and whether its exact text is, keeps the needed
// segments whole or as a short form, and has the summary cover what was not
// kept whole. The history after compaction fits the stock compactor's size
// budget.
//
// Any Decide failure, a pairing problem, or a selection with nothing to
// summarize falls back to the stock Start, and OnSelection receives the
// cause in Selection.Fallback.
package selective

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/decide/internal/client"
	"github.com/deepnoodle-ai/dashi/internal/compactionrules"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/patterns/compact"
)

const (
	// DefaultOlderFraction is the share of the window that older segments
	// may fill, out of Base's tail share.
	DefaultOlderFraction = 0.15

	// DefaultMaxRequests is the number of concurrent Decide requests.
	DefaultMaxRequests = 4

	// DefaultTimeout bounds the whole wait for Decide.
	DefaultTimeout = 10 * time.Second
)

// Compactor implements session.Compactor. The zero value works: it uses the
// zero compaction.Compactor as Base and decide.NewClient() for Decide. Pin
// the decision model on the client with decide.WithModel, because an alias
// such as jev-latest can move.
type Compactor struct {
	// Base says when to compact, the window, the total size kept, and the
	// summary model. Its TailFraction (0.25 of the window by default) is the
	// total share kept: the tail and the older segments together. A
	// negative TailFraction keeps nothing word for word, so Start returns
	// Base's Start.
	Base compaction.Compactor

	// Client asks Decide. Nil means decide.NewClient() from the
	// environment; when that fails, Start falls back to Base.
	Client *decide.Client

	// OlderFraction is the share of the window for older segments, taken
	// from Base's total, so the tail is cut later. Zero means
	// DefaultOlderFraction. Older segments fill whatever room the tail
	// leaves. A negative value turns selection off: Start returns Base's
	// Start, and Selection.Fallback says why. The tail never starts after the user's latest own words while
	// Base's tail would keep them: a current turn larger than the tail
	// share is kept whole, as Base keeps it, and leaves less room for older
	// segments.
	OlderFraction float64

	// MinNeeded is the floor on P(needed). Zero keeps a segment only if
	// P(needed) is above 0.5. A positive value keeps it at that
	// probability or above. A negative value keeps by rank alone.
	MinNeeded float64

	// MinVerbatim is the floor on P(exact text needed). Zero keeps a
	// segment whole only if P(exact text needed) is above 0.5, and short
	// otherwise. A positive value keeps it whole at that probability or
	// above. A negative value always prefers the whole text and does not
	// ask.
	MinVerbatim float64

	// MaxRequests is the number of concurrent Decide requests. Zero means
	// DefaultMaxRequests.
	MaxRequests int

	// Timeout bounds the whole wait for Decide. Zero means DefaultTimeout.
	Timeout time.Duration

	// Prompt replaces DefaultPrompt for the summary.
	Prompt string

	// ShortForm writes a smaller copy of a segment in code. Nil means
	// DefaultShortForm. It returns false when the segment has no smaller
	// copy. It must keep every tool call with its results.
	ShortForm func(segment []llm.Message) ([]llm.Message, bool)

	// OnSelection, when set, is called once for every Start, with what
	// Decide answered and what it cost, also when Start fell back to Base.
	// It may be called from any goroutine that calls Start.
	OnSelection func(Selection)
}

// Selection is what one Start chose and what Decide cost. A host joins its
// spend to the compaction turn by Hash.
type Selection struct {
	// Hash is the dashi.HistoryHash of the messages Start was given. It
	// equals the compaction turn's CompactionFact.Hash.
	Hash string

	// Segments holds one choice per judged segment, in history order.
	Segments []SegmentChoice

	// Usage is the token usage of every Decide request.
	Usage decide.Usage

	// RequestIDs holds the Decide request IDs.
	RequestIDs []string

	// Models holds the resolved decision model versions, such as
	// jev-1.13.0, sorted and without repeats.
	Models []string

	// Fallback is why Start returned Base's Start instead; empty when the
	// selection was used.
	Fallback string
}

// Action is what compaction did with a segment.
type Action string

const (
	// ActionKeep keeps the segment word for word.
	ActionKeep Action = "keep"
	// ActionShort keeps the segment's short form; the summary covers its
	// full text.
	ActionShort Action = "short"
	// ActionDrop leaves the segment to the summary.
	ActionDrop Action = "drop"
)

// SegmentChoice is the choice for one segment.
type SegmentChoice struct {
	// MessageIndexes holds the indexes of the segment's messages in the
	// history Start was given.
	MessageIndexes []int

	// Action is what compaction did with the segment. It is empty when
	// Start stopped before selection.
	Action Action

	// Scores are Decide's answers. Scores.Err is set when the segment could
	// not be judged, and such a segment is dropped.
	Scores compact.Scores

	// ShortJudged is true when the segment was too large for a Decide
	// request and was judged by its short form; if kept, it is kept short.
	ShortJudged bool

	// Size is the estimated token count of the whole segment, and ShortSize
	// that of its short form, or zero when it has none.
	Size      int
	ShortSize int
}

// Needed reports whether Base finds compaction due.
func (c *Compactor) Needed(messages []llm.Message, last llm.Usage) bool {
	return c.Base.Needed(messages, last)
}

// Start builds the compaction turn's Start. It keeps the history's system
// and developer messages, the user's own words, the older segments Decide
// finds still needed (whole or short), and a later-cut tail that holds at
// least what Base's tail holds of the current turn, with the summary of the
// rest just before the tail. It returns Base's Start
// instead when Decide fails or passes Timeout, when the user's own words
// alone exceed the budget, when nothing was dropped or shortened, or when a
// tool call would be separated from its result. It returns an error only
// when Base's Start fails or ctx ends.
func (c *Compactor) Start(ctx context.Context, messages []llm.Message) (dashi.Start, error) {
	var sel Selection
	defer func() {
		if c.OnSelection != nil {
			c.OnSelection(sel)
		}
	}()
	stock, err := c.Base.Start(ctx, messages)
	if err != nil {
		sel.Fallback = err.Error()
		return dashi.Start{}, err
	}
	sel.Hash = stock.Compaction.Hash
	start, err := c.selective(ctx, messages, &sel)
	if err == nil {
		return start, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		sel.Fallback = ctxErr.Error()
		return dashi.Start{}, ctxErr
	}
	sel.Fallback = err.Error()
	return stock, nil
}

// errNothingDropped means every older segment was kept whole, so there is
// nothing for a summary to cover.
var errNothingDropped = errors.New("selective: nothing was dropped or shortened")

// selective builds the selective Start, or returns why it cannot.
func (c *Compactor) selective(ctx context.Context, messages []llm.Message, sel *Selection) (dashi.Start, error) {
	judge, err := client.Default(c.Client)
	if err != nil {
		return dashi.Start{}, fmt.Errorf("selective: no Decide client: %w", err)
	}
	window, _ := c.Base.Policy.Window()
	total := c.Base.TailFraction
	if total == 0 {
		total = compaction.DefaultTailFraction
	}
	older := c.OlderFraction
	if older == 0 {
		older = DefaultOlderFraction
	}
	switch {
	case total < 0:
		return dashi.Start{}, errors.New("selective: Base.TailFraction is negative, so nothing is kept word for word")
	case older < 0:
		return dashi.Start{}, errors.New("selective: OlderFraction is negative, so selection is off")
	}
	older = min(older, total)
	budget := int(total * float64(window))
	cut := currentTurnCut(messages,
		compactionrules.Cut(messages, max(0, budget)),
		compactionrules.Cut(messages, max(0, int((total-older)*float64(window)))))
	budget -= compactionrules.EstimateAll(messages[cut:])
	if budget <= 0 {
		return dashi.Start{}, errors.New("selective: the tail fills the budget")
	}

	p := split(messages, cut)
	if len(p.segments) == 0 {
		return dashi.Start{}, errNothingDropped
	}
	shortForm := c.ShortForm
	if shortForm == nil {
		shortForm = DefaultShortForm
	}
	for i := range p.segments {
		p.segments[i].measure(p, shortForm)
	}
	sel.Segments = make([]SegmentChoice, len(p.segments))
	for i, s := range p.segments {
		sel.Segments[i] = SegmentChoice{
			MessageIndexes: slices.Clone(s.indexes),
			Size:           s.size,
			ShortSize:      s.shortSize,
		}
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	workers := c.MaxRequests
	if workers <= 0 {
		workers = DefaultMaxRequests
	}
	askCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	q := questions{
		state:    focusState(messages, cut, p),
		verbatim: c.MinVerbatim >= 0,
	}
	answers, err := q.ask(askCtx, judge, p.segments, workers, sel)
	if err != nil {
		return dashi.Start{}, fmt.Errorf("selective: Decide: %w", err)
	}
	for i := range sel.Segments {
		sel.Segments[i].Scores = answers[i].scores
		sel.Segments[i].ShortJudged = answers[i].shortJudged
	}

	actions, err := c.choose(p, answers, budget)
	if err != nil {
		return dashi.Start{}, err
	}
	changed := false
	for i, a := range actions {
		sel.Segments[i].Action = a
		changed = changed || a != ActionKeep
	}
	if !changed {
		return dashi.Start{}, errNothingDropped
	}

	keep, summaryAt := p.keep(messages, cut, actions)
	if err := checkPairing(messages, keep); err != nil {
		return dashi.Start{}, err
	}
	prompt := c.Prompt
	if prompt == "" {
		prompt = DefaultPrompt
	}
	summarizer := c.Base
	summarizer.Prompt = prompt
	summarizer.TailFraction = -1 // the summary covers all of its input
	summary, err := summarizer.Start(ctx, p.summaryInput(actions))
	if err != nil {
		return dashi.Start{}, fmt.Errorf("selective: summary request: %w", err)
	}
	return dashi.Start{
		Agent:  summary.Agent,
		Origin: dashi.StartCompaction,
		Input:  summary.Input,
		Compaction: &dashi.CompactionFact{
			Keep:      keep,
			SummaryAt: summaryAt,
			Frame:     summary.Compaction.Frame,
			Hash:      sel.Hash,
		},
	}, nil
}

// currentTurnCut returns where the tail starts, given stockCut, Base's own
// cut at the total share, and cut, the later cut at the tail share. When
// the current turn (from the user's latest own words on) is larger than the
// tail share, cut falls inside it. The tail then starts where the current
// turn does, or at stockCut if that is later, so every message after the
// user's latest own words that the stock compactor keeps word for word is
// kept word for word here too, counted in the budget, and never judged.
func currentTurnCut(messages []llm.Message, stockCut, cut int) int {
	latest := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if m, ok := compactionrules.WithoutReminders(messages[i]); ok && ownWords(m) {
			latest = i
			break
		}
	}
	switch {
	case latest < 0 || cut <= latest:
		return cut
	case stockCut >= latest:
		return stockCut
	}
	for at := latest; at > stockCut; at-- {
		if compactionrules.CanCut(messages, at) {
			return at
		}
	}
	return stockCut
}

// choose runs compact.Select over the pinned messages and the judged
// segments and returns an action per segment. A segment that could not be
// judged is dropped, not kept first as Select would.
func (c *Compactor) choose(p *plan, answers []answer, budget int) ([]Action, error) {
	var segs []compact.Segment
	var scores []compact.Scores
	var index []int // index[k] is the segment of segs[k], or -1 when pinned
	pinned := 0
	for _, i := range p.pinned {
		pinned += p.sizes[i]
	}
	segs = append(segs, compact.Segment{Size: pinned, Pinned: true})
	scores = append(scores, compact.Scores{})
	index = append(index, -1)
	actions := make([]Action, len(p.segments))
	for i, s := range p.segments {
		a := answers[i]
		if a.scores.Err != nil {
			actions[i] = ActionDrop
			continue
		}
		seg := compact.Segment{Size: s.size, Short: shortMark(s.short != nil), ShortSize: s.shortSize}
		if a.shortJudged {
			// Judged by its short form, so it can only be kept short.
			seg = compact.Segment{Size: s.shortSize}
		}
		segs = append(segs, seg)
		scores = append(scores, a.scores)
		index = append(index, i)
	}
	rule := compact.Rule{
		Budget: budget,
		Floor:  floor(c.MinNeeded),
	}
	if c.MinVerbatim >= 0 {
		rule.ShortBelow = floor(c.MinVerbatim)
	}
	decisions, err := compact.Select(segs, scores, rule)
	if errors.Is(err, compact.ErrOverBudget) {
		return nil, errors.New("selective: the user's own words exceed the budget")
	}
	if err != nil {
		return nil, fmt.Errorf("selective: select: %w", err)
	}
	for _, d := range decisions {
		i := index[d.Index]
		if i < 0 {
			continue
		}
		switch {
		case d.Action == compact.Drop:
			actions[i] = ActionDrop
		case d.Action == compact.Short || answers[i].shortJudged:
			actions[i] = ActionShort
		default:
			actions[i] = ActionKeep
		}
	}
	return actions, nil
}

// shortMark gives Select a non-empty Short when the segment has a short
// form. Select reads only whether Short is empty; the text is not sent.
func shortMark(ok bool) string {
	if ok {
		return "short form"
	}
	return ""
}

// floor turns a Min field into a compact.Rule floor: zero means above 0.5,
// a negative value turns the floor off, and a positive one is used as is,
// at most 1.
func floor(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v == 0:
		return justAboveHalf
	}
	return min(v, 1)
}

// justAboveHalf is the smallest probability above 0.5. Select keeps a
// segment whose P(needed) is at least its floor, so this floor keeps only
// probabilities above 0.5.
var justAboveHalf = math.Nextafter(0.5, 1)

var _ session.Compactor = (*Compactor)(nil)
