package dashi

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

type replayState struct {
	contextCache      *contextValidationCache
	source            Turn
	pendingCompaction string
	// compactionUsed holds an applied summary's guard until ordinary work
	// succeeds. Rebuilding the view keeps the guard across interruption
	// and reload.
	compactionUsed bool
	TurnView
	// modelCalls counts every requested call so retried calls get distinct IDs.
	modelCalls            int
	modelCallStart        string
	unresolvedModel       string
	unresolvedPartial     bool
	unresolvedInterrupted bool
	startedModelCalls     map[string]*ModelFact
	// modelOrder holds model call IDs in record order.
	modelOrder             []string
	modelFinishes          map[string]*ModelFact
	modelMessages          map[string]string
	modelCallOfTool        map[string]string
	latestModelCall        string
	finalReplyOnly         bool
	modelCallCap           int
	modelDefs              map[string]tool.Definition
	callOrder              []string
	providerTool           map[string]llm.ProviderToolApprovalRequest
	providerToolOrder      []string
	providerToolModelCalls map[string]string
	commandHashes          map[string]string
	latestResult           *llm.Result
	awaitingClassification bool
	failedModel            string
	// activeHooks is the ActiveHooks of the latest command.
	activeHooks *ActiveHooks
	// answerVetoed is set while the latest reply is one a BeforeFinish hook
	// vetoed; see ContextFact.Vetoed.
	answerVetoed bool
	// pausedProviderTool is set while the latest actionable result is a
	// provider pause that left a provider tool call unfinished. The next
	// request must end with that result so the provider can finish the
	// call, so no steer may follow it yet.
	pausedProviderTool bool
	// deliveredOnKept sizes the context added while a final text reply
	// was kept by a stop; see keptFinishCandidate.
	deliveredOnKept int
	// contextIDs maps each recorded context item ID to its item, so an
	// item added twice is recognized.
	contextIDs map[string]ContextItem
	// liveSeq holds the step that recorded each live item, and
	// callStartSeq the step that recorded each model call's start, so a
	// NextCallOnly item expires only after a later call is sent.
	liveSeq      map[string]int64
	callStartSeq map[string]int64
}

// View checks the turn record and rebuilds what it means now: its stop,
// reply, spend, and pending work. It only reads the record; it never
// runs a model call or a tool.
func (t Turn) View() (TurnView, error) {
	s, e := replay(t)
	if e != nil {
		return TurnView{}, e
	}
	view := snapshotView(s.TurnView)
	view.Stop = s.inferredStop()
	return view, nil
}

func snapshotView(v TurnView) TurnView {
	b, _ := json.Marshal(v)
	var out TurnView
	_ = json.Unmarshal(b, &out)
	for id, tv := range v.Tools {
		if tv.result != nil {
			copied := out.Tools[id]
			copied.result = new(llmclone.ToolResult(*tv.result))
			if tv.shortened != nil {
				copied.shortened = new(*tv.shortened)
			}
			out.Tools[id] = copied
		}
	}
	if len(v.live) > 0 {
		out.live = make(map[string]ContextItem, len(v.live))
		for k, item := range v.live {
			out.live[k] = cloneContextItem(item)
		}
	}
	out.acted = maps.Clone(v.acted)
	out.usedContextIDs = maps.Clone(v.usedContextIDs)
	return out
}

func replay(t Turn) (*replayState, error) {
	return replayCached(t, &contextValidationCache{prefixes: map[int64][]contextMessage{}})
}

