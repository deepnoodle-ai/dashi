package skill

import (
	"context"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

const catalogName = "skills"

// Reminder returns the catalog as contextual runtime context. Text from
// project files has no operator authority, so the catalog is not a good fit
// for Agent.Instructions. The zero Reminder is returned for an empty bundle.
func (b *Bundle) Reminder() llm.Reminder {
	if b == nil || b.catalog == "" {
		return llm.Reminder{}
	}
	return llm.Reminder{
		Name:    catalogName,
		Tier:    llm.ReminderContextual,
		Content: b.catalog,
	}
}

// Hooks returns the hooks that give the catalog to the model. Join them with
// the host's other hooks. Before each model call, the hook delivers the
// catalog as a runtime snapshot unless the turn already holds it. The item ID
// carries the digest, so a changed bundle supersedes the old catalog and
// the record shows which catalog the model saw. An empty bundle returns no hooks.
func (b *Bundle) Hooks() dashi.Hooks {
	if b == nil || b.catalog == "" {
		return dashi.Hooks{}
	}
	item := dashi.ContextItem{
		ID:       "skills-" + b.digest,
		Name:     catalogName,
		Origin:   dashi.ContextRuntime,
		Snapshot: true,
		Message:  llm.ReminderMessage(b.Reminder()),
	}
	return dashi.Hooks{
		BeforeModel: []dashi.ModelHook{{
			Name: "skills-catalog",
			Func: func(
				_ context.Context,
				plan dashi.ModelPlan,
			) (dashi.ModelDecision, error) {
				current, ok := plan.Turn.Context(
					dashi.ContextRuntime,
					catalogName,
				)
				if ok && current.ID == item.ID {
					return dashi.ModelDecision{}, nil
				}
				return dashi.ModelDecision{
					Deliver: []dashi.ContextItem{item},
				}, nil
			},
		}},
	}
}
