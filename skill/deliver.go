package skill

import (
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

// Hooks returns hooks that add the catalog to the turn as a runtime snapshot
// unless its latest visible value is unchanged. Session turns share that
// presence rule with SeedContext. A changed catalog appends, and compaction
// that drops it causes the catalog to be added again. The item ID carries
// the catalog hash for audit.
// An empty bundle returns no hooks.
func (b *Bundle) Hooks() dashi.Hooks {
	if b == nil || b.catalog == "" {
		return dashi.Hooks{}
	}
	item := dashi.ContextItem{
		ID:       "skills-" + b.hash,
		Name:     catalogName,
		Origin:   dashi.ContextRuntime,
		Snapshot: true,
		Message:  llm.ReminderMessage(b.Reminder()),
	}
	hook := dashi.SeedContext(item)
	hook.Name = "skills-catalog"
	return hook.Hooks()
}
