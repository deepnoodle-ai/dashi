// Package delegate lets one turn hand a focused task to a child turn. It
// provides the "task" tool: the model names a subagent and gives a prompt,
// the tool runs a child turn to the end, and the child's final answer
// becomes the tool result.
//
// The child is a full Dashi turn with its own record. The record names the
// parent tool call (dashi.Start.Parent), and the child's ID comes from that
// call (ChildID), so a retry after a crash finds the same child and never
// starts a second one. Report adds the children's spend to the parent's
// without mixing child model cost into tool cost.
//
// A child inherits from the runner that runs the task call, at call time: the
// client, recorder, hooks, and cancel behavior, and the parent's current
// model unless the Subagent sets its own. The host therefore changes any of
// them for the parent and the children follow. Hosts wire it in one line:
//
//	tools = tool.Join(tools, delegate.New(delegate.Config{OnChild: draw}))
package delegate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// ToolName is the name of the tool that New provides.
const ToolName = "task"

// defaultMaxAnswer is the byte cap on an answer returned to the parent when
// Config.MaxAnswer is zero. A longer answer would refill the context the
// parent delegated to protect.
const defaultMaxAnswer = 16 << 10

// Subagent is a kind of child that the model can choose.
type Subagent struct {
	// Name is what the model passes as "subagent", such as "explore". It
	// also becomes the child's Agent.ID when that is empty, so a hook that
	// reads TurnView.Agent.ID can tell which subagent asks for a tool.
	Name string
	// Description tells the model when to choose this subagent.
	Description string
	// Agent holds the instructions, model, and limits of the child turn.
	// Limits bound the child's work; delegate adds no timeout of its own.
	Agent dashi.Agent
	// Tools names the parent tools the child may use. Empty means every
	// parent tool. The child never receives the task tool, so a child
	// cannot start a child.
	Tools []string
	// ReadOnly limits the child to tools annotated read-only. It applies
	// with Tools. A tool without the annotation is excluded, because an
	// unknown effect is not a safe one.
	ReadOnly bool
}

// Explore returns a read-only subagent for searching and reading. It uses
// only tools annotated read-only, so MCP tools that declare the hint are
// included and the rest are not.
func Explore() Subagent {
	return Subagent{
		Name: "explore",
		Description: "Searches and reads to answer a question about the " +
			"codebase. It cannot change anything. Use it when the answer " +
			"needs many reads that would fill your context.",
		Agent: dashi.Agent{
			Instructions: "You are a read-only research subagent. Search " +
				"and read to answer the request. Do not change anything. " +
				"Your final message is the only thing the parent agent " +
				"sees. Make it short. Name the files and lines that matter.",
			Limits: dashi.Limits{ModelAttempts: 24, ToolConcurrency: 4},
		},
		ReadOnly: true,
	}
}

// General returns a subagent that can use every parent tool. Use it for
// multi-step work that the parent does not need to watch.
func General() Subagent {
	return Subagent{
		Name: "general",
		Description: "Completes a multi-step task with every tool you have. " +
			"Use it for work that needs many steps and only one result.",
		Agent: dashi.Agent{
			Instructions: "You are a subagent. Complete the task in the " +
				"request. Your final message is the only thing the parent " +
				"agent sees. State the result and each change you made.",
			Limits: dashi.Limits{ModelAttempts: 32},
		},
	}
}

// Config configures the task tool. The zero value works.
type Config struct {
	// Subagents is the catalog the model chooses from. Empty means Explore
	// and General.
	Subagents []Subagent
	// Runner holds optional overrides for child turns. Each child inherits
	// from the runner that runs the task call. Set Client, Recorder,
	// OnContextDone, or Observer here only to make children differ from the
	// parent; a nonzero field replaces the inherited value. Hooks are always
	// inherited, so the host's approval prompt covers child tool calls. Agent
	// and Tools are ignored. A Recorder should implement dashi.Loader:
	// without it a crash can lead to a second child (see New).
	Runner dashi.Runner
	// MaxAnswer caps the bytes of the child's answer that reach the parent.
	// Zero means 16 KiB; a negative value means no cap.
	MaxAnswer int
	// OnChild receives every child event for live display: committed steps,
	// model deltas, and tool progress. The Observer in Runner, if set, also
	// receives each event, after OnChild. The parent's Observer does not.
	// It runs on the child's observer goroutine and must return promptly.
	OnChild func(ChildEvent)
}

