package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// BlockKind names what a Block shows.
type BlockKind string

const (
	KindUser      BlockKind = "user"
	KindAssistant BlockKind = "assistant"
	KindTool      BlockKind = "tool"
	KindDiff      BlockKind = "diff"
	KindSteer     BlockKind = "steer"
	KindStop      BlockKind = "stop"
	KindFooter    BlockKind = "footer"
	KindNotice    BlockKind = "notice"
	KindWarning   BlockKind = "warning"
)

// Tool line states.
const (
	StateRunning = "running"
	StateOK      = "ok"
	StateFailed  = "failed"
	StateDenied  = "denied"
	StateWaiting = "waiting"
)

// Steer line states.
const (
	SteerQueued    = "queued"
	SteerDelivered = "delivered"
)

// Block is one item of the transcript. A block with an ID that the transcript
// already holds replaces that block. This is how streamed text grows and how a
// tool line moves from running to done.
type Block struct {
	ID   string
	Kind BlockKind
	// Text is the user or assistant text, a notice, a stop message, or the
	// footer. For a tool line it is empty.
	Text string
	// Name and Arg describe a tool call: its name and its main argument.
	Name string
	Arg  string
	// State is the tool or steer state.
	State   string
	Elapsed time.Duration
	// Lines holds detail lines: the last output of a running tool, the
	// first error line of a failed tool, or the lines of a diff.
	Lines []string
	// Done marks a block that will not change again.
	Done bool
}

// Renderer turns Dashi events into blocks. The screen, line mode, and -p use
// it, and a resumed session replays step events through it, so the scrollback
// of a reopened session equals what the live run showed.
type Renderer struct {
	// Turn is the turn number shown in the footer.
	Turn int
	// Model is the model name shown in the footer.
	Model string

	calls map[string]*callState
	text  map[string]string
}

type callState struct {
	name    string
	arg     string
	started time.Time
	denied  string
}

// NewRenderer returns a renderer for turn number n.
func NewRenderer(n int, model string) *Renderer {
	return &Renderer{
		Turn:  n,
		Model: model,
		calls: map[string]*callState{},
		text:  map[string]string{},
	}
}

// CallName returns the tool name of a call the renderer has seen.
func (r *Renderer) CallName(callID string) string {
	if c := r.calls[callID]; c != nil {
		return c.name
	}
	return ""
}

// Event returns the blocks that one event changes.
func (r *Renderer) Event(ev dashi.Event) []Block {
	switch {
	case ev.Delta != nil:
		return r.delta(ev)
	case ev.Progress != nil:
		return r.progress(ev.Progress)
	case ev.Step != nil:
		return r.step(*ev.Step)
	}
	return nil
}

func (r *Renderer) delta(ev dashi.Event) []Block {
	if ev.Delta.Kind != llm.DeltaText || ev.Delta.Text == "" {
		return nil
	}
	id := messageKey(ev.MessageID, ev.AttemptID)
	r.text[id] += ev.Delta.Text
	return []Block{{ID: id, Kind: KindAssistant, Text: r.text[id]}}
}

func messageKey(messageID, attemptID string) string {
	if messageID != "" {
		return "a:" + messageID
	}
	return "a:" + attemptID
}

func (r *Renderer) progress(p *dashi.ToolProgress) []Block {
	c := r.calls[p.CallID]
	if c == nil {
		c = &callState{name: p.Name, started: time.Now()}
		r.calls[p.CallID] = c
	}
	b := r.toolBlock(p.CallID, c, StateRunning)
	if p.Message != "" {
		b.Lines = tailLines(p.Message, 3)
	}
	return []Block{b}
}

func (r *Renderer) toolBlock(id string, c *callState, state string) Block {
	return Block{
		ID:    "t:" + id,
		Kind:  KindTool,
		Name:  c.name,
		Arg:   c.arg,
		State: state,
	}
}

func (r *Renderer) step(s dashi.Step) []Block {
	var out []Block
	switch s.Kind {
	case dashi.StepStarted:
		out = append(out, Block{
			ID:   "u:" + s.TurnID,
			Kind: KindUser,
			Text: joinMessages(s.Started.Input),
			Done: true,
		})
	case dashi.StepModelSettled, dashi.StepModelReconciled:
		out = append(out, r.modelSettled(s)...)
	case dashi.StepToolDecision:
		out = append(out, r.toolDecision(s)...)
	case dashi.StepToolStarted:
		out = append(out, r.toolStarted(s)...)
	case dashi.StepToolSettled:
		out = append(out, r.toolSettled(s)...)
	case dashi.StepToolWaiting:
		if c := r.calls[s.Tool.Call.ID]; c != nil {
			out = append(out, r.toolBlock(s.Tool.Call.ID, c, StateWaiting))
		}
	case dashi.StepDelivered:
		if s.Context != nil && s.Context.Steering {
			for _, item := range s.Context.Items {
				out = append(out, Block{
					ID:    "s:" + item.ID,
					Kind:  KindSteer,
					Text:  item.Message.Text(),
					State: SteerDelivered,
					Done:  true,
				})
			}
		}
	case dashi.StepStopped:
		out = append(out, r.stopped(s)...)
	}
	for _, w := range s.Warnings {
		out = append(out, Block{
			Kind: KindWarning,
			Text: w.Message,
			Done: true,
		})
	}
	return out
}

