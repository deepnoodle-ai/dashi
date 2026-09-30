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
	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// cliPreferences keeps new-turn intent separate from the client driving an
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
}

func (p *cliPreferences) agent() dashi.Agent {
	p.mu.RLock()
	defer p.mu.RUnlock()
	agent, _ := selectionAgent(p.launch.Selection, p.launch.Controls.Settings)
	// Own pointers even when admission waits behind queued work.
	raw, _ := json.Marshal(agent)
	var owned dashi.Agent
	_ = json.Unmarshal(raw, &owned)
	return owned
}
func (p *cliPreferences) clientFor(ctx context.Context, agent dashi.Agent) (llm.Client, error) {
	d, ok := p.routes.registry[agent.Provider]
	if !ok {
		return nil, fmt.Errorf("recorded destination %q is no longer configured", agent.Provider)
	}
	fingerprint, err := d.fingerprint()
	if err != nil {
		return nil, err
	}
	// Check the recorded binding before presenting or granting new trust.
	if strings.HasPrefix(agent.Revision, cliRouteRevision) && agent.Revision != cliRouteRevision+fingerprint {
		return nil, errors.New("recorded destination binding changed; restore its configuration before continuing")
	}
	if d.Project && !d.trustedBy(p.routes.user) && !p.routes.grants[fingerprint] {
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
		p.routes.grants[fingerprint] = true
	}
	client, err := p.routes.clientFor(agent)
	if err != nil {
		return nil, err
	}
	return client, nil
}
func (p *cliPreferences) bind(ctx context.Context, agent dashi.Agent) error {
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

type modelChoice struct{ provider, model, label, detail string }

func (p *cliPreferences) modelChoices() []modelChoice {
	var choices []modelChoice
	for _, id := range slices.Sorted(slices.Values(mapKeys(p.routes.registry))) {
		d := p.routes.registry[id]
		status := "configured"
		if !d.trustedBy(p.routes.user) && !p.routes.grants[mustFingerprint(d)] {
			status = "needs project binding trust"
		} else if key, err := d.credential(p.routes.lookup, true); err != nil {
			status = "invalid configuration"
		} else if key == "" && (d.Definition.CredentialEnv != "" || len(d.BuiltinAliases) != 0) && d.ID != "ollama" {
			status = "needs credential"
		}
		for _, model := range slices.Sorted(slices.Values(d.Definition.Models)) {
			limits := "context unknown; output unknown; tools unknown; summary unknown; evidence unknown"
			if info, known := catalog.Lookup(d.Definition.API, d.Definition.Endpoint, model); known {
				limits = fmt.Sprintf("context %s; output %s; tools %s; summary %s; %s (%s)", knownLimit(info.Capabilities.ContextWindow), knownLimit(info.Capabilities.MaxOutputTokens), knownSupport(info.Capabilities.Features[llm.FeatureTools]), knownSupport(info.Capabilities.Features[llm.FeatureReasoningSummary]), info.Evidence.Source, info.Evidence.QualifiedAt.Format("2006-01-02"))
			}
			choices = append(choices, modelChoice{provider: id, model: model, label: id + " " + model, detail: status + "\n" + strings.ReplaceAll(limits, "; ", "\n")})
		}
	}
	return choices
}
func mustFingerprint(d configuredDestination) string {
	fingerprint, _ := d.fingerprint()
	return fingerprint
}