// ChildEvent is one event of a child turn, with the link to its parent.
type ChildEvent struct {
	// Parent is the parent turn and the task call that runs the child.
	Parent dashi.CallRef
	// Subagent is the catalog name the model chose.
	Subagent string
	// Event is the child's own event; Event.TurnID is the child turn.
	Event dashi.Event
}

// SpendReport splits spend between a parent turn and its children.
type SpendReport struct {
	// Parent is the parent turn's own spend.
	Parent dashi.Spend
	// Children is the sum over the children the parent's task calls started.
	Children dashi.Spend
	// Inclusive is Parent plus Children, each counted once.
	Inclusive dashi.Spend
}

// ChildID returns the ID of the child turn for one parent tool call. The
// result depends only on the two arguments, and different pairs give
// different IDs, so the host stores nothing and a retry finds the same child.
// A host that wants its own ID can call Runner.Run with Start itself.
func ChildID(parentTurnID, callID string) string {
	// The length prefix keeps ("a", "b:c") apart from ("a:b", "c").
	return fmt.Sprintf(
		"child-%d:%s:%s",
		len(parentTurnID),
		parentTurnID,
		callID,
	)
}

// Report sums the spend of parent and of the children its task calls started.
// It finds children only through those calls, by ChildID: a child that a host
// started with its own ID is not counted. Each child counts once, however
// many times it resumed. Parent comes from parent.Spend, which never holds
// child spend.
func Report(
	ctx context.Context,
	r dashi.Loader,
	parent dashi.TurnView,
) (SpendReport, error) {
	report := SpendReport{Parent: parent.Spend.Clone()}
	for _, callID := range slices.Sorted(maps.Keys(parent.Tools)) {
		if parent.Tools[callID].Call.Name != ToolName {
			continue
		}
		turn, err := r.Load(ctx, ChildID(parent.ID, callID))
		if errors.Is(err, dashi.ErrTurnNotFound) {
			continue
		}
		if err != nil {
			return SpendReport{}, fmt.Errorf("delegate: load child: %w", err)
		}
		view, err := dashi.Replay(turn)
		if err != nil {
			return SpendReport{}, fmt.Errorf("delegate: replay child: %w", err)
		}
		report.Children = report.Children.Add(view.Spend)
	}
	report.Inclusive = report.Parent.Add(report.Children)
	return report, nil
}

// New returns the toolset that holds the task tool. Join it with the tools
// the parent already has. Children draw from the tools of the runner that runs
// the call, filtered by each Subagent; they never receive the task tool.
//
// The tool is marked idempotent. After a crash the runner runs the call
// again, and the same child continues from its record. This needs a recorder
// that implements dashi.Loader. The recorder is known only at call time, so
// when it is not a Loader the tool logs one warning on first use, and a
// crash can then lead to a second child. When cfg.Runner.Recorder is set and
// is not a Loader, New knows this at once, warns, and leaves the mark off.
func New(cfg Config) tool.Toolset {
	catalog := cfg.Subagents
	if len(catalog) == 0 {
		catalog = []Subagent{Explore(), General()}
	}
	t := &taskTools{cfg: cfg, catalog: slices.Clone(catalog), resumable: true}
	if cfg.Runner.Recorder != nil {
		if _, ok := cfg.Runner.Recorder.(dashi.Loader); !ok {
			t.resumable = false
			t.warnNoLoader()
		}
	}
	return t
}

type taskTools struct {
	cfg       Config
	catalog   []Subagent
	resumable bool
	warn      sync.Once
}

func (t *taskTools) warnNoLoader() {
	t.warn.Do(func() {
		slog.Warn(
			"delegate: recorder is not a dashi.Loader; a retry after a " +
				"crash can start a second child",
		)
	})
}

