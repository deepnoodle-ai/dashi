package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

// ChildServices freezes a child's model and reference services. Nil fields
// retain the inherited services. These do not change execution ownership.
type ChildServices struct {
	Client        llm.Client
	ContentLoader dashi.ContentLoader
}

func copyValue[T any](value T) (T, error) {
	var owned T
	b, err := json.Marshal(value)
	if err != nil {
		return owned, err
	}
	err = json.Unmarshal(b, &owned)
	return owned, err
}
func equalValue(a, b any) bool {
	x, e := canonjson.Marshal(a)
	if e != nil {
		return false
	}
	y, e := canonjson.Marshal(b)
	return e == nil && bytes.Equal(x, y)
}

func (ts *Tasks) prepareStart(ctx context.Context, ref dashi.CallRef, start dashi.Start) (dashi.Start, error) {
	if ts.cfg.PrepareStart == nil {
		return start, nil
	}
	owned, err := copyValue(start)
	if err != nil {
		return dashi.Start{}, err
	}
	if start.OutputSchema == nil {
		owned.OutputSchema = nil
	}
	prepared, err := ts.cfg.PrepareStart(ctx, ref, owned)
	if err != nil {
		return dashi.Start{}, fmt.Errorf("delegate: prepare child: %w", err)
	}
	// Compare the protected fields after removing the explicitly allowed
	// settings and version changes. The original input is an unchanged prefix.
	if len(prepared.Input) < len(start.Input) || !equalValue(start.Input, prepared.Input[:len(start.Input)]) {
		return dashi.Start{}, errors.New("delegate: child preparation changed the task prompt")
	}
	checked := prepared
	checked.Input = start.Input
	checked.Agent.Settings = start.Agent.Settings
	checked.Agent.ActiveCompaction = start.Agent.ActiveCompaction
	checked.Agent.Version = start.Agent.Version
	checked.Agent.NoInheritedSettings = start.Agent.NoInheritedSettings
	if !equalValue(checked, start) {
		return dashi.Start{}, errors.New("delegate: child preparation changed protected Start fields")
	}
	absentSchema := prepared.OutputSchema == nil
	prepared, err = copyValue(prepared)
	if absentSchema {
		prepared.OutputSchema = nil
	}
	if err != nil {
		return dashi.Start{}, err
	}
	return prepared, nil
}
func (ts *Tasks) bindServices(ctx context.Context, ref dashi.CallRef, agent dashi.Agent, runner dashi.Runner) (dashi.Runner, error) {
	if ts.cfg.BindServices == nil {
		return runner, nil
	}
	owned, err := copyValue(agent)
	if err != nil {
		return runner, err
	}
	services, err := ts.cfg.BindServices(ctx, ref, owned)
	if err != nil {
		return runner, fmt.Errorf("delegate: bind child services: %w", err)
	}
	if services.Client != nil {
		runner.Client = services.Client
	}
	if services.ContentLoader != nil {
		runner.ContentLoader = services.ContentLoader
	}
	return runner, nil
}
func (ts *Tasks) deferNotice(turn dashi.Turn) bool {
	if ts.cfg.NoticeReady == nil {
		return false
	}
	owned, err := copyValue(turn)
	if err != nil {
		return true
	}
	view, err := owned.View()
	if err != nil {
		return true
	}
	return !ts.cfg.NoticeReady(view)
}
func (ts *Tasks) prepareNoticePolicy(rec *record) {
	rec.deferred = make([]bool, len(rec.views))
	for i, turn := range rec.turns {
		rec.deferred[i] = ts.deferNotice(turn)
	}
}

// AttentionError reports stopped children whose final notices are deferred.
// It is returned by Wait only when no live or reporting work remains. The
// host inspects their records and resolves the exact child commands.
type AttentionError struct{ Tasks []TaskView }

func (e *AttentionError) Error() string {
	return fmt.Sprintf("delegate: %d task(s) need attention before final result delivery", len(e.Tasks))
}

func (ts *Tasks) attentionLocked() ([]TaskView, bool) {
	var attention []TaskView
	busy := false
	for _, id := range ts.order {
		t := ts.tasks[id]
		if t.cancel != nil {
			busy = true
		}
		for _, c := range t.turns {
			// A historical notice stays acknowledged, but its unresolved
			// child still requires attention under the opted-in policy.
			if c.deferred && c.state != TaskRunning && !c.suppressed {
				attention = append(attention, ts.viewLocked(t, c))
			} else if !c.reported {
				busy = true
			}
		}
	}
	return attention, busy
}

func (ts *Tasks) hasPendingCommandLocked() bool {
	for _, t := range ts.tasks {
		if t.pending != nil {
			return true
		}
	}
	return false
}
