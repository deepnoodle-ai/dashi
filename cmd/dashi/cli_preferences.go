package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// cliPreferences keeps new-turn choices separate from the client driving an
// existing turn. The Controller changes it only while no turn is running.
type cliPreferences struct {
	mu                 sync.RWMutex
	launch             launchPreferences
	routes             *routeClients
	client             *turnClient
	writer             userPreferenceWriter
	interactive        bool
	ask                AskFunc
	guidance           string
	guidanceSet        bool
	environmentOrigins map[string]string
	environmentFiles   []string
	resolveSession     func(preferenceFile) (launchPreferences, error)
	// featuredModels is the user file's raw "featured_models" object.
	featuredModels json.RawMessage
}

func (p *cliPreferences) agent() dashi.Agent {
	p.mu.RLock()
	defer p.mu.RUnlock()
	agent, _ := selectionAgent(p.launch.Selection, p.launch.Controls.Settings)
	// Own pointers even when the Start waits behind queued work.
	raw, _ := json.Marshal(agent)
	var owned dashi.Agent
	_ = json.Unmarshal(raw, &owned)
	owned.ActiveCompaction = &dashi.ActiveCompactionPolicy{}
	return owned
}
func (p *cliPreferences) clientFor(ctx context.Context, agent dashi.Agent) (llm.Client, error) {
	d, ok := p.routes.registry[agent.Provider]
	if !ok {
		return nil, fmt.Errorf("recorded destination %q is no longer configured", agent.Provider)
	}
	hash, err := d.hash()
	if err != nil {
		return nil, err
	}
	// Check the recorded binding before presenting or granting new trust.
	if strings.HasPrefix(agent.Version, cliRouteVersion) && agent.Version != cliRouteVersion+hash {
		return nil, errors.New("recorded destination binding changed; restore its configuration before continuing")
	}
	if d.Project && !d.trustedBy(p.routes.user) && !p.routes.granted(hash) {
		if !p.interactive || p.ask == nil {
			return nil, fmt.Errorf("destination %q needs project binding trust; configure its reviewed binding in user settings or use an interactive run", d.ID)
		}
		prompt := fmt.Sprintf("Allow this destination binding for this invocation?\nSource: %s\nDestination: %s\nAPI: %s\nEndpoint: %s\nModels: %s\nDefault: %s\nCredential variable: %s", d.Source, d.ID, d.Definition.API, d.Definition.Endpoint, strings.Join(d.Definition.Models, ", "), d.Definition.DefaultModel, d.Definition.CredentialEnv)
		answer, err := p.ask(ctx, Question{Prompt: prompt, Choices: []workspace.Choice{{Label: "Decline"}, {Label: "Allow binding"}}})
		if err != nil {
			return nil, err
		}
		if answer.Text != "Allow binding" {
			return nil, errors.New("project destination binding declined")
		}
		p.routes.grant(hash)
	}
	client, err := p.routes.clientFor(agent)
	if err != nil {
		return nil, err
	}
	return client, nil
}
func (p *cliPreferences) selectionReady() error {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	message := p.launch.SelectionError
	p.mu.RUnlock()
	if message != "" {
		return errors.New(message)
	}
	return nil
}
func (p *cliPreferences) bind(ctx context.Context, agent dashi.Agent) error {
	if err := p.selectionReady(); err != nil {
		return err
	}
	client, err := p.clientFor(ctx, agent)
	if err != nil {
		return err
	}
	p.client.bind(client, nil)
	return nil
}
func (p *cliPreferences) metadata() json.RawMessage {
	p.mu.RLock()
	defer p.mu.RUnlock()
	temperature := preferenceField[float64]{Set: true, Null: p.launch.Controls.Settings.Temperature == nil}
	if !temperature.Null {
		temperature.Value = *p.launch.Controls.Settings.Temperature
	}
	effort := ""
	if p.launch.Controls.Settings.Reasoning != nil {
		effort = p.launch.Controls.Settings.Reasoning.Effort
	}
	// Encode actual JSON values rather than the presence bookkeeping structs.
	fields := map[string]any{"provider": p.launch.Selection.Destination.ID, "model": p.launch.Selection.Model}
	if p.launch.Controls.Settings.Reasoning != nil || p.launch.Controls.Sources["thinking_effort"].Winning != "" {
		fields["thinking_effort"] = effort
	}
	if p.launch.Controls.Settings.ReasoningSummary != nil || p.launch.Controls.Sources["show_thinking"].Winning != "" {
		fields["show_thinking"] = p.launch.Controls.ShowThinking
	}
	if !temperature.Null || p.launch.Controls.Sources["temperature"].Winning != "" {
		fields["temperature"] = nil
		if !temperature.Null {
			fields["temperature"] = temperature.Value
		}
	}
	if p.launch.Controls.Settings.MaxTokens != 0 || p.launch.Controls.Sources["max_tokens"].Winning != "" {
		fields["max_tokens"] = p.launch.Controls.Settings.MaxTokens
	}
	raw, _ := json.Marshal(fields)
	return raw
}
func reopenedPreferences(raw json.RawMessage, legacyModel string) (preferenceFile, error) {
	if len(raw) == 0 {
		return preferenceFile{modelSelection: modelSelection{Model: legacyModel}}, nil
	}
	var p preferenceFile
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return p, errors.New("invalid saved next-turn preferences")
	}
	if json.Unmarshal(raw, &p) != nil {
		return p, errors.New("invalid saved next-turn preferences")
	}
	if err := p.generationPreferences.validate(); err != nil {
		return p, err
	}
	return p, nil
}
func (p *cliPreferences) choose(ctx context.Context, provider, model string) (resolvedSelection, llm.Request, error) {
	selection, err := resolveSelection(p.routes.registry, []selectionTier{{modelSelection: modelSelection{Provider: provider, Model: model}, Source: "session change"}})
	if err != nil {
		return selection, llm.Request{}, err
	}
	p.mu.RLock()
	settings := p.launch.Controls.Settings
	p.mu.RUnlock()
	agent, err := selectionAgent(selection, settings)
	if err != nil {
		return selection, llm.Request{}, err
	}
	// Candidate preparation never changes the active turn client.
	client, err := p.clientFor(ctx, agent)
	if err != nil {
		return selection, llm.Request{}, err
	}
	request, err := client.Prepare(ctx, llm.Input{Model: agent.Model, Settings: settings, Stream: true, Messages: []llm.Message{llm.UserMessage(llm.Text("Preparation preview."))}})
	return selection, request, err
}