func (r *Renderer) modelSettled(s dashi.Step) []Block {
	m := s.Model
	if m == nil || m.Result == nil || m.Interrupted != "" {
		return nil
	}
	text := m.Result.Message.Text()
	if text == "" {
		return nil
	}
	id := messageKey(m.MessageID, m.AttemptID)
	r.text[id] = text
	return []Block{{ID: id, Kind: KindAssistant, Text: text, Done: true}}
}

func (r *Renderer) toolCall(s dashi.Step) *callState {
	call := s.Tool.Call
	c := r.calls[call.ID]
	if c == nil {
		c = &callState{name: call.Name}
		r.calls[call.ID] = c
	}
	c.name = call.Name
	c.arg = mainArg(call.Name, call.Args)
	return c
}

func (r *Renderer) toolDecision(s dashi.Step) []Block {
	c := r.toolCall(s)
	if s.Tool.Verdict.Kind == dashi.DecisionDeny {
		c.denied = s.Tool.Verdict.Reason
	}
	return nil
}

func (r *Renderer) toolStarted(s dashi.Step) []Block {
	c := r.toolCall(s)
	c.started = s.At
	return []Block{r.toolBlock(s.Tool.Call.ID, c, StateRunning)}
}

func (r *Renderer) toolSettled(s dashi.Step) []Block {
	id := s.Tool.Call.ID
	c := r.toolCall(s)
	state := StateOK
	var lines []string
	if o := s.Tool.Outcome; o != nil {
		switch o.State {
		case tool.StateFailed:
			state = StateFailed
			lines = tailLines(outputText(o), 1)
		case tool.StateNotExecuted:
			state = StateDenied
			if c.denied != "" {
				lines = []string{c.denied}
			}
		case tool.StateWaiting, tool.StateDetached:
			state = StateWaiting
		}
	}
	b := r.toolBlock(id, c, state)
	b.Lines = lines
	b.Done = true
	if !c.started.IsZero() && !s.At.IsZero() {
		b.Elapsed = s.At.Sub(c.started)
	}
	return []Block{b}
}

func (r *Renderer) stopped(s dashi.Step) []Block {
	st := s.Stop
	var text string
	switch st.Kind {
	case dashi.StopCompleted:
		return nil
	case dashi.StopLimited:
		text = limitText(st)
	case dashi.StopInterrupted:
		text = "Interrupted. Press Enter to continue, or type a new prompt."
	case dashi.StopCanceled:
		text = fmt.Sprintf("Turn %d canceled.", r.Turn)
	case dashi.StopFailed:
		text = "The turn failed: " + orDefault(st.Reason, "unknown error") + "."
	case dashi.StopRefused:
		text = "The model refused the request."
	case dashi.StopWaiting:
		text = "The turn is waiting: " + orDefault(st.Reason, "for input") + "."
	case dashi.StopUncertain:
		text = "The turn stopped with an unknown result: " +
			orDefault(st.Reason, "unknown") + "."
	default:
		text = "The turn stopped: " + string(st.Kind) + "."
	}
	return []Block{{
		ID:   fmt.Sprintf("stop:%s:%d", s.TurnID, s.Seq),
		Kind: KindStop,
		Text: text,
		Done: true,
	}}
}

func limitText(st *dashi.Stop) string {
	if st.Limit != nil && st.Limit.Kind == dashi.LimitModelAttempts {
		return fmt.Sprintf(
			"Stopped after %d model calls. Type /continue to go on.",
			st.Limit.Used,
		)
	}
	return "Stopped at a limit: " + orDefault(st.Reason, "unknown") + "."
}

// Footer returns the line that closes a settled turn.
func (r *Renderer) Footer(v dashi.TurnView) Block {
	return Block{
		ID:   fmt.Sprintf("footer:%s", v.ID),
		Kind: KindFooter,
		Text: FooterText(r.Turn, r.Model, v),
		Done: true,
	}
}

