package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// BlockKind names what a Block shows.
type BlockKind string

const (
	KindUser      BlockKind = "user"
	KindAssistant BlockKind = "assistant"
	KindReasoning BlockKind = "reasoning"
	KindTool      BlockKind = "tool"
	KindDiff      BlockKind = "diff"
	KindSteer     BlockKind = "steer"
	KindStop      BlockKind = "stop"
	KindFooter    BlockKind = "footer"
	KindNotice    BlockKind = "notice"
	// KindHint is a quiet line about the CLI's own state, such as
	// when task results will be sent. It draws muted and indented.
	KindHint       BlockKind = "hint"
	KindWarning    BlockKind = "warning"
	KindCompaction BlockKind = "compaction"
	KindPending    BlockKind = "pending"
	KindHelp       BlockKind = "help"
	// KindTaskStatus is the state of a delegated task. It draws under the
	// task tool line that started the task (see Block.Parent).
	KindTaskStatus BlockKind = "task-status"
	// KindTaskResult marks a task result that reached the parent agent. It
	// takes the place of the user line of a turn that the result started.
	KindTaskResult BlockKind = "task-result"
	// KindTaskMessage is a message between the parent agent and a task:
	// one the parent sent with task_send, or one a task sent with
	// message_parent that reached the parent. State holds its direction.
	KindTaskMessage BlockKind = "task-message"
)

// Tool line states.
const (
	StateRunning = "running"
	StateOK      = "ok"
	StateFailed  = "failed"
	StateDenied  = "denied"
	StateWaiting = "waiting"
	// StateChecking is a bash call in bypass mode while decide checks it,
	// and StateWaitingForYou one the check holds for the person's answer.
	// Neither has started.
	StateChecking      = "checking"
	StateWaitingForYou = "waiting for you"
	// StateCheckingResult is a call that finished while decide checks its
	// result, before the model reads it.
	StateCheckingResult = "checking result"
	// StateStopped is a task that was interrupted or canceled. It is
	// settled and waits on nobody.
	StateStopped = "stopped"
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
	// Label names a tool call in words, such as "Read render.go". The
	// screen's tool row shows it in place of Name and Arg.
	Label toolLabel
	// Input is a tool call's whole input, which an expanded tool row shows
	// (see toolInput).
	Input string
	// State is the tool or steer state, a message line's direction, or a
	// stop's dashi.StopKind.
	State   string
	Elapsed time.Duration
	// Lines holds detail lines: the last output of a running tool, or the
	// lines of a diff.
	Lines []string
	// Summary is the result line of a finished tool call, such as the
	// user's answer, "214 lines", or the last error line (see toolSummary).
	// Plain output shows it under the tool line; the screen does not.
	Summary string
	// Detail retains complete tool output for expansion and native scrollback.
	Detail string
	// Expanded shows a tool call's Input and whole output. Display-only.
	Expanded bool
	// Spinner is the spinner frame a running tool line shows in place of its
	// glyph. Only the live screen sets it. Display-only.
	Spinner string
	// Done marks a block that will not change again.
	Done bool
	// Footer holds the facts of a footer block, and Help the structure of a
	// help block. Text keeps the plain form of both.
	Footer *footerFacts
	Help   *helpDoc
	// Parent is the ID of the tool block a task status line belongs under.
	Parent string
	// Task is the task status line drawn under a tool line. Transcript sets
	// it when a KindTaskStatus block names this tool block as its Parent.
	Task *Block
	// Decide is the decide answer of a checked tool call, or nil.
	Decide *decideMark
	// DecideResult is the prompt-injection answer of a call's result, or
	// nil: a warning the agent got, or a check that failed.
	DecideResult *decideMark
	// Openable marks a task status line that opens a task pane on click.
	// Only the fullscreen view sets it, and draws a faint mark for it.
	Openable bool
}

// Renderer turns Dashi events into blocks. The screen, line mode, and the run
// command use it, and a resumed session replays step events through it, so
// the scrollback of a reopened session equals what the live run showed.
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
	activeContext map[string]*activeContextModelCall
}