// Tools returns the task tool.
func (t *taskTools) Tools(context.Context) ([]tool.Tool, error) {
	names := make([]string, len(t.catalog))
	var list strings.Builder
	for i, s := range t.catalog {
		if s.Name == "" {
			return nil, errors.New("delegate: subagent requires a name")
		}
		if slices.Contains(names[:i], s.Name) {
			return nil, fmt.Errorf("delegate: subagent %q is listed twice", s.Name)
		}
		names[i] = s.Name
		fmt.Fprintf(&list, "\n- %s: %s", s.Name, s.Description)
	}
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subagent": map[string]any{
				"type":        "string",
				"enum":        names,
				"description": "Which subagent runs the task.",
			},
			"prompt": map[string]any{
				"type": "string",
				"description": "The complete task. The subagent cannot see " +
					"this conversation, so include every fact it needs.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "A label of a few words for display.",
			},
		},
		"required": []string{"subagent", "prompt", "description"},
	})
	if err != nil {
		return nil, err
	}
	def := tool.Definition{
		Name: ToolName,
		Description: "Hand a focused task to a subagent. The subagent starts " +
			"with no memory of this conversation and returns one answer. " +
			"Use it to keep long searches out of your own context. " +
			"Subagents:" + list.String(),
		Schema: schema,
		Kind:   tool.KindLocal,
	}
	if t.resumable {
		def.Annotations.Idempotent = new(true)
	}
	return []tool.Tool{{Definition: def, Executor: t}}, nil
}

type taskArgs struct {
	Subagent    string `json:"subagent"`
	Prompt      string `json:"prompt"`
	Description string `json:"description"`
}

// Execute runs the child turn for one task call and returns its answer.
func (t *taskTools) Execute(
	ctx context.Context,
	call tool.Call,
) (tool.Outcome, error) {
	var args taskArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return tool.Outcome{}, fmt.Errorf("delegate: read arguments: %w", err)
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return tool.Outcome{}, errors.New("delegate: prompt is required")
	}
	sub, ok := t.find(args.Subagent)
	if !ok {
		return tool.Outcome{}, fmt.Errorf(
			"delegate: unknown subagent %q; choose one of: %s",
			args.Subagent,
			strings.Join(t.names(), ", "),
		)
	}
	ref, ok := dashi.CurrentCall(ctx)
	if !ok {
		return tool.Outcome{}, tool.Fatal(errors.New(
			"delegate: the task tool must run under a dashi.Runner",
		))
	}
	parent, _ := dashi.CurrentRunner(ctx)
	id := ChildID(ref.TurnID, ref.CallID)
	runner := t.childRunner(sub, ref, parent)
	if _, ok := runner.Recorder.(dashi.Loader); !ok {
		t.warnNoLoader()
	}
	turn, command, saved, err := t.plan(ctx, runner, id, ref, args.Prompt)
	if err != nil {
		return tool.Outcome{}, t.failure(ctx, err)
	}
	out := saved
	if command != nil {
		out, err = runner.Run(ctx, turn, command)
		if err != nil {
			return tool.Outcome{}, t.failure(ctx, err)
		}
	}
	return t.answer(sub.Name, out)
}

// plan decides what the call does about the child: start it, continue it, or
// use its saved result. A saved child that stopped for a reason other than an
// interruption or a crash is not run again, because a retry after a crash must not pay
// for the same failed work twice.
func (t *taskTools) plan(
	ctx context.Context,
	runner dashi.Runner,
	id string,
	ref dashi.CallRef,
	prompt string,
) (dashi.Turn, dashi.Command, dashi.Outcome, error) {
	start := dashi.Start{
		CommandID: id + "/start",
		Parent:    &ref,
		Input:     []llm.Message{llm.UserMessage(llm.Text(prompt))},
	}
	loader, ok := runner.Recorder.(dashi.Loader)
	if !ok {
		return dashi.Turn{ID: id}, start, dashi.Outcome{}, nil
	}
	turn, err := loader.Load(ctx, id)
	if errors.Is(err, dashi.ErrTurnNotFound) {
		return dashi.Turn{ID: id}, start, dashi.Outcome{}, nil
	}
	if err != nil {
		return dashi.Turn{}, nil, dashi.Outcome{}, err
	}
	view, err := dashi.Replay(turn)
	if err != nil {
		return dashi.Turn{}, nil, dashi.Outcome{}, err
	}
	// A child with no stop yet was cut off before its first run recorded one.
	if view.Stop.Kind != "" && view.Stop.Kind != dashi.StopInterrupted {
		return turn, nil, dashi.Outcome{Turn: turn, TurnView: view}, nil
	}
	return turn, dashi.Continue{}, dashi.Outcome{}, nil
}

