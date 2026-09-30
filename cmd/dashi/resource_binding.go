package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/skill"
	"github.com/deepnoodle-ai/dashi/tool"
)

const resourcePinName = "cli-resource-pin"

var resourceToolNames = []string{"load_skill", "read_skill_file"}

type resourceBinding struct {
	Snapshot  resourceSnapshot
	Bundle    *skill.Bundle
	Reference session.ResourceReference
	Legacy    bool
}

// resourceBindingManager belongs to one CLI process. Its approvals belong to
// the workspace policy; only immutable content is reconstructed on recovery.
type resourceBindingManager struct {
	workspace, home string
	store           *session.Store
	mu              sync.Mutex
	sess            *session.Session
	loader          dashi.Loader
	staged          map[string]*discoveredResources
	bound           map[string]*resourceBinding
	warning         func(string, ...any)
	files           map[dashi.CallRef]*validatedSkillFile
}

func newResourceBindingManager(dir, home string, store *session.Store) *resourceBindingManager {
	return &resourceBindingManager{workspace: dir, home: home, store: store, staged: map[string]*discoveredResources{}, bound: map[string]*resourceBinding{}, files: map[dashi.CallRef]*validatedSkillFile{}}
}
func (m *resourceBindingManager) open(sess *session.Session, loader dashi.Loader) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sess, m.loader = sess, loader
	m.staged = map[string]*discoveredResources{}
	m.bound = map[string]*resourceBinding{}
	m.files = map[dashi.CallRef]*validatedSkillFile{}
}
func (m *resourceBindingManager) discover() (*discoveredResources, error) {
	found, err := discoverResources(m.workspace, m.home)
	if err != nil {
		return nil, err
	}
	if m.warning != nil {
		for _, warning := range found.Warnings {
			m.warning("Resource %s: %s", warning.Path, warning.Message)
		}
	}
	return found, nil
}
func (m *resourceBindingManager) stage(command string, found *discoveredResources) {
	m.mu.Lock()
	m.staged = map[string]*discoveredResources{command: found}
	m.mu.Unlock()
}
func (m *resourceBindingManager) environment() (*session.Session, dashi.Loader) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sess, m.loader
}
func (m *resourceBindingManager) read(ref session.ResourceReference, sess *session.Session) (*resourceBinding, error) {
	raw, err := m.store.ReadResources(sess, ref)
	if err != nil {
		return nil, err
	}
	snapshot, bundle, err := decodeResourceSnapshot(raw)
	if err != nil {
		return nil, err
	}
	if snapshot.CatalogDigest != ref.CatalogDigest {
		return nil, errors.New("resource reference catalog digest differs")
	}
	return &resourceBinding{Snapshot: snapshot, Bundle: bundle, Reference: ref}, nil
}