// FooterText formats the turn summary: number, model, model calls, tokens,
// and cost.
func FooterText(turn int, model string, v dashi.TurnView) string {
	parts := []string{fmt.Sprintf("turn %d", turn)}
	if model != "" {
		parts = append(parts, model)
	}
	calls := v.ModelAttempts
	parts = append(parts, fmt.Sprintf("%d model %s", calls, plural(calls, "call")))
	tokens := v.Spend.Usage.TotalInputTokens() + v.Spend.Usage.OutputTokens
	parts = append(parts, groupDigits(tokens)+" tokens")
	if cost := costText(v.Spend); cost != "" {
		parts = append(parts, cost)
	}
	return "[" + strings.Join(parts, " | ") + "]"
}

func costText(sp dashi.Spend) string {
	if len(sp.Cost) == 0 {
		return ""
	}
	cur := make([]string, 0, len(sp.Cost))
	for c := range sp.Cost {
		cur = append(cur, c)
	}
	sort.Strings(cur)
	var out []string
	for _, c := range cur {
		v := sp.Cost[c].Float64()
		if c == "USD" {
			out = append(out, fmt.Sprintf("$%.3f", v))
		} else {
			out = append(out, fmt.Sprintf("%.3f %s", v, c))
		}
	}
	text := strings.Join(out, " + ")
	if sp.CostUnknown > 0 || sp.CostPartial > 0 {
		text = ">=" + text
	}
	return text
}

// RenderTurn returns every block of a recorded turn, in order, ending with
// its footer when the turn has ended.
func RenderTurn(n int, model string, turn dashi.Turn) ([]Block, error) {
	view, err := dashi.Replay(turn)
	if err != nil {
		return nil, err
	}
	r := NewRenderer(n, model)
	var out []Block
	for i := range turn.Steps {
		step := turn.Steps[i]
		out = append(out, r.Event(dashi.Event{
			TurnID: turn.ID,
			Seq:    step.Seq,
			Step:   &step,
		})...)
	}
	if view.Stop.Kind != "" {
		out = append(out, r.Footer(view))
	}
	return out, nil
}

// Transcript holds blocks in order and applies replacements by ID.
type Transcript struct {
	Blocks []Block
	index  map[string]int
}

// Apply adds b, or replaces the block that has the same ID. It returns the
// index of the block.
func (t *Transcript) Apply(b Block) int {
	if b.ID != "" {
		if i, ok := t.index[b.ID]; ok {
			t.Blocks[i] = b
			return i
		}
	}
	t.Blocks = append(t.Blocks, b)
	if b.ID != "" {
		if t.index == nil {
			t.index = map[string]int{}
		}
		t.index[b.ID] = len(t.Blocks) - 1
	}
	return len(t.Blocks) - 1
}

// FormatBlock returns the plain-text form of a block, without color, for line
// mode, -p, and tests. Multi-line blocks join with newlines.
func FormatBlock(b Block) string {
	switch b.Kind {
	case KindUser:
		return "> " + b.Text
	case KindAssistant:
		return b.Text
	case KindTool:
		return formatTool(b)
	case KindDiff:
		return strings.Join(b.Lines, "\n")
	case KindSteer:
		if b.State == SteerDelivered {
			return "  steer delivered: " + b.Text
		}
		return "  steer queued: " + b.Text
	case KindWarning:
		return "warning: " + b.Text
	case KindNotice:
		return b.Text
	}
	return b.Text
}

const toolLineWidth = 58

func formatTool(b Block) string {
	left := "  " + b.Name
	if b.Arg != "" {
		left += "  " + b.Arg
	}
	left = truncate(left, toolLineWidth)
	right := b.State
	if b.State == StateOK || b.State == StateFailed {
		right += fmt.Sprintf("  %.1fs", b.Elapsed.Seconds())
	}
	if b.State == StateRunning {
		right = "..."
	}
	line := fmt.Sprintf("%-*s %s", toolLineWidth, left, right)
	for _, l := range b.Lines {
		line += "\n    " + l
	}
	return line
}

// mainArg picks the argument to show next to a tool name: the path, command,
// pattern, or other first string that names what the call works on.
func mainArg(name string, raw json.RawMessage) string {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return ""
	}
	for _, key := range []string{
		"path", "file_path", "command", "pattern", "query", "url", "dir", "name",
	} {
		if s, ok := args[key].(string); ok && s != "" {
			return firstLine(s)
		}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := args[k].(string); ok && s != "" {
			return firstLine(s)
		}
	}
	return ""
}

func firstLine(s string) string {
	first, _, multi := strings.Cut(s, "\n")
	if multi {
		first += " ..."
	}
	return first
}

func outputText(o *tool.Outcome) string {
	if o == nil || o.Output == nil {
		return ""
	}
	var parts []string
	for _, b := range o.Output.Content {
		if b.Text != nil {
			parts = append(parts, b.Text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func tailLines(s string, n int) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func truncate(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-3]) + "..."
}

func joinMessages(msgs []llm.Message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if t := m.Text(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func groupDigits(n int) string {
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