type callState struct {
	name    string
	arg     string
	label   toolLabel
	input   string
	started time.Time
	denied  string
	decide  *decideMark
	result  *decideMark
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
	if r.Origin == dashi.StartCompaction || r.isActiveSummary(ev.ModelCallID) {
		return nil
	}
	if ev.Delta.Kind == llm.DeltaReasoning && r.showReasoning {
		return r.reasoningDelta(ev)
	}
	if ev.Delta.Kind != llm.DeltaText || ev.Delta.Text == "" {
		return nil
	}
	id := messageKey(ev.MessageID, ev.ModelCallID)
	r.text[id] += ev.Delta.Text
	return []Block{{ID: id, Kind: KindAssistant, Text: r.text[id]}}
}

func messageKey(messageID, modelCallID string) string {
	if messageID != "" {
		return "a:" + messageID
	}
	return "a:" + modelCallID
}

func (r *Renderer) progress(p *dashi.ToolProgress) []Block {
	c := r.calls[p.CallID]
	if c == nil {
		c = &callState{name: p.Name, started: time.Now(), label: newToolLabel(p.Name, nil, "")}
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
		ID:           "t:" + id,
		Kind:         KindTool,
		Name:         c.name,
		Arg:          c.arg,
		Label:        c.label,
		Input:        c.input,
		State:        state,
		Decide:       c.decide,
		DecideResult: c.result,
	}
}