func replayCached(t Turn, cache *contextValidationCache) (*replayState, error) {
	if cache.states == nil {
		cache.states = map[int64]*replayState{}
	}
	if cache.needed == nil {
		cache.needed = map[int64]bool{}
	}
	for _, step := range t.Steps {
		if step.Kind == StepModelCallStarted && step.Model != nil && step.Model.Compaction != nil {
			cache.needed[step.Model.Compaction.Through] = true
		}
	}
	s := &replayState{
		TurnView:               TurnView{ID: t.ID, Tools: map[string]ToolView{}},
		source:                 t,
		contextCache:           cache,
		modelDefs:              map[string]tool.Definition{},
		providerTool:           map[string]llm.ProviderToolApprovalRequest{},
		providerToolModelCalls: map[string]string{},
		commandHashes:          map[string]string{},
		startedModelCalls:      map[string]*ModelFact{},
		modelFinishes:          map[string]*ModelFact{},
		modelMessages:          map[string]string{},
		modelCallOfTool:        map[string]string{},
	}
	if t.ID == "" {
		return nil, fmt.Errorf("%w: empty ID", ErrInvalidTurn)
	}
	for i, step := range t.Steps {
		if _, err := json.Marshal(step); err != nil {
			return nil, fmt.Errorf(
				"%w: step %d cannot serialize: %v",
				ErrInvalidTurn,
				i+1,
				err,
			)
		}
		if step.Version != 1 || step.TurnID != t.ID || step.Seq != int64(i+1) {
			return nil, fmt.Errorf("%w: invalid step header at %d", ErrInvalidTurn, i+1)
		}
		if err := validateStepPayload(step); err != nil {
			return nil, fmt.Errorf("%w: step %d: %v", ErrInvalidTurn, i+1, err)
		}
		if step.CommandID != "" {
			if step.CommandHash == "" {
				return nil, fmt.Errorf("%w: command hash missing", ErrInvalidTurn)
			}
			if old, ok := s.commandHashes[step.CommandID]; ok &&
				old != step.CommandHash {
				return nil, ErrCommandIDReused
			}
			s.commandHashes[step.CommandID] = step.CommandHash
		}
		if i == 0 && step.Kind != StepTurnStarted {
			return nil, fmt.Errorf("%w: first step is not start", ErrInvalidTurn)
		}
		if i > 0 && step.Kind == StepTurnStarted {
			return nil, fmt.Errorf("%w: duplicate start", ErrInvalidTurn)
		}
		if err := s.apply(step); err != nil {
			return nil, fmt.Errorf("%w: step %d: %v", ErrInvalidTurn, i+1, err)
		}
		s.Warnings = append(s.Warnings, step.Warnings...)
		if recordsActiveHooks(step) {
			s.activeHooks = step.ActiveHooks
		}
		s.trackContext(step)
		s.trackHooks(step)
		if err := s.trackIDs(step); err != nil {
			return nil, fmt.Errorf("%w: step %d: %v", ErrInvalidTurn, i+1, err)
		}
		if !s.awaitingClassification {
			s.deliveredOnKept = 0
		}
		s.LastSeq = step.Seq
		if cache.needed[step.Seq] {
			cache.states[step.Seq] = s.viewSnapshot()
		}
	}
	// Derive ArgsHash from the final status, so no transition out of
	// approval can leave a stale hash behind.
	for id, v := range s.Tools {
		v.ArgsHash = ""
		if v.Status == ToolApprovalPending {
			v.ArgsHash = ArgsHash(v.Call.Args)
		}
		s.Tools[id] = v
	}
	s.Pending = s.pendingItems()
	s.Reply = answerText(t.Steps)
	// A vetoed reply never reaches the host through Reply.
	if s.answerVetoed {
		s.Reply = ""
	}
	if err := s.calculateSpend(); err != nil {
		return nil, fmt.Errorf("%w: calculate spend: %v", ErrInvalidTurn, err)
	}
	s.ReadyToContinue = s.canContinue()
	if end := endedLength(t.Steps, s.Stop); end < len(t.Steps) {
		// Late results after the stop that ended the turn count in Spend,
		// but the reply is fixed there, as TurnMessagesFull's messages are.
		prefix := t
		prefix.Steps = t.Steps[:end]
		ended, err := replayCached(prefix, cache)
		if err != nil {
			return nil, err
		}
		s.Reply = ended.Reply
	}
	return s, nil
}

func (s *replayState) calculateSpend() error {
	s.Spend = Spend{
		Cost:     map[string]money.Amount{},
		ToolCost: map[string]money.Amount{},
	}
	s.ModelCallCosts = nil
	for _, id := range s.modelOrder {
		modelCall, incomplete := modelCallCostOf(
			s.startedModelCalls[id],
			s.modelFinishes[id],
		)
		s.ModelCallCosts = append(s.ModelCallCosts, modelCall)
		if modelCall.UsageSource == UsageSourceNotSent {
			continue
		}
		s.Spend.UsageIncomplete = s.Spend.UsageIncomplete || incomplete
		if usage := modelCall.Usage; usage != nil {
			if err := usage.Validate(); err != nil {
				return err
			}
			s.Spend.addUsage(usage)
		}
		if modelCall.UsageSource == UsageSourceEstimated {
			s.Spend.Estimated = cappedCount(s.Spend.Estimated, 1)
		}
		charge := modelCall.Cost
		if charge == nil {
			s.Spend.CostUnknown = cappedCount(s.Spend.CostUnknown, 1)
			continue
		}
		s.Spend.Cost = addSpendCost(
			&s.Spend,
			s.Spend.Cost,
			charge.Currency,
			charge.Total,
		)
		capped := s.Budget != nil && s.Budget.MaxCost != nil
		if charge.Partial() {
			s.Spend.CostPartial = cappedCount(s.Spend.CostPartial, 1)
			if s.Spend.UnpricedRequests == nil {
				s.Spend.UnpricedRequests = map[string]int{}
			}
			for key, count := range charge.UnpricedRequests {
				s.Spend.UnpricedRequests[key] = cappedCount(
					s.Spend.UnpricedRequests[key],
					count,
				)
			}
		}
		// A partial total is a lower bound, so it cannot show that a cap
		// holds. The budget check before each call refuses provider tools
		// under a cap, so a partial cost there is provider tool work
		// nothing foresaw.
		if capped && (charge.Currency != s.Budget.MaxCost.Currency ||
			charge.Partial()) {
			s.Spend.CostUnknown = cappedCount(s.Spend.CostUnknown, 1)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.Tools)) {
		view := s.Tools[id]
		if view.Status == ToolFinished && view.Outcome != nil &&
			view.Outcome.Cost != nil {
			cost := view.Outcome.Cost
			s.Spend.ToolCost = addSpendCost(
				&s.Spend,
				s.Spend.ToolCost,
				cost.Currency,
				cost.Amount,
			)
		}
	}
	return nil
}

// viewSnapshot captures only the immutable facts needed to build the
// turn's messages.
// It does not calculate accounting or recursively validate a prefix.
func (s *replayState) viewSnapshot() *replayState {
	return &replayState{TurnView: TurnView{Stop: s.Stop, Tools: maps.Clone(s.Tools)}, modelCallStart: s.modelCallStart, unresolvedModel: s.unresolvedModel, callOrder: slices.Clone(s.callOrder), providerTool: maps.Clone(s.providerTool)}
}
