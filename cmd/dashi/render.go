package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// BlockKind names what a Block shows.
type BlockKind string

const (
	KindUser       BlockKind = "user"
	KindAssistant  BlockKind = "assistant"
	KindReasoning  BlockKind = "reasoning"
	KindTool       BlockKind = "tool"
	KindDiff       BlockKind = "diff"
	KindSteer      BlockKind = "steer"
	KindStop       BlockKind = "stop"
	KindFooter     BlockKind = "footer"
	KindNotice     BlockKind = "notice"
	KindWarning    BlockKind = "warning"
	KindCompaction BlockKind = "compaction"
	KindPending    BlockKind = "pending"
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
	// SteerNext is input that waits for the next turn.
	SteerNext = "next"
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
	// Detail retains complete tool output for expansion and native scrollback.
	Detail string
	// Expanded is a display projection; the stored transcript keeps its excerpt.
	Expanded bool
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
	// Dir is the workspace directory. Paths inside it show relative to it.
	Dir string
	// Origin distinguishes an internal summary from ordinary model text.
	Origin dashi.StartOrigin

	calls         map[string]*callState
	text          map[string]string
	summary       string
	showReasoning bool
	reasoning     map[string]map[summaryPart]string
	activeContext map[string]*activeContextAttempt
}

type callState struct {
	name    string
	arg     string
	started time.Time
	denied  string
}

