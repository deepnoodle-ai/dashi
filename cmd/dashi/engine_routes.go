package main

import (
	"context"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/llm"
)

// routedCompactor preserves the compactor's own controls and records its route
// before its first model attempt. Ordinary next-turn controls do not replace it.
type routedCompactor struct {
	*dashiCompactor
	agent dashi.Agent
}
type dashiCompactor = compaction.Compactor

func (c *routedCompactor) Start(history []llm.Message) (dashi.Start, error) {
	start, err := c.dashiCompactor.Start(history)
	if err != nil {
		return start, err
	}
	start.Agent.ID = "dashi-cli-summary"
	start.Agent.Revision, start.Agent.Provider, start.Agent.API = c.agent.Revision, c.agent.Provider, c.agent.API
	if start.Agent.Model == "" {
		start.Agent.Model = c.agent.Model
	}
	return start, nil
}

func (e *Engine) bindCommand(ctx context.Context, command dashi.Command) error {
	p := e.cfg.Preferences
	if p == nil {
		return nil
	}
	if start, ok := command.(dashi.Start); ok {
		last := e.Last(ctx)
		if last != nil && last.Open() && last.View.Origin != dashi.StartCompaction &&
			(last.View.Agent.Provider != start.Agent.Provider || last.View.Agent.API != start.Agent.API) {
			// Session closes a stopped turn before admitting Start, and checks
			// its old destination during that close. Complete that close first.
			if last.Next == dashi.NextRunning || last.Next == dashi.NextAnswer {
				return &dashi.TurnBusyError{TurnID: last.View.ID, Next: last.Next}
			}
			if err := p.bind(ctx, last.View.Agent); err != nil {
				return err
			}
			var closing dashi.Command = dashi.Cancel{CommandID: "close:" + last.View.ID, Reason: dashi.CloseReason}
			if last.View.Output != nil {
				cont := dashi.Continue{CommandID: "close:" + last.View.ID}
				if limit := last.View.Stop.Limit; limit != nil && limit.Kind == dashi.LimitModelAttempts {
					cont.ModelAttempts = int(limit.Limit) + 1
				}
				closing = cont
			}
			// A close authorizes only this turn, never queued next-turn work.
			_, err := e.streamBounded(ctx, e.handle().Stream(ctx, closing), false, true)
			if err != nil {
				return err
			}
		}
		return p.bind(ctx, start.Agent)
	}
	last := e.Last(ctx)
	if last == nil {
		return nil
	}
	return p.bind(ctx, last.View.Agent)
}

func (e *Engine) routeCompactor(agent dashi.Agent) *routedCompactor {
	p := e.cfg.Preferences
	d := p.routes.registry[agent.Provider]
	info, _ := catalog.Lookup(d.Definition.API, d.Definition.Endpoint, agent.Model)
	return &routedCompactor{dashiCompactor: compaction.New(compaction.Policy{Model: info}), agent: agent}
}

func (e *Engine) saveNextPreferences() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess == nil || e.cfg.Preferences == nil {
		return nil
	}
	e.sess.NextTurn = e.cfg.Preferences.metadata()
	if !e.cfg.Store.Saved(e.sess) {
		return nil
	}
	return e.cfg.Store.Save(e.sess)
}