func (r *Renderer) step(s dashi.Step) []Block {
	var out []Block
	switch s.Kind {
	case dashi.StepTurnStarted:
		r.Origin = s.Started.Origin
		r.showReasoning = s.Started.Agent.Settings.ReasoningSummary != nil && *s.Started.Agent.Settings.ReasoningSummary
		if m := s.Started.Agent.Model; m != "" {
			r.Model = m
		}
		if r.Origin == dashi.StartCompaction {
			out = append(out, r.compactionBlock(s.TurnID, dashi.Stop{}))
			break
		}
		// A turn that a task result or another agent message started has
		// no user text. Show what started it instead of an empty user line.
		markers := deliveredItemBlocks("r:"+s.TurnID, s.Started.Input)
		out = append(out, markers...)
		text := joinMessages(s.Started.Input)
		if text == "" && len(markers) > 0 {
			break
		}
		out = append(out, Block{
			ID:   "u:" + s.TurnID,
			Kind: KindUser,
			Text: text,
			Done: true,
		})
	case dashi.StepModelCallStarted:
		out = append(out, r.activeContextCallStart(s)...)
		out = append(out, modelPreparationBlocks(s)...)
	case dashi.StepModelCallFinished, dashi.StepModelCallFinishedByHost:
		if r.isActiveSummaryFact(s.Model) {
			out = append(out, r.activeContextFinished(s)...)
			break
		}
		out = append(out, r.reasoningFinished(s)...)
		out = append(out, r.modelFinished(s)...)
	case dashi.StepToolDecision:
		out = append(out, r.toolDecision(s)...)
	case dashi.StepApprovalDecided:
		r.approvalDecided(s.Approval)
	case dashi.StepToolStarted:
		out = append(out, r.toolStarted(s)...)
	case dashi.StepToolFinished:
		out = append(out, r.toolFinished(s)...)
	case dashi.StepToolWaiting:
		if c := r.calls[s.Tool.Call.ID]; c != nil {
			out = append(out, r.toolBlock(s.Tool.Call.ID, c, StateWaiting))
		}
	case dashi.StepMessagesAdded:
		if s.Context != nil && s.Context.Steering {
			for _, item := range s.Context.Items {
				if item.Message.Text() == "" {
					if markers := deliveredItemBlocks("s:"+item.ID, []llm.Message{item.Message}); len(markers) > 0 {
						out = append(out, markers...)
						continue
					}
				}
				out = append(out, Block{
					ID:    "s:" + item.ID,
					Kind:  KindSteer,
					Text:  item.Message.Text(),
					State: SteerDelivered,
					Done:  true,
				})
			}
		}
	case dashi.StepTurnStopped:
		out = append(out, r.activeContextStopped(*s.Stop)...)
		if r.Origin == dashi.StartCompaction {
			out = append(out, r.compactionBlock(s.TurnID, *s.Stop))
		} else {
			out = append(out, r.stopped(s)...)
		}
	case dashi.StepContextCompacted:
		out = append(out, r.activeContextApplied(s)...)
	}
	if hint, ok := watchHint(s); ok {
		out = append(out, hint)
	}
	for _, w := range s.Warnings {
		if isDecideHookWarning(w) {
			// The tool row or /settings shows it.
			continue
		}
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

func (r *Renderer) modelFinished(s dashi.Step) []Block {
	m := s.Model
	if m == nil || m.Result == nil || m.Interrupted != "" {
		return nil
	}
	text := m.Result.Message.Text()
	if r.Origin == dashi.StartCompaction {
		r.summary = text
		return nil
	}
	id := messageKey(m.MessageID, m.ModelCallID)
	if text == "" {
		// A tool-only message has no text. If text streamed before it
		// finished, an empty finished block replaces that provisional text.
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
		if !stop.Inferred {
			return b
		}
		b.State, b.Text = "interrupted", "Compaction interrupted. Press Enter to continue."
	case dashi.StopCompleted:
		if compactiontext.Summary(r.summary) != "" {
			b.State, b.Text = StateOK, "Compaction completed."
			break
		}
		b.State, b.Text = StateFailed, "Compaction failed: the summary was empty; earlier history shortened without a summary."
	case dashi.StopInterrupted:
		b.State, b.Text = "interrupted", "Compaction interrupted. Press Enter to continue."
	default:
		b.State = StateFailed
		b.Text = "Compaction " + string(stop.Kind) + "; earlier history shortened without a summary."
		if stop.Reason != "" {
			b.Text = "Compaction " + string(stop.Kind) + ": " + stop.Reason + "; earlier history shortened without a summary."
		}
	}
	b.Done = true
	return b
}

func (r *Renderer) compactionOutcome(v dashi.TurnView) Block {
	r.Origin, r.summary = v.Origin, v.Reply
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
	c.label = newToolLabel(call.Name, call.Args, r.Dir)
	c.input = toolInput(call.Name, call.Args, r.Dir)
	switch call.Name {
	case delegate.ToolName:
		c.arg = taskToolArg(call.Args, s.TurnID, call.ID)
	case delegate.SendToolName:
		c.arg = taskSendArg(call.Args)
	}
	return c
}

func (r *Renderer) toolDecision(s dashi.Step) []Block {
	c := r.toolCall(s)
	c.decide = markFromRecord(s.Tool.Verdict.RulesVersion, s.Tool.Verdict.RulesFinding)
	if s.Tool.Verdict.Kind == dashi.DecisionDeny {
		// The row shows the decide answer on its own line.
		c.denied = s.Tool.Verdict.Reason
	}
	return nil
}

// approvalDecided records a host decision given later with Resolve. A check
// recorded with the decision replaces the one from the tool decision.
func (r *Renderer) approvalDecided(a *dashi.ApprovalDecision) {
	if a == nil {
		return
	}
	c := r.calls[a.CallID]
	if c == nil {
		return
	}
	if m := markFromRecord(a.RulesVersion, a.RulesFinding); m != nil {
		c.decide = m
	}
	if !a.Approve {
		// The same sentence the model gets for a denial without a reason.
		c.denied = cmp.Or(a.Reason, "The call was denied.")
	}
}

func (r *Renderer) toolStarted(s dashi.Step) []Block {
	c := r.toolCall(s)
	c.started = s.At
	return []Block{r.toolBlock(s.Tool.Call.ID, c, StateRunning)}
}

func (r *Renderer) toolFinished(s dashi.Step) []Block {
	id := s.Tool.Call.ID
	c := r.toolCall(s)
	c.result = resultMark(s.HookResults)
	state := StateOK
	if o := s.Tool.Outcome; o != nil {
		switch o.State {
		case tool.StateFailed:
			state = StateFailed
		case tool.StateNotExecuted:
			state = StateDenied
		case tool.StateWaiting, tool.StateDetached:
			state = StateWaiting
		}
	}
	// A sent message shows as a message line, not a tool line.
	if c.name == delegate.SendToolName && state == StateOK {
		return []Block{taskSendBlock(id, s.Tool.Call.Args)}
	}
	if c.name == workspace.ToolWriteFile && state == StateOK && strings.HasPrefix(outputText(s.Tool.Outcome), "Created ") {
		c.label = c.label.created()
	}
	b := r.toolBlock(id, c, state)
	// A started task's output only instructs the model. The status line
	// under the tool line tells the user more.
	if s.Tool.Outcome != nil && (c.name != delegate.ToolName || state != StateOK) {
		b.Detail = outputText(s.Tool.Outcome)
	}
	b.Summary = toolSummary(c.name, s.Tool.Call.Args, state, outputText(s.Tool.Outcome), c.denied)
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
	case dashi.StopResultUnknown:
		text = "The turn stopped with an unknown result: " +
			orDefault(st.Reason, "unknown") + "."
	default:
		text = "The turn stopped: " + string(st.Kind) + "."
	}
	return []Block{{
		ID:    fmt.Sprintf("stop:%s:%d", s.TurnID, s.Seq),
		Kind:  KindStop,
		Text:  text,
		State: string(st.Kind),
		Done:  true,
	}}
}

func limitText(st *dashi.Stop) string {
	if st.Limit != nil && st.Limit.Kind == dashi.LimitContextWindow {
		return "The next request cannot fit its recorded context window. Full work is retained. Use a larger-window model or smaller input for a new Turn; compact earlier session history when needed."
	}
	if st.Limit != nil && st.Limit.Kind == dashi.LimitModelCalls {
		return fmt.Sprintf(
			"Stopped after %d model calls. Press Enter or type /continue to go on.",
			st.Limit.Used,
		)
	}
	return "Stopped at a limit: " + orDefault(st.Reason, "unknown") + "."
}

// Footer returns the line that closes a finished turn.
// steps are the turn's steps, from which the footer counts the decide
// answers that hooks recorded.
func (r *Renderer) Footer(v dashi.TurnView, steps []dashi.Step) Block {
	f := newFooterFacts(r.Turn, r.Model, v, steps)
	return Block{
		ID:     fmt.Sprintf("footer:%s", v.ID),
		Kind:   KindFooter,
		Text:   f.String(),
		Footer: &f,
		Done:   true,
	}
}

// footerFacts is the turn summary: number, model, model calls, tokens, and
// cost. The screen styles each value; plain output joins them.
type footerFacts struct {
	Turn  int
	Model string
	Calls int
	// Tokens is meaningless when UsageIncomplete is set.
	Tokens          int
	UsageIncomplete bool
	Cost            string
	// DecideChecked, DecideFlagged, and DecideUnchecked count the turn's
	// calls and results that decide checked, flagged, and could not
	// check, as the record keeps them. DecideReminded counts the turn
	// watcher's reminders.
	DecideChecked, DecideFlagged, DecideUnchecked, DecideReminded int
}

func newFooterFacts(turn int, model string, v dashi.TurnView, steps []dashi.Step) footerFacts {
	checked, flagged, unchecked := decideCounts(v)
	rChecked, rFlagged, rUnchecked, reminded := decideStepCounts(steps)
	return footerFacts{
		DecideChecked:   checked + rChecked,
		DecideFlagged:   flagged + rFlagged,
		DecideUnchecked: unchecked + rUnchecked,
		DecideReminded:  reminded,
		Turn:            turn,
		Model:           model,
		Calls:           v.ModelCalls,
		Tokens:          v.Spend.Usage.TotalInputTokens() + v.Spend.Usage.OutputTokens,
		UsageIncomplete: v.Spend.UsageIncomplete,
		Cost:            costText(v.Spend),
	}
}

// footerPart is one value of the footer. Meta values are quiet; the screen
// emphasizes the rest by kind.
type footerPart struct {
	text string
	kind footerKind
	// drop orders the parts a narrow screen leaves out, highest first. Zero
	// is never left out.
	drop int
}

type footerKind int

const (
	footerMeta footerKind = iota
	footerValue
	footerCost
	footerWarn
)

func (f footerFacts) parts() []footerPart {
	parts := []footerPart{{fmt.Sprintf("turn %d", f.Turn), footerMeta, 0}}
	if f.Model != "" {
		parts = append(parts, footerPart{f.Model, footerValue, 3})
	}
	parts = append(parts, footerPart{fmt.Sprintf("%d model %s", f.Calls, plural(f.Calls, "call")), footerMeta, 4})
	if f.UsageIncomplete {
		parts = append(parts, footerPart{"usage incomplete", footerWarn, 0})
	} else {
		parts = append(parts, footerPart{groupDigits(f.Tokens) + " tokens", footerValue, 2})
	}
	if f.Cost != "" {
		parts = append(parts, footerPart{f.Cost, footerCost, 1})
	}
	if f.DecideChecked+f.DecideUnchecked+f.DecideReminded > 0 {
		kind := footerMeta
		if f.DecideFlagged > 0 {
			kind = footerWarn
		}
		parts = append(parts, footerPart{f.decideText(), kind, 0})
	}
	return parts
}

// decideText is the footer's decide counts, such as "decide 4 checked, 1
// flagged" or "decide 1 reminder". A zero count is left out.
func (f footerFacts) decideText() string {
	var counts []string
	if f.DecideChecked > 0 {
		counts = append(counts, fmt.Sprintf("%d checked", f.DecideChecked))
	}
	if f.DecideFlagged > 0 {
		counts = append(counts, fmt.Sprintf("%d flagged", f.DecideFlagged))
	}
	if f.DecideUnchecked > 0 {
		counts = append(counts, fmt.Sprintf("%d unchecked", f.DecideUnchecked))
	}
	if f.DecideReminded > 0 {
		counts = append(counts, fmt.Sprintf("%d %s", f.DecideReminded, plural(f.DecideReminded, "reminder")))
	}
	return "decide " + strings.Join(counts, ", ")
}

// String is the plain form: "── turn 3 · model · 2 model calls · ...".
func (f footerFacts) String() string {
	var out strings.Builder
	out.WriteString(footerRule)
	for i, p := range f.parts() {
		if i == 0 {
			out.WriteString(" ")
		} else {
			out.WriteString(footerSep)
		}
		out.WriteString(p.text)
	}
	return out.String()
}

const (
	footerRule = "──"
	footerSep  = " · "
)

// FooterText formats the turn summary as plain text.
func FooterText(turn int, model string, v dashi.TurnView, steps []dashi.Step) string {
	return newFooterFacts(turn, model, v, steps).String()
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
	view, err := turn.View()
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
		out = append(out, r.Footer(view, turn.Steps))
	}
	if view.Origin == dashi.StartCompaction {
		// Reopened output shows the current state, not historical progress.
		out = nil
		r.summary = view.Reply
		out = append(out, r.compactionBlock(view.ID, view.Stop))
		if view.Stop.Kind != "" {
			out = append(out, r.Footer(view, turn.Steps))
		}
	}
	return out, nil
}

