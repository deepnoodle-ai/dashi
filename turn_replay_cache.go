package dashi

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
)

// replayCache rebuilds the state of one turn that only grows, such as a
// run's turn or a recorder's, by applying only the steps added since the
// last rebuild. A full replay checks every step each time, so a turn of S
// steps rebuilt after each step costs O(S²); the cache makes each rebuild
// cost the new steps plus one copy of the state.
//
// The cache trusts that steps it has applied never change: it reuses its
// state only while the turn's steps share the backing array of the steps
// it applied, and replays in full otherwise. Callers append steps; they
// never rewrite one in place.
type replayCache struct {
	mu sync.Mutex
	// steps are the steps base has applied.
	steps []Step
	// base is the state after steps, before finish. It is never handed
	// out; each rebuild finishes a copy.
	base *replayState
	// handed and handedWant are, when checkReplayCache is on, the last
	// state handed out and a full replay of its turn. The next rebuild
	// checks that applying more steps to base left handed unchanged.
	handed, handedWant *replayState
}

// checkReplayCache makes every cached rebuild also replay in full and
// panic when the two differ. Tests turn it on.
var checkReplayCache = false

// replay returns the state of t, as replay(t) does.
func (c *replayCache) replay(t Turn) (*replayState, error) {
	return c.replayWith(t, nil)
}

// replayWith returns the state of t with extra appended, as
// replay(t + extra) does, without keeping extra in the cache: extra is a
// step not yet committed.
func (c *replayCache) replayWith(t Turn, extra *Step) (*replayState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	full := t
	if extra != nil {
		full.Steps = append(slices.Clip(t.Steps), *extra)
	}
	out, err := c.rebuild(t, full)
	if checkReplayCache {
		if c.handed != nil {
			sameReplayState(c.handed, c.handedWant, "a later rebuild changed a state handed out earlier")
		}
		c.handed, c.handedWant = nil, nil
		if want := checkCachedReplay(full, out, err); want != nil {
			c.handed, c.handedWant = out.clone(), want
		}
	}
	return out, err
}

// rebuild brings base up to t, then finishes a copy of it with full's
// further steps applied.
func (c *replayCache) rebuild(t, full Turn) (*replayState, error) {
	if !c.extends(t) {
		c.base, c.steps = nil, nil
		base, err := replaySteps(t, &contextValidationCache{prefixes: map[int64][]contextMessage{}})
		if err != nil {
			return nil, err
		}
		c.base, c.steps = base, t.Steps
	} else if len(t.Steps) > len(c.steps) {
		c.base.source = t
		if err := c.base.applySteps(t.Steps[len(c.steps):]); err != nil {
			c.base, c.steps = nil, nil
			return nil, err
		}
		c.steps = t.Steps
	}
	s := c.base.clone()
	s.source = full
	if err := s.applySteps(full.Steps[len(t.Steps):]); err != nil {
		return nil, err
	}
	out, err := s.finish(full)
	if err != nil {
		// finish may reject a step that base already holds, and a caller
		// may then write a different step into the same slot of the backing
		// array. Drop the cache so that step is checked again.
		c.base, c.steps = nil, nil
		return nil, err
	}
	return out, nil
}

// extends reports whether t is base's turn with zero or more steps
// appended. Sharing the backing array does not show that no step was
// rewritten in place; comparing the last applied step's Seq catches the
// rewrites a caller is most likely to make.
func (c *replayCache) extends(t Turn) bool {
	n := len(c.steps)
	return c.base != nil &&
		t.ID == c.base.ID &&
		len(t.Steps) >= n &&
		n > 0 &&
		&t.Steps[0] == &c.steps[0] &&
		t.Steps[n-1].Seq == c.base.LastSeq
}

// clone copies s so that applying more steps to either copy, or finishing
// either, leaves the other unchanged. Steps' own data is shared, as a full
// replay shares it with the turn. TestReplayStateCloneCoversFields fails
// when a field is added here without a decision.
func (s *replayState) clone() *replayState {
	out := *s
	out.TurnView = s.TurnView.cloneContainers()
	out.startedModelCalls = maps.Clone(s.startedModelCalls)
	out.modelOrder = slices.Clip(s.modelOrder)
	out.modelFinishes = maps.Clone(s.modelFinishes)
	out.modelMessages = maps.Clone(s.modelMessages)
	out.modelCallOfTool = maps.Clone(s.modelCallOfTool)
	out.modelDefs = maps.Clone(s.modelDefs)
	out.callOrder = slices.Clip(s.callOrder)
	out.providerTool = maps.Clone(s.providerTool)
	out.providerToolOrder = slices.Clip(s.providerToolOrder)
	out.providerToolModelCalls = maps.Clone(s.providerToolModelCalls)
	out.commandHashes = maps.Clone(s.commandHashes)
	out.contextIDs = maps.Clone(s.contextIDs)
	out.liveSeq = maps.Clone(s.liveSeq)
	out.callStartSeq = maps.Clone(s.callStartSeq)
	return &out
}

// cloneContainers copies the maps and slices of v that replay changes in
// place, so a later step cannot change the copy. Unlike clone, it shares
// the values in them.
func (v TurnView) cloneContainers() TurnView {
	v.ContextCompactions = slices.Clip(v.ContextCompactions)
	v.Input = slices.Clip(v.Input)
	v.Raises = slices.Clip(v.Raises)
	v.Spend = v.Spend.Clone()
	v.ModelCallCosts = slices.Clip(v.ModelCallCosts)
	v.Pending = slices.Clip(v.Pending)
	v.Warnings = slices.Clip(v.Warnings)
	v.Tools = maps.Clone(v.Tools)
	v.live = maps.Clone(v.live)
	v.usedContextIDs = maps.Clone(v.usedContextIDs)
	v.acted = maps.Clone(v.acted)
	return v
}

// checkCachedReplay panics when a cached rebuild of t differs from a full
// replay of it, and returns the full replay.
func checkCachedReplay(t Turn, got *replayState, gotErr error) *replayState {
	want, wantErr := replay(t)
	if (gotErr == nil) != (wantErr == nil) ||
		gotErr != nil && gotErr.Error() != wantErr.Error() {
		panic(fmt.Sprintf("dashi: cached replay error %v, full replay error %v", gotErr, wantErr))
	}
	if gotErr != nil {
		return nil
	}
	sameReplayState(got, want, fmt.Sprintf("cached replay of turn %q at %d steps differs from a full replay", t.ID, len(t.Steps)))
	return want
}

func sameReplayState(got, want *replayState, message string) {
	a, b := *got, *want
	a.contextCache, b.contextCache = nil, nil
	if !reflect.DeepEqual(a, b) {
		panic("dashi: " + message)
	}
}