// modelChoice is one row of the /model picker. A row without a provider
// opens the complete list.
type modelChoice struct {
	provider, model string
	// label is the destination and model ID; note is short muted text,
	// such as the context window.
	label, note string
}

// modelMenu is what the /model picker shows: rows for ready destinations,
// setup lines for destinations that are not ready yet, and the row of the
// selected model.
type modelMenu struct {
	choices  []modelChoice
	setup    []string
	selected int
}

// modelChoices lists the models of ready destinations. Unless all is set,
// the picker offers the user's featured_models when set, or else each
// built-in destination's featured models and every model of a destination
// from user settings. A listed destination that is not ready gets a setup
// line instead of rows, and a last row opens the complete list. The
// complete list shows every registered model, with the reason a
// destination is not ready. The selected model is always listed.
func (p *cliPreferences) modelChoices(all bool) modelMenu {
	current := p.agent()
	var menu modelMenu
	var problems []string
	total := 0
	for _, d := range p.routes.registry {
		total += len(d.Definition.Models)
	}
	// groups are the destinations to show, in order, with their models.
	var groups []featuredList
	featured, problem := parseFeaturedModels(p.featuredModels)
	if problem != "" {
		problems = append(problems, problem)
	}
	switch {
	case all:
		featured = nil
		fallthrough
	case len(featured) == 0:
		for _, id := range slices.Sorted(slices.Values(mapKeys(p.routes.registry))) {
			d := p.routes.registry[id]
			models := slices.Sorted(slices.Values(d.Definition.Models))
			if !all && len(d.FeaturedModels) != 0 {
				models = d.FeaturedModels
			}
			groups = append(groups, featuredList{destination: id, models: models})
		}
	default:
		for _, list := range featured {
			d, ok := p.routes.registry[list.destination]
			if !ok || len(d.Definition.Models) == 0 {
				problems = append(problems, fmt.Sprintf("featured_models: %s is not registered", list.destination))
				continue
			}
			var models []string
			for _, model := range list.models {
				if !slices.Contains(d.Definition.Models, model) {
					problems = append(problems, fmt.Sprintf("featured_models: %s %s is not registered", list.destination, model))
				} else if !slices.Contains(models, model) {
					models = append(models, model)
				}
			}
			groups = append(groups, featuredList{destination: list.destination, models: models})
		}
	}
	// The selected model's destination joins the list when it is unlisted.
	if !all && !slices.ContainsFunc(groups, func(g featuredList) bool { return g.destination == current.Provider }) {
		groups = append(groups, featuredList{destination: current.Provider})
	}
	var waiting []modelChoice
	var keys, keyed []string // credential variables to set, and their destinations
	seen := map[string]bool{}
	for _, group := range groups {
		id := group.destination
		d := p.routes.registry[id]
		if len(d.Definition.Models) == 0 || seen[id] {
			continue // nothing to offer until models are registered
		}
		seen[id] = true
		status, setup, key := p.destinationReadiness(d)
		models := slices.DeleteFunc(slices.Clone(group.models), func(model string) bool {
			return !slices.Contains(d.Definition.Models, model)
		})
		if !all {
			if key != "" {
				keys, keyed = append(keys, key), append(keyed, id)
				models = nil
			} else if status != "" {
				menu.setup = append(menu.setup, setup)
				models = nil
			}
			if id == current.Provider && slices.Contains(d.Definition.Models, current.Model) && !slices.Contains(models, current.Model) {
				models = append(models, current.Model)
			}
		}
		for _, model := range models {
			var notes []string
			if info, known := d.modelInfo(model); known && info.Capabilities.ContextWindow != 0 {
				notes = append(notes, tokenCount(info.Capabilities.ContextWindow))
			}
			if id == current.Provider && model == current.Model {
				notes = append(notes, "current")
			}
			if status != "" {
				notes = append(notes, status)
			}
			choice := modelChoice{provider: id, model: model, note: strings.Join(notes, " · ")}
			if status != "" {
				waiting = append(waiting, choice)
			} else {
				menu.choices = append(menu.choices, choice)
			}
		}
	}
	menu.choices = append(menu.choices, waiting...)
	// One line covers every missing key, so the list keeps its room.
	switch len(keys) {
	case 0:
	case 1:
		menu.setup = append(menu.setup, fmt.Sprintf("Set %s to add %s models", keys[0], keyed[0]))
	default:
		for i := range keys {
			keys[i] += " (" + keyed[i] + ")"
		}
		last := len(keys) - 1
		list := strings.Join(keys[:last], ", ") + ", or " + keys[last]
		if last == 1 {
			list = keys[0] + " or " + keys[1]
		}
		menu.setup = append(menu.setup, "Set "+list+" to add more models")
	}
	if !all {
		menu.setup = append(menu.setup, problems...)
	}
	// Model IDs line up in one column after the destination.
	width := 0
	for _, choice := range menu.choices {
		width = max(width, len(choice.provider))
	}
	for i, choice := range menu.choices {
		menu.choices[i].label = fmt.Sprintf("%-*s  %s", width, choice.provider, choice.model)
		if choice.provider == current.Provider && choice.model == current.Model {
			menu.selected = i
		}
	}
	if hidden := total - len(menu.choices); !all && hidden > 0 {
		menu.choices = append(menu.choices, modelChoice{label: "All models…", note: fmt.Sprintf("%d more", hidden)})
	}
	return menu
}

// destinationReadiness returns empty strings when the picker can offer a
// destination's models. Otherwise status is a short row note, such as
// "needs GEMINI_API_KEY", and either key names the credential variable to
// set or setup tells the user what to do first.
func (p *cliPreferences) destinationReadiness(d configuredDestination) (status, setup, key string) {
	if !d.trustedBy(p.routes.user) && !p.routes.granted(mustHash(d)) {
		return "needs project binding trust", fmt.Sprintf("Use /model %s <model> to review the project destination %s", d.ID, d.ID), ""
	}
	value, err := d.credential(p.routes.lookup, true)
	if err != nil {
		return "invalid configuration", fmt.Sprintf("Fix the configuration of destination %s to add its models", d.ID), ""
	}
	if value == "" && (d.Definition.CredentialEnv != "" || len(d.BuiltinAliases) != 0) && d.ID != "ollama" {
		name := credentialNames(d)[0]
		return "needs " + name, "", name
	}
	return "", "", ""
}
func mustHash(d configuredDestination) string {
	hash, _ := d.hash()
	return hash
}
