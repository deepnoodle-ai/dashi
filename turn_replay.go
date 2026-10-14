package dashi

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type replayState struct {
	contextCache      *contextValidationCache
	source            Turn
	pendingCompaction string
	// failedSummary is the model call ID of the latest summary call that
	// finished with an error, so the run retries it once or applies a
	// summary fallback. It outlasts an interrupted retry, so that retry
	// falls back too, and clears when a summary or fallback is recorded.
	failedSummary string
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
	// compiledSchema is OutputSchema compiled, so replay compiles it once.
	compiledSchema *jsonschema.Schema
}

// outputSchema returns the turn's OutputSchema compiled.
func (s *replayState) outputSchema() (*jsonschema.Schema, error) {
	if s.compiledSchema == nil {
		schema, err := compileOutputSchema(s.OutputSchema)
		if err != nil {
			return nil, err
		}
		s.compiledSchema = schema
	}
	return s.compiledSchema, nil
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

// snapshotView returns a deep copy of v for a caller outside replay.
func snapshotView(v TurnView) TurnView {
	return v.clone()
}

func replay(t Turn) (*replayState, error) {
	return replayCached(t, &contextValidationCache{prefixes: map[int64][]contextMessage{}})
}

func replayCached(t Turn, cache *contextValidationCache) (*replayState, error) {
	s, err := replaySteps(t, cache)
	if err != nil {
		return nil, err
	}
	return s.finish(t)
}

// replaySteps checks and applies every step of t, without the totals and
// pending work that finish derives.
func replaySteps(t Turn, cache *contextValidationCache) (*replayState, error) {
	if cache.states == nil {
		cache.states = map[int64]*replayState{}
	}
	if cache.needed == nil {
		cache.needed = map[int64]bool{}
	}
	// needed is set only here, from the steps known now. A replayCache
	// that later applies a compaction step through applySteps has no
	// snapshot for its Through, so building its history replays that
	// prefix once instead (see coveredMessagesCached). The result is the same.
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
	if err := s.applySteps(t.Steps); err != nil {
		return nil, err
	}
	return s, nil
}

// applySteps checks and applies steps, which follow the LastSeq steps s
// has applied. On an error s is left part-applied and must be dropped.
func (s *replayState) applySteps(steps []Step) error {
	cache := s.contextCache
	for _, step := range steps {
		i := int(s.LastSeq)
		if _, err := json.Marshal(step); err != nil {
			return fmt.Errorf(
				"%w: step %d cannot serialize: %v",
				ErrInvalidTurn,
				i+1,
				err,
			)
		}
		if step.Version != 1 || step.TurnID != s.ID || step.Seq != int64(i+1) {
			return fmt.Errorf("%w: invalid step header at %d", ErrInvalidTurn, i+1)
		}
		if err := validateStepPayload(step); err != nil {
			return fmt.Errorf("%w: step %d: %v", ErrInvalidTurn, i+1, err)
		}
		if step.CommandID != "" {
			if step.CommandHash == "" {
				return fmt.Errorf("%w: command hash missing", ErrInvalidTurn)
			}
			if old, ok := s.commandHashes[step.CommandID]; ok &&
				old != step.CommandHash {
				return ErrCommandIDReused
			}
			s.commandHashes[step.CommandID] = step.CommandHash
		}
		if i == 0 && step.Kind != StepTurnStarted {
			return fmt.Errorf("%w: first step is not start", ErrInvalidTurn)
		}
		if i > 0 && step.Kind == StepTurnStarted {
			return fmt.Errorf("%w: duplicate start", ErrInvalidTurn)
		}
		if err := s.apply(step); err != nil {
			return fmt.Errorf("%w: step %d: %v", ErrInvalidTurn, i+1, err)
		}
		s.Warnings = append(s.Warnings, step.Warnings...)
		if recordsActiveHooks(step) {
			s.activeHooks = step.ActiveHooks
		}
		s.trackInput(step)
		s.trackContext(step)
		s.trackHooks(step)
		if err := s.trackIDs(step); err != nil {
			return fmt.Errorf("%w: step %d: %v", ErrInvalidTurn, i+1, err)
		}
		if !s.awaitingClassification {
			s.deliveredOnKept = 0
		}
		s.LastSeq = step.Seq
		if cache.needed[step.Seq] {
			cache.states[step.Seq] = s.viewSnapshot()
		}
	}
	return nil
}

// finish derives what the applied steps of t mean now: argument hashes,
// pending work, reply, spend, and whether the turn can continue.
func (s *replayState) finish(t Turn) (*replayState, error) {
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
		// Late model results after the stop that ended the turn count in Spend,
		// but the reply is fixed there, as TurnMessagesFull's messages are.
		prefix := t
		prefix.Steps = t.Steps[:end]
		ended, err := replayCached(prefix, s.contextCache)
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
		missing := modelCall.UsageSource == UsageSourceMissing
		if modelCall.Finished &&
			(missing || modelCall.UsageSource == UsageSourceCutOff) {
			s.Spend.CallsWithoutFinalUsage = cappedCount(
				s.Spend.CallsWithoutFinalUsage,
				1,
			)
		}
		charge := modelCall.Cost
		if charge == nil {
			// A call with no reported usage counts zero, not unknown, so
			// an unpriced one does not block a cap (decision 47).
			if !missing {
				s.Spend.CostUnknown = cappedCount(s.Spend.CostUnknown, 1)
			}
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
		if capped && (charge.Currency != s.Budget.MaxCost.Currency &&
			(!missing || charge.Total != 0) || charge.Partial()) {
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