// childRunner builds the runner for one child. It starts from the runner that
// runs the task call, applies the host's overrides, and replaces the agent and
// tools. A Subagent that sets no model uses the parent's current model.
func (t *taskTools) childRunner(
	sub Subagent,
	ref dashi.CallRef,
	parent dashi.Runner,
) dashi.Runner {
	over := t.cfg.Runner
	runner := parent
	if over.Client != nil {
		runner.Client = over.Client
	}
	if over.Recorder != nil {
		runner.Recorder = over.Recorder
	}
	if over.OnContextDone != "" {
		runner.OnContextDone = over.OnContextDone
	}
	agent := sub.Agent
	if agent.Model == "" && agent.Provider == "" && agent.API == "" {
		agent.Provider = parent.Agent.Provider
		agent.API = parent.Agent.API
		agent.Model = parent.Agent.Model
	}
	agent.ID = cmp.Or(agent.ID, sub.Name)
	runner.Agent = agent
	runner.Tools = filtered{parent: parent.Tools, sub: sub}
	template := over.Observer
	onChild := t.cfg.OnChild
	runner.Observer = nil
	if onChild != nil || template != nil {
		runner.Observer = func(e dashi.Event) {
			if onChild != nil {
				onChild(ChildEvent{Parent: ref, Subagent: sub.Name, Event: e})
			}
			if template != nil {
				template(e)
			}
		}
	}
	return runner
}

// failure turns an error from loading or running the child into the call's
// error. A context that ended is not a fault; the runner records that stop
// itself. Any other error is a record or client fault the model cannot fix,
// so it stops the parent turn and leaves the child resumable by its ID.
func (t *taskTools) failure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return tool.Fatal(fmt.Errorf("delegate: run subagent: %w", err))
}

// answer maps a child's stop to the parent call's result. Only a completed
// child gives an answer. An interrupted child gives an error the runner
// discards when the parent's context has ended, which leaves the call open
// so a resume continues the same child.
func (t *taskTools) answer(
	name string,
	out dashi.Outcome,
) (tool.Outcome, error) {
	switch stop := out.Stop; stop.Kind {
	case dashi.StopCompleted:
		text := out.Answer
		if strings.TrimSpace(text) == "" {
			text = "The subagent finished without a written answer."
		}
		return tool.Outcome{
			State:  tool.StateSucceeded,
			Output: new(tool.TextOutput(t.capAnswer(text))),
		}, nil
	case dashi.StopWaiting:
		return tool.Outcome{}, errors.New(
			"subagent needs approval that was deferred; " +
				"this host cannot answer it inline",
		)
	default:
		return tool.Outcome{}, fmt.Errorf(
			"subagent %s stopped: %s%s",
			name,
			stop.Kind,
			reasonSuffix(stop.Reason),
		)
	}
}

func (t *taskTools) capAnswer(text string) string {
	limit := cmp.Or(t.cfg.MaxAnswer, defaultMaxAnswer)
	if limit < 0 || len(text) <= limit {
		return text
	}
	cut := text[:limit]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return fmt.Sprintf("%s\n[answer cut at %d bytes]", cut, len(cut))
}

func (t *taskTools) find(name string) (Subagent, bool) {
	i := slices.IndexFunc(t.catalog, func(s Subagent) bool {
		return s.Name == name
	})
	if i < 0 {
		return Subagent{}, false
	}
	return t.catalog[i], true
}

func (t *taskTools) names() []string {
	names := make([]string, len(t.catalog))
	for i, s := range t.catalog {
		names[i] = s.Name
	}
	return names
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// filtered is the child's view of the parent toolset. It resolves the parent
// on every model call, so a dynamic toolset such as an MCP server keeps its
// live behavior inside the child.
type filtered struct {
	parent tool.Toolset
	sub    Subagent
}

func (f filtered) Tools(ctx context.Context) ([]tool.Tool, error) {
	if f.parent == nil {
		return nil, nil
	}
	all, err := f.parent.Tools(ctx)
	if err != nil {
		return nil, err
	}
	var out []tool.Tool
	for _, t := range all {
		name := t.Definition.Name
		readOnly := t.Definition.Annotations.ReadOnly
		switch {
		case name == ToolName:
		case len(f.sub.Tools) > 0 && !slices.Contains(f.sub.Tools, name):
		case f.sub.ReadOnly && (readOnly == nil || !*readOnly):
		default:
			out = append(out, t)
		}
	}
	return out, nil
}