// Transcript holds blocks in order and applies replacements by ID.
type Transcript struct {
	Blocks []Block
	index  map[string]int
	// toolDone holds the own Done of each tool block that has a task line.
	// Such a block is done only when its task line is done too.
	toolDone map[string]bool
}

// Apply adds b, or replaces the block that has the same ID. It returns the
// index of the block. A task status block whose Parent tool block is in the
// transcript goes under that block instead (see Block.Task).
func (t *Transcript) Apply(b Block) int {
	if b.Kind == KindTaskStatus && b.Parent != "" {
		if i, ok := t.index[b.Parent]; ok && t.Blocks[i].Kind == KindTool {
			if t.toolDone == nil {
				t.toolDone = map[string]bool{}
			}
			if _, seen := t.toolDone[b.Parent]; !seen {
				t.toolDone[b.Parent] = t.Blocks[i].Done
			}
			task := b
			t.Blocks[i].Task = &task
			t.Blocks[i].Done = t.toolDone[b.Parent] && b.Done
			return i
		}
	}
	if b.ID != "" {
		if i, ok := t.index[b.ID]; ok {
			if old := t.Blocks[i]; b.Kind == KindTool && old.Task != nil && b.Task == nil {
				t.toolDone[b.ID] = b.Done
				b.Task = old.Task
				b.Done = b.Done && old.Task.Done
			}
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

// truncate keeps the first n blocks and drops the rest.
func (t *Transcript) truncate(n int) {
	if n < 0 || n >= len(t.Blocks) {
		return
	}
	t.Blocks = t.Blocks[:n:n]
	t.index = map[string]int{}
	for i, b := range t.Blocks {
		if b.ID != "" {
			t.index[b.ID] = i
		}
	}
	for id := range t.toolDone {
		if _, ok := t.index[id]; !ok {
			delete(t.toolDone, id)
		}
	}
}

// FormatBlock returns the plain-text form of a block, without color, for line
// mode, the run command, and tests. Multi-line blocks join with newlines.
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
	case KindHint:
		return "  " + strings.ReplaceAll(b.Text, "\n", "\n  ")
	case KindTaskStatus, KindTaskResult:
		return taskLineText(b)
	case KindTaskMessage:
		return taskMessageText(b)
	}
	return b.Text
}

const toolLineWidth = 58

// formatTool is the plain form of a tool line: the name, argument, and
// state, then the decide lines and the result line, such as
//
//	bash  go test ./...                                        12.4s
//	  └ ok  github.com/deepnoodle-ai/dashi  3.1s
func formatTool(b Block) string {
	left := "  " + b.Name
	if b.Arg != "" {
		left += "  " + fitArg(b.Arg, toolLineWidth-len(left)-2)
	}
	elapsed := elapsedText(b.Elapsed)
	right := b.State
	switch b.State {
	case StateOK:
		right = elapsed
	case StateFailed:
		right += "  " + elapsed
	case StateRunning:
		right = "..."
	case StateChecking:
		right = "decide: checking command…"
	case StateCheckingResult:
		right = "decide: checking result…"
	}
	if tick := decideTicks(b); tick != "" {
		right += "  " + tick
	}
	line := fmt.Sprintf("%-*s %s", toolLineWidth, left, right)
	for _, m := range decideLines(b) {
		line += "\n    " + m.Text
	}
	if body := toolBody(b); body != "" {
		line += "\n    " + toolResultMarker + strings.ReplaceAll(body, "\n", "\n      ")
	}
	if b.Task != nil {
		line += "\n" + taskLineUnderText(*b.Task)
	}
	return line
}

// toolBody is the text under a tool line in plain output: the whole
// output when the line is expanded, else the result line, else the last
// output lines of a running call.
func toolBody(b Block) string {
	switch {
	case b.Expanded && b.Detail != "":
		return strings.TrimRight(b.Detail, "\n")
	case b.Summary != "":
		return b.Summary
	}
	return strings.Join(b.Lines, "\n")
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
	b.Summary = workspace.PrintableText(b.Summary)
	b.Name = workspace.PrintableText(b.Name)
	b.Arg = workspace.PrintableText(b.Arg)
	b.Label.Running = workspace.PrintableText(b.Label.Running)
	b.Label.Done = workspace.PrintableText(b.Label.Done)
	b.Input = workspace.PrintableText(b.Input)
	if b.Footer != nil {
		f := *b.Footer
		f.Model, f.Cost = workspace.PrintableText(f.Model), workspace.PrintableText(f.Cost)
		b.Footer = &f
	}
	if b.Help != nil {
		h := b.Help.printable()
		b.Help = &h
	}
	if b.Task != nil {
		task := printable(*b.Task)
		b.Task = &task
	}
	if b.Decide != nil {
		mark := *b.Decide
		mark.Text = workspace.PrintableText(mark.Text)
		b.Decide = &mark
	}
	if b.DecideResult != nil {
		mark := *b.DecideResult
		mark.Text = workspace.PrintableText(mark.Text)
		b.DecideResult = &mark
	}
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

// elapsedText is a tool call's run time in tenths of a second. A call
// that took under a tenth shows "<0.1s" rather than "0.0s".
func elapsedText(d time.Duration) string {
	if d < 100*time.Millisecond {
		return "<0.1s"
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
