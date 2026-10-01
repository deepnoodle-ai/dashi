package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// resourceManager connects discovery to the existing skill hooks and loader.
// Loaded skill text is an ordinary tool result; session replay owns its history.
type resourceManager struct {
	workspace, home string
	mu              sync.Mutex
	warning         func(string, ...any)
	files           map[dashi.CallRef]*validatedSkillFile
	// Display only. The workspace policy owns and enforces these answers.
	remembered map[skillFileArgs]bool
	// shown holds the keys of current instruction problems already shown.
	// A key that disappears is dropped, so a problem that returns shows again.
	shown map[string]bool
}

func newResourceManager(dir, home string) *resourceManager {
	return &resourceManager{workspace: dir, home: home, files: map[dashi.CallRef]*validatedSkillFile{}, remembered: map[skillFileArgs]bool{}, shown: map[string]bool{}}
}

// discover refreshes resources before each use. Skill problems never reach
// the transcript; /resources lists them. A workspace instruction file that
// exists but cannot be used is shown once, because the model then works
// without instructions the user relies on.
func (m *resourceManager) discover() (*discoveredResources, error) {
	found, err := discoverResources(m.workspace, m.home)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	current := map[string]bool{}
	var fresh []resourceWarning
	for _, warning := range found.Warnings {
		if !warning.Instructions {
			continue
		}
		key := warning.key()
		current[key] = true
		if !m.shown[key] {
			fresh = append(fresh, warning)
		}
	}
	m.shown = current
	m.mu.Unlock()
	if m.warning != nil {
		for _, warning := range fresh {
			// The line already says the instructions were not loaded.
			reason := strings.TrimPrefix(warning.Message, "instructions omitted: ")
			reason = strings.TrimPrefix(reason, "cannot inspect workspace instructions: ")
			m.warning("Workspace instructions not loaded: %s: %s. /resources shows details.", warning.Path, reason)
		}
	}
	return found, nil
}
func (m *resourceManager) rememberApproval(request ApprovalRequest, answer Approval) {
	if request.Tool != "read_skill_file" || !answer.Allow || !answer.Remember {
		return
	}
	var args skillFileArgs
	if json.Unmarshal(request.Args, &args) != nil {
		return
	}
	m.mu.Lock()
	m.remembered[args] = true
	m.mu.Unlock()
}
func (m *resourceManager) rememberedApprovals() []skillFileArgs {
	m.mu.Lock()
	var out []skillFileArgs
	for args := range m.remembered {
		out = append(out, args)
	}
	m.mu.Unlock()
	slices.SortFunc(out, func(a, b skillFileArgs) int {
		if order := strings.Compare(a.Skill, b.Skill); order != 0 {
			return order
		}
		return strings.Compare(a.Path, b.Path)
	})
	return out
}

func (m *resourceManager) beforeModel(ctx context.Context, plan dashi.ModelPlan) (dashi.ModelDecision, error) {
	if plan.Turn.Origin == dashi.StartCompaction || plan.Turn.Parent != nil {
		return dashi.ModelDecision{}, nil
	}
	found, err := m.discover()
	if err != nil {
		return dashi.ModelDecision{Stop: &dashi.Stop{Kind: dashi.StopFailed, Reason: "Resources unavailable: " + err.Error()}}, nil
	}
	hooks := []dashi.ModelHook{dashi.SeedContext(found.InstructionSource), dashi.SeedContext(found.Instructions)}
	catalog := found.Bundle.Hooks().BeforeModel
	if len(catalog) == 0 {
		catalog = []dashi.ModelHook{dashi.SeedContext(dashi.ContextItem{Name: "skills", Origin: dashi.ContextRuntime, Snapshot: true, Message: llm.ReminderMessage(llm.Reminder{Name: "skills", Tier: llm.ReminderContextual, Content: "No model skills are available."})})}
	}
	hooks = append(hooks, catalog...)
	var delivery []dashi.ContextItem
	for _, hook := range hooks {
		decision, err := hook.Func(ctx, plan)
		if err != nil {
			return dashi.ModelDecision{Stop: &dashi.Stop{Kind: dashi.StopFailed, Reason: "Adding resource messages failed: " + err.Error()}}, nil
		}
		delivery = append(delivery, decision.AddMessages...)
	}
	return dashi.ModelDecision{AddMessages: delivery}, nil
}
func (m *resourceManager) beforeTool(ctx context.Context, plan dashi.ToolPlan) (dashi.ToolDecision, error) {
	if plan.Call.Name != "read_skill_file" {
		return dashi.ToolDecision{}, nil
	}
	found, err := m.discover()
	if err != nil {
		return dashi.Deny("Resources unavailable: " + err.Error()), nil
	}
	return m.validateSkillFile(ctx, plan, found)
}
func (m *resourceManager) hooks() dashi.Hooks {
	return dashi.Hooks{
		BeforeModel: []dashi.ModelHook{{Name: "cli-resources", Func: m.beforeModel}},
		BeforeTool:  []dashi.ToolHook{{Name: "cli-resources", Func: m.beforeTool}},
		AfterTool: []dashi.ResultHook{{Name: "cli-resources", Func: func(_ context.Context, plan dashi.ResultPlan) (dashi.ResultDecision, error) {
			m.mu.Lock()
			delete(m.files, dashi.CallRef{TurnID: plan.Turn.ID, CallID: plan.Call.ID})
			m.mu.Unlock()
			return dashi.ResultDecision{}, nil
		}}},
	}
}

type skillLoadArgs struct {
	Name string `json:"name" description:"Name of an available skill to load"`
}

func (m *resourceManager) tools() (tool.Toolset, error) {
	loader, err := tool.Func("load_skill", "Load the full instructions for an available skill by name.", func(ctx context.Context, call tool.Call, _ skillLoadArgs) (tool.Outcome, error) {
		found, err := m.discover()
		if err != nil {
			return tool.Failed(fmt.Sprintf("Skills unavailable: %v", err)), nil
		}
		tools, err := found.Bundle.Tools(ctx)
		if err != nil {
			return tool.Outcome{}, err
		}
		if len(tools) == 0 {
			return tool.Failed("No model skills are available."), nil
		}
		return tools[0].Executor.Execute(ctx, call)
	}, tool.ReadOnly(), tool.Idempotent())
	if err != nil {
		return nil, err
	}
	loader.Definition.Annotations.Destructive = new(false)
	loader.Definition.Annotations.OpenWorld = new(false)
	files, err := m.skillFileTool()
	if err != nil {
		return nil, err
	}
	return tool.List{loader, files}, nil
}