// NewRenderer returns a renderer for turn number n. The model is what the
// footer shows when the turn does not record its own.
func NewRenderer(n int, model, dir string) *Renderer {
	return &Renderer{
		Turn:      n,
		Model:     model,
		Dir:       dir,
		calls:     map[string]*callState{},
		text:      map[string]string{},
		reasoning: map[string]map[summaryPart]string{},
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
	if r.Origin == dashi.StartCompaction || r.isActiveSummary(ev.AttemptID) {
		return nil
	}
	if ev.Delta.Kind == llm.DeltaReasoning && r.showReasoning {
		return r.reasoningDelta(ev)
	}
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
	var shell workspace.ShellProgress
	switch {
	case len(p.Data) > 0 && json.Unmarshal(p.Data, &shell) == nil &&
		len(shell.Lines) > 0:
		b.Lines = shell.Lines[max(len(shell.Lines)-3, 0):]
	case p.Message != "":
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
		r.Origin = s.Started.Origin
		r.showReasoning = s.Started.Agent.Settings.ReasoningSummary != nil && *s.Started.Agent.Settings.ReasoningSummary
		if m := s.Started.Agent.Model; m != "" {
			r.Model = m
		}
		if r.Origin == dashi.StartCompaction {
			out = append(out, r.compactionBlock(s.TurnID, dashi.Stop{}))
			break
		}
		out = append(out, Block{
			ID:   "u:" + s.TurnID,
			Kind: KindUser,
			Text: joinMessages(s.Started.Input),
			Done: true,
		})
	case dashi.StepModelIntent:
		out = append(out, r.activeContextIntent(s)...)
		out = append(out, modelPreparationBlocks(s)...)
	case dashi.StepModelSettled, dashi.StepModelReconciled:
		if r.isActiveSummaryFact(s.Model) {
			out = append(out, r.activeContextSettled(s)...)
			break
		}
		out = append(out, r.reasoningSettled(s)...)
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
		out = append(out, r.activeContextStopped(*s.Stop)...)
		if r.Origin == dashi.StartCompaction {
			out = append(out, r.compactionBlock(s.TurnID, *s.Stop))
		} else {
			out = append(out, r.stopped(s)...)
		}
	case dashi.StepContextCompacted:
		out = append(out, r.activeContextApplied(s)...)
	}
	for _, w := range s.Warnings {
		if block, ok := activeContextUnavailable(s, w); ok {
			out = append(out, block)
			continue
		}
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
	if r.Origin == dashi.StartCompaction {
		r.summary = text
		return nil
	}
	id := messageKey(m.MessageID, m.AttemptID)
	if text == "" {
		// A tool-only message has no text. If text streamed before it
		// settled, an empty settled block replaces that provisional text.
		if r.text[id] == "" {
			return nil
		}
		delete(r.text, id)
		return []Block{{ID: id, Kind: KindAssistant, Done: true}}
	}
	r.text[id] = text
	return []Block{{ID: id, Kind: KindAssistant, Text: text, Done: true}}
}

func (r *Renderer) compactionBlock(id string, stop dashi.Stop) Block {
	b := Block{
		ID:    "compaction:" + id,
		Kind:  KindCompaction,
		State: StateRunning,
		Text:  "Compacting history...",
	}
	switch stop.Kind {
	case "":
		if !stop.Projected {
			return b
		}
		b.State, b.Text = "interrupted", "Compaction interrupted. Press Enter to continue."
	case dashi.StopCompleted:
		if compactiontext.Summary(r.summary) != "" {
			b.State, b.Text = StateOK, "Compaction completed."
			break
		}
		b.State, b.Text = StateFailed, "Compaction failed: the summary was empty; previous history kept."
	case dashi.StopInterrupted:
		b.State, b.Text = "interrupted", "Compaction interrupted. Press Enter to continue."
	default:
		b.State = StateFailed
		b.Text = "Compaction " + string(stop.Kind) + "; previous history kept."
		if stop.Reason != "" {
			b.Text = "Compaction " + string(stop.Kind) + ": " + stop.Reason + "; previous history kept."
		}
	}
	b.Done = true
	return b
}

func (r *Renderer) compactionOutcome(v dashi.TurnView) Block {
	r.Origin, r.summary = v.Origin, v.Answer
	return r.compactionBlock(v.ID, v.Stop)
}

func (r *Renderer) toolCall(s dashi.Step) *callState {
	call := s.Tool.Call
	c := r.calls[call.ID]
	if c == nil {
		c = &callState{name: call.Name}
		r.calls[call.ID] = c
	}
	c.name = call.Name
	c.arg = mainArg(call.Args, r.Dir)
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
	if s.Tool.Outcome != nil {
		b.Detail = outputText(s.Tool.Outcome)
	}
	b.Lines = lines
	b.Done = state != StateWaiting
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
	if st.Limit != nil && st.Limit.Kind == dashi.LimitContextWindow {
		return "The next request cannot fit its recorded context window. Full work is retained. Use a larger-window model or smaller input for a new Turn; compact earlier session history when needed."
	}
	if st.Limit != nil && st.Limit.Kind == dashi.LimitModelAttempts {
		return fmt.Sprintf(
			"Stopped after %d model calls. Press Enter or type /continue to go on.",
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
	if v.Spend.UsageIncomplete {
		parts = append(parts, "usage incomplete")
	} else {
		tokens := v.Spend.Usage.TotalInputTokens() + v.Spend.Usage.OutputTokens
		parts = append(parts, groupDigits(tokens)+" tokens")
	}
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
			if v < 0.01 {
				out = append(out, fmt.Sprintf("$%.4f", v))
			} else {
				out = append(out, fmt.Sprintf("$%.3f", v))
			}
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
// its footer when the turn has ended. diffs holds the saved diffs of the
// turn by step sequence number. Each one shows after its step.
func RenderTurn(
	n int,
	model, dir string,
	turn dashi.Turn,
	diffs map[int64][]string,
) ([]Block, error) {
	view, err := dashi.Replay(turn)
	if err != nil {
		return nil, err
	}
	r := NewRenderer(n, model, dir)
	var out []Block
	for i := range turn.Steps {
		step := turn.Steps[i]
		out = append(out, r.Event(dashi.Event{
			TurnID: turn.ID,
			Seq:    step.Seq,
			Step:   &step,
		})...)
		if lines, ok := diffs[step.Seq]; ok {
			out = append(out, diffBlock(turn.ID, step.Seq, lines))
		}
	}
	if view.Stop.Kind != "" {
		out = append(out, r.activeContextStopped(view.Stop)...)
		out = append(out, r.Footer(view))
	}
	if view.Origin == dashi.StartCompaction {
		// Reopened output shows the current state, not historical progress.
		out = nil
		r.summary = view.Answer
		out = append(out, r.compactionBlock(view.ID, view.Stop))
		if view.Stop.Kind != "" {
			out = append(out, r.Footer(view))
		}
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
	case KindReasoning:
		return "[Reasoning]\n" + b.Text
	case KindCompaction:
		return "[" + b.Text + "]"
	case KindPending:
		if b.Done {
			return ""
		}
		return "  pending prompt: " + b.Text
	case KindTool:
		return formatTool(b)
	case KindDiff:
		return strings.Join(b.Lines, "\n")
	case KindSteer:
		switch b.State {
		case SteerDelivered:
			return "  steer delivered: " + b.Text
		case SteerNext:
			return "  queued for the next turn: " + b.Text
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
		left += "  " + fitArg(b.Arg, toolLineWidth-len(left)-2)
	}
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
// pattern, or other first string that names what the call works on. A path
// inside dir shows relative to dir.
func mainArg(raw json.RawMessage, dir string) string {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return ""
	}
	for _, key := range []string{
		"path", "file_path", "command", "pattern", "query", "url", "dir", "name",
	} {
		if s, ok := args[key].(string); ok && s != "" {
			if key == "path" || key == "file_path" || key == "dir" {
				s = relPath(dir, s)
			}
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

// relPath shows p relative to dir when p is inside dir.
func relPath(dir, p string) string {
	if dir == "" || !filepath.IsAbs(p) {
		return p
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return rel
}

// fitArg shortens arg to width. A path (no spaces) loses its left end, so the
// file name stays; anything else loses its right end.
func fitArg(arg string, width int) string {
	r := []rune(arg)
	if width < 8 || len(r) <= width {
		return arg
	}
	if !strings.ContainsAny(arg, " \t") {
		return "..." + string(r[len(r)-(width-3):])
	}
	return string(r[:width-3]) + "..."
}

// printable makes every text field of b safe to show in a terminal. Text that
// the model controls could otherwise carry escape sequences or carriage
// returns that rewrite the screen. Every UI receives blocks through this.
func printable(b Block) Block {
	b.Detail = workspace.PrintableText(b.Detail)
	b.Text = workspace.PrintableText(b.Text)
	b.Name = workspace.PrintableText(b.Name)
	b.Arg = workspace.PrintableText(b.Arg)
	if b.Lines != nil {
		lines := make([]string, len(b.Lines))
		for i, l := range b.Lines {
			lines[i] = workspace.PrintableText(l)
		}
		b.Lines = lines
	}
	return b
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