// bind reads only this turn's facts. A pin inherited in prior history never
// identifies the current turn, and an inspection never changes its selection.
func (m *resourceBindingManager) bind(ctx context.Context, id string, create bool) (binding *resourceBinding, pinned bool, err error) {
	m.mu.Lock()
	sess, loader, previous := m.sess, m.loader, m.bound[id]
	m.mu.Unlock()
	if loader == nil || sess == nil {
		return nil, false, errors.New("resource recovery store is unavailable")
	}
	turn, err := loader.Load(ctx, id)
	if err != nil {
		return nil, false, err
	}
	view, err := dashi.Replay(turn)
	if err != nil {
		return nil, false, err
	}
	if view.Origin == dashi.StartCompaction {
		return &resourceBinding{Legacy: true}, false, nil
	}
	ref, found, err := turnResourceReference(turn)
	if err != nil {
		return nil, false, err
	}
	if found {
		binding, err = m.read(ref, sess)
		if err != nil {
			return nil, true, err
		}
	} else {
		active, started := turnResourceActivity(turn)
		if !active {
			binding = &resourceBinding{Legacy: true}
		} else if started {
			return nil, false, errors.New("resource pin is missing from a started turn")
		} else if previous != nil {
			binding, err = m.read(previous.Reference, sess)
			if err != nil {
				return nil, false, err
			}
		} else {
			if !create {
				return nil, false, errors.New("resource pin has not been recorded")
			}
			command := ""
			if len(turn.Steps) > 0 {
				command = turn.Steps[0].CommandID
			}
			m.mu.Lock()
			selection := m.staged[command]
			if selection == nil {
				// Exclusive recovery from damaged history lets Session assign the
				// command key. Its admission selection still belongs to this Start.
				selection = m.staged[""]
			}
			m.mu.Unlock()
			if selection == nil {
				selection, err = m.discover()
				if err != nil {
					return nil, false, err
				}
			}
			raw, err := selection.Snapshot.encode()
			if err != nil {
				return nil, false, err
			}
			ref, err := m.store.SaveResources(sess, raw, selection.Snapshot.CatalogDigest)
			if err != nil {
				return nil, false, err
			}
			binding, err = m.read(ref, sess)
			if err != nil {
				return nil, false, err
			}
			m.mu.Lock()
			delete(m.staged, command)
			delete(m.staged, "")
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	for other := range m.bound {
		if other != id {
			delete(m.bound, other)
		}
	}
	for ref := range m.files {
		if ref.TurnID != id {
			delete(m.files, ref)
		}
	}
	m.bound[id] = binding
	m.mu.Unlock()
	return binding, found, nil
}
func turnResourceReference(turn dashi.Turn) (session.ResourceReference, bool, error) {
	var selected session.ResourceReference
	found := false
	for _, step := range turn.Steps {
		for _, item := range step.Items() {
			if item.Name != resourcePinName {
				continue
			}
			if found || item.ID != resourcePinName+":"+turn.ID || item.Origin != dashi.ContextRuntime || !item.Snapshot || item.NextCallOnly || item.Message.Role != llm.RoleUser || len(item.Message.Content) != 1 {
				return selected, false, errors.New("invalid or duplicate turn resource pin")
			}
			content := item.Message.Content[0]
			if content.Kind != llm.ContentReminder || content.Reminder == nil || content.Reminder.Name != resourcePinName || content.Reminder.Tier != llm.ReminderContextual || content.Reminder.Validate() != nil {
				return selected, false, errors.New("invalid contextual resource pin")
			}
			decoder := json.NewDecoder(bytes.NewBufferString(content.Reminder.Content))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&selected); err != nil {
				return selected, false, err
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return selected, false, errors.New("trailing resource reference JSON")
			}
			found = true
		}
	}
	return selected, found, nil
}
func (m *resourceBindingManager) beforeModel(ctx context.Context, plan dashi.ModelPlan) (dashi.ModelDecision, error) {
	if plan.Turn.Origin == dashi.StartCompaction {
		return dashi.ModelDecision{}, nil
	}
	binding, pinned, err := m.bind(ctx, plan.Turn.ID, true)
	if err != nil {
		return dashi.ModelDecision{Stop: &dashi.Stop{Kind: dashi.StopFailed, Reason: "Resources unavailable: " + err.Error()}}, nil
	}
	if binding.Legacy {
		var hidden []string
		for _, definition := range plan.Input.Tools {
			if definition.Name == "load_skill" || definition.Name == "read_skill_file" {
				hidden = append(hidden, definition.Name)
			}
		}
		return dashi.ModelDecision{Hide: hidden}, nil
	}
	var delivery []dashi.ContextItem
	if !pinned {
		raw, _ := json.Marshal(binding.Reference)
		delivery = append(delivery, dashi.ContextItem{ID: resourcePinName + ":" + plan.Turn.ID, Name: resourcePinName, Origin: dashi.ContextRuntime, Snapshot: true, Message: llm.ReminderMessage(llm.Reminder{Name: resourcePinName, Tier: llm.ReminderContextual, Content: string(raw)})})
	}
	hooks := []dashi.ModelHook{dashi.SeedContext(binding.Snapshot.InstructionSource), dashi.SeedContext(binding.Snapshot.Instructions)}
	catalog := binding.Bundle.Hooks().BeforeModel
	if len(catalog) == 0 {
		catalog = []dashi.ModelHook{dashi.SeedContext(dashi.ContextItem{ID: "skills-empty-" + binding.Reference.CatalogDigest, Name: "skills", Origin: dashi.ContextRuntime, Snapshot: true, Message: llm.ReminderMessage(llm.Reminder{Name: "skills", Tier: llm.ReminderContextual, Content: "No model skills are available."})})}
	}
	hooks = append(hooks, catalog...)
	for _, hook := range hooks {
		decision, err := hook.Func(ctx, plan)
		if err != nil {
			return dashi.ModelDecision{Stop: &dashi.Stop{Kind: dashi.StopFailed, Reason: "Resource delivery failed: " + err.Error()}}, nil
		}
		delivery = append(delivery, decision.Deliver...)
	}
	return dashi.ModelDecision{Deliver: delivery}, nil
}
func (m *resourceBindingManager) beforeTool(ctx context.Context, plan dashi.ToolPlan) (dashi.ToolDecision, error) {
	binding, _, err := m.bind(ctx, plan.Turn.ID, false)
	if err != nil {
		return dashi.Deny("Resources unavailable: " + err.Error()), nil
	}
	if binding.Legacy {
		if plan.Call.Name == "load_skill" || plan.Call.Name == "read_skill_file" {
			return dashi.Deny("Resources were not selected for this turn."), nil
		}
		return dashi.ToolDecision{}, nil
	}
	if plan.Call.Name == "read_skill_file" {
		return m.validateSkillFile(ctx, plan, binding)
	}
	return dashi.ToolDecision{}, nil
}
func (m *resourceBindingManager) hooks() dashi.Hooks {
	return dashi.Hooks{BeforeModel: []dashi.ModelHook{{Name: "cli-resources", Func: m.beforeModel}}, BeforeTool: []dashi.ToolHook{{Name: "cli-resources", Func: m.beforeTool}}}
}

type skillLoadArgs struct {
	Name string `json:"name" description:"Name of an available skill to load"`
}

func (m *resourceBindingManager) tools() (tool.Toolset, error) {
	loader, err := tool.Func("load_skill", "Load the full instructions for an available skill by name.", func(ctx context.Context, call tool.Call, _ skillLoadArgs) (tool.Outcome, error) {
		ref, ok := dashi.CurrentCall(ctx)
		if !ok {
			return tool.Outcome{}, tool.Fatal(errors.New("skill loader requires a recorded turn"))
		}
		binding, _, err := m.bind(ctx, ref.TurnID, false)
		if err != nil {
			return tool.Outcome{}, tool.Fatal(fmt.Errorf("resource recovery: %w", err))
		}
		if binding.Legacy {
			return tool.Failed("No skills were selected for this turn."), nil
		}
		tools, err := binding.Bundle.Tools(ctx)
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

func (m *resourceBindingManager) preflight(ctx context.Context, sess *session.Session, loader dashi.Loader, id string) (*resourceBinding, error) {
	turn, err := loader.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	view, err := dashi.Replay(turn)
	if err != nil {
		return nil, err
	}
	if view.Origin == dashi.StartCompaction {
		return nil, nil
	}
	ref, found, err := turnResourceReference(turn)
	if err != nil {
		return nil, err
	}
	if found {
		return m.read(ref, sess)
	}
	active, started := turnResourceActivity(turn)
	if active && started {
		return nil, errors.New("resource pin is missing from a started turn")
	}
	return nil, nil
}

func turnResourceActivity(turn dashi.Turn) (active, started bool) {
	for _, step := range turn.Steps {
		// Continue records today's hooks even for a legacy turn. Only its
		// original Start establishes resource selection before any intent.
		if step.Kind == dashi.StepStarted && step.ActiveHooks != nil {
			for _, name := range step.ActiveHooks.BeforeModel {
				if name == "cli-resources" {
					active = true
				}
			}
		}
		if step.Model != nil || step.Tool != nil {
			started = true
			if step.Model != nil {
				for _, definition := range step.Model.Tools {
					if definition.Name == "load_skill" || definition.Name == "read_skill_file" {
						active = true
					}
				}
			}
		}
	}
	return active, started
}
