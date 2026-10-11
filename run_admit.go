package dashi

import (
	"context"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Admit validates and records Start without preparing or sending a model
// request, running tools or hooks, fetching steers, or loading content. The
// turn stops as StopReady; Continue runs it and Cancel withdraws it. WithOwner
// fences its commits as for Run. Pause and steering options do not run work.
//
// Without a Client, Start must name its provider, API, and model. Enabled
// active compaction also needs an explicit Window. Exact Start retries load
// their original evidence before consulting today's client or defaults.
// Errors have Run's meaning, including uncertain writes: reload after a
// CommitError. If only Start committed, NextRecover describes that prefix.
func (r Runner) Admit(
	ctx context.Context,
	turn Turn,
	start Start,
	opts ...RunOption,
) (RunResult, error) {
	o := newRunOptions(opts)
	o.admit = true
	return r.run(ctx, turn, start, o)
}

// retryStart compares against the settings accepted at the original Start,
// so credential removal, catalog changes, and new defaults cannot obstruct
// an acknowledgement or alter already accepted work.
func (r Runner) retryStart(
	ctx context.Context,
	turn Turn,
	start Start,
) (RunResult, bool, error) {
	if _, err := replay(turn); err != nil {
		return RunResult{}, false, err
	}
	saved := turn
	if loader, ok := r.Recorder.(Loader); ok {
		loaded, err := loader.Load(ctx, turn.ID)
		if errors.Is(err, ErrTurnNotFound) && len(turn.Steps) == 0 {
			return RunResult{}, false, nil
		}
		if err != nil {
			return RunResult{}, false, err
		}
		saved = loaded
	}
	state, err := replay(saved)
	if err != nil {
		return RunResult{}, false, err
	}
	old, found := state.commandHashes[start.CommandID]
	if !found {
		return RunResult{}, false, nil
	}
	agent, err := retryAgent(start.Agent, state.Agent)
	if err != nil {
		return RunResult{}, false, err
	}
	if err := agentChanged(agent, state.Agent); err != nil {
		return RunResult{}, false, err
	}
	start.Agent = agent
	hash, err := commandHash(start)
	if err != nil {
		return RunResult{}, false, err
	}
	if old != hash {
		return RunResult{}, false, ErrCommandIDReused
	}
	return outcome(saved, state), true, nil
}

func retryAgent(agent, recorded Agent) (Agent, error) {
	if err := agent.Validate(); err != nil {
		return Agent{}, err
	}
	if agent.Provider == "" {
		agent.Provider = recorded.Provider
	}
	if agent.API == "" {
		agent.API = recorded.API
	}
	if agent.Model == "" {
		agent.Model = recorded.Model
	}
	if agent.Version == "" {
		agent.Version = recorded.Version
	}
	l, old := &agent.Limits, recorded.Limits
	if l.ModelCalls == 0 {
		l.ModelCalls = old.ModelCalls
	}
	if l.Resumes == 0 {
		l.Resumes = old.Resumes
	}
	if l.ProviderPauses == 0 {
		l.ProviderPauses = old.ProviderPauses
	}
	if l.ToolConcurrency == 0 {
		l.ToolConcurrency = old.ToolConcurrency
	}
	if l.ToolResultBytes == 0 {
		l.ToolResultBytes = old.ToolResultBytes
	}
	if agent.ActiveCompaction != nil && recorded.ActiveCompaction != nil {
		p := *agent.ActiveCompaction
		if p.Fraction == 0 {
			p.Fraction = recorded.ActiveCompaction.Fraction
		}
		if p.MaxTokens == 0 {
			p.MaxTokens = recorded.ActiveCompaction.MaxTokens
		}
		agent.ActiveCompaction = &p
	}
	return agent.resolved(), nil
}

func validateStartInput(origin StartOrigin, input []llm.Message) error {
	if origin != StartUser && origin != StartScheduled &&
		origin != StartOperator && origin != StartCompaction {
		return errors.New("invalid start origin")
	}
	for i, message := range input {
		if origin != StartOperator && message.Role != llm.RoleUser {
			return errors.New("user or scheduled start input must use user role")
		}
		if origin == StartOperator && message.Role != llm.RoleUser &&
			message.Role != llm.RoleSystem && message.Role != llm.RoleDeveloper {
			return errors.New("invalid operator start role")
		}
		if err := validateMessage(message); err != nil {
			return fmt.Errorf("start input %d: %w", i+1, err)
		}
		if reminder := messageReminder(message); reminder != nil &&
			reminder.Tier == llm.ReminderOperator && origin != StartOperator {
			return errors.New("operator reminder requires operator start origin")
		}
	}
	return nil
}
