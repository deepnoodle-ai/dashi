package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// settingsInspection is deliberately independent of configuration and request
// snapshots. Export has no key values, headers, prompt text, or history.
type settingsInspection struct {
	CredentialVariable         string                          `json:"credential_variable,omitempty"`
	CredentialSource           string                          `json:"credential_source"`
	EnvironmentFiles           []string                        `json:"environment_files,omitempty"`
	OverriddenSelectionSources []string                        `json:"overridden_selection_sources,omitempty"`
	Knowledge                  *inspectedKnowledge             `json:"model_knowledge,omitempty"`
	Destination                string                          `json:"destination"`
	Model                      string                          `json:"model"`
	API                        string                          `json:"api"`
	Endpoint                   string                          `json:"endpoint"`
	SelectionSource            string                          `json:"selection_source"`
	Sources                    map[string]preferenceProvenance `json:"sources"`
	Requested                  inspectedControls               `json:"requested"`
	Effective                  *inspectedControls              `json:"effective,omitempty"`
	Warnings                   []llm.Warning                   `json:"warnings,omitempty"`
	Unverified                 []llm.Feature                   `json:"unverified,omitempty"`
	PreviewError               string                          `json:"preview_error,omitempty"`
	PromptSource               string                          `json:"prompt_source"`
	Recorded                   *recordedSettings               `json:"recorded_attempt,omitempty"`
}
type inspectedKnowledge struct {
	ContextWindow   int                         `json:"context_window,omitempty"`
	MaxOutputTokens int                         `json:"max_output_tokens,omitempty"`
	Features        map[llm.Feature]llm.Support `json:"features"`
	Source          string                      `json:"source"`
	QualifiedAt     string                      `json:"qualified_at"`
}
type inspectedControls struct {
	Effort           string   `json:"thinking_effort"`
	ReasoningSummary *bool    `json:"reasoning_summary,omitempty"`
	ShowThinking     bool     `json:"show_thinking"`
	Temperature      *float64 `json:"temperature"`
	MaxTokens        int      `json:"max_tokens"`
}
type recordedSettings struct {
	AttemptID   string            `json:"attempt_id"`
	Destination string            `json:"destination"`
	Model       string            `json:"model"`
	Effective   inspectedControls `json:"effective"`
	Summary     string            `json:"summary"`
}

func inspectControls(settings llm.Settings, show bool) inspectedControls {
	effort := ""
	if settings.Reasoning != nil {
		effort = settings.Reasoning.Effort
	}
	return inspectedControls{Effort: effort, ReasoningSummary: settings.ReasoningSummary, ShowThinking: show, Temperature: settings.Temperature, MaxTokens: settings.MaxTokens}
}
func (e *Engine) inspectSettings(ctx context.Context) settingsInspection {
	p := e.cfg.Preferences
	p.mu.RLock()
	launch := p.launch
	promptSource := "built-in guidance"
	if p.guidanceSet {
		promptSource = "--system-prompt"
	}
	p.mu.RUnlock()
	d := launch.Selection.Destination
	view := settingsInspection{Destination: d.ID, Model: launch.Selection.Model, API: d.Definition.API, Endpoint: d.Definition.Endpoint, SelectionSource: launch.Selection.Source, Sources: launch.Controls.Sources, Requested: inspectControls(launch.Controls.Settings, launch.Controls.ShowThinking), PromptSource: promptSource}
	view.EnvironmentFiles = append([]string(nil), p.environmentFiles...)
	for _, overridden := range launch.Selection.Overridden {
		view.OverriddenSelectionSources = append(view.OverriddenSelectionSources, overridden.Source)
	}
	if info, known := d.modelInfo(view.Model); known {
		view.Knowledge = &inspectedKnowledge{ContextWindow: info.Capabilities.ContextWindow, MaxOutputTokens: info.Capabilities.MaxOutputTokens, Features: map[llm.Feature]llm.Support{}, Source: info.Evidence.Source, QualifiedAt: info.Evidence.QualifiedAt.Format("2006-01-02")}
		for _, feature := range []llm.Feature{llm.FeatureTools, llm.FeatureStreaming, llm.FeatureReasoningEffort, llm.FeatureReasoningSummary} {
			support := info.Capabilities.Features[feature]
			if support == "" {
				support = llm.SupportUnknown
			}
			view.Knowledge.Features[feature] = support
		}
	}
	view.CredentialSource = "none required"
	fingerprint, _ := d.fingerprint()
	if d.Project && !d.trustedBy(p.routes.user) && !p.routes.grants[fingerprint] {
		view.CredentialVariable = d.Definition.CredentialEnv
		view.CredentialSource = "project binding trust required"
	} else {
		names := []string{d.Definition.CredentialEnv}
		if len(d.BuiltinAliases) != 0 {
			names = d.BuiltinAliases
		}
		for _, name := range names {
			if name == "" {
				continue
			}
			view.CredentialVariable, view.CredentialSource = name, "missing"
			if value, present := p.routes.lookup(name); present && value != "" {
				view.CredentialSource = "process environment"
				if path := p.environmentOrigins[name]; path != "" {
					view.CredentialSource = "environment file " + path
				}
				break
			}
		}
		if view.CredentialSource == "missing" && d.ID == "ollama" {
			view.CredentialSource = "built-in placeholder"
		}
	}
	if launch.SelectionError != "" {
		view.PreviewError = launch.SelectionError
		return view
	}
	client, err := p.routes.factory(d, p.routes.lookup, d.trustedBy(p.routes.user) || p.routes.grants[fingerprint])
	if err == nil {
		var request llm.Request
		request, err = client.Prepare(ctx, llm.Input{Model: view.Model, Stream: true, Settings: launch.Controls.Settings, Messages: []llm.Message{llm.UserMessage(llm.Text("Preparation preview."))}})
		if err == nil {
			effective := inspectControls(request.Effective(), launch.Controls.ShowThinking)
			view.Effective, view.Warnings, view.Unverified = &effective, request.Warnings(), request.Snapshot().Unverified
		}
	}
	if err != nil {
		view.PreviewError = err.Error()
	}
	if last := e.Last(ctx); last != nil {
		loader, ok := e.handle().Runner.Recorder.(dashi.Loader)
		if !ok {
			return view
		}
		turn, err := loader.Load(ctx, last.View.ID)
		if err == nil {
			for _, step := range turn.Steps {
				if step.Kind == dashi.StepModelIntent && step.Model != nil {
					fact := step.Model
					show := last.View.Agent.Settings.ReasoningSummary != nil && *last.View.Agent.Settings.ReasoningSummary
					view.Recorded = &recordedSettings{AttemptID: fact.AttemptID, Destination: fact.Provider, Model: fact.Model, Effective: inspectControls(fact.Effective, show), Summary: "no settled summary"}
				}
				if view.Recorded != nil && step.Model != nil && step.Model.AttemptID == view.Recorded.AttemptID && step.Model.Result != nil {
					view.Recorded.Summary = "no summary returned"
					for _, part := range step.Model.Result.Message.Content {
						if part.Kind == llm.ContentReasoning && part.Reasoning != nil && part.Reasoning.Text != "" {
							view.Recorded.Summary = "public summary returned"
						}
					}
				}
			}
		}
	}
	return view
}
func (c *Controller) settings(ctx context.Context, arg string) {
	if c.eng.cfg.Preferences == nil {
		c.notice("Settings inspection needs configured destinations.")
		return
	}
	if arg != "" && arg != "--json" {
		c.notice("Usage: /settings [--json]")
		return
	}
	view := c.eng.inspectSettings(ctx)
	if arg == "--json" {
		raw, _ := json.MarshalIndent(view, "", "  ")
		c.notice("%s", string(raw))
		return
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Next turn: %s %s\nAPI: %s\nEndpoint: %s\nSelection source: %s\nPrompt source: %s", view.Destination, view.Model, view.API, view.Endpoint, view.SelectionSource, view.PromptSource)
	fmt.Fprintf(&text, "\nCredential variable: %s (%s)", view.CredentialVariable, view.CredentialSource)
	if len(view.EnvironmentFiles) > 0 {
		fmt.Fprintf(&text, "\nExplicit environment files: %s", strings.Join(view.EnvironmentFiles, ", "))
	}
	if len(view.OverriddenSelectionSources) > 0 {
		fmt.Fprintf(&text, "\nOverridden selection sources: %s", strings.Join(view.OverriddenSelectionSources, "; "))
	}
	if view.Knowledge != nil {
		info := view.Knowledge
		fmt.Fprintf(&text, "\nModel facts: context %s; output %s; %s (%s)", knownLimit(info.ContextWindow), knownLimit(info.MaxOutputTokens), info.Source, info.QualifiedAt)
	} else {
		text.WriteString("\nModel facts: unknown")
	}

	text.WriteString("\nPreparation preview (not dispatched):")
	for _, field := range []string{"thinking_effort", "show_thinking", "temperature", "max_tokens"} {
		label, requested := inspectedControlValue(field, view.Requested)
		effective := "unavailable"
		if view.Effective != nil {
			_, effective = inspectedControlValue(field, *view.Effective)
		}
		source := view.Sources[field]
		winning := source.Winning
		if winning == "" {
			winning = "default"
		}
		fmt.Fprintf(&text, "\n%s: %s → %s · %s", label, requested, effective, winning)
		if len(source.Overridden) > 0 {
			fmt.Fprintf(&text, " (overrides %s)", strings.Join(source.Overridden, ", "))
		}
	}

	for _, warning := range view.Warnings {
		fmt.Fprintf(&text, "\nWarning: %s", warning.Message)
	}
	if len(view.Unverified) != 0 {
		fmt.Fprintf(&text, "\nUnverified support: %v", view.Unverified)
	}
	if view.PreviewError != "" {
		fmt.Fprintf(&text, "\nCannot prepare preview: %s", view.PreviewError)
	}
	if view.Recorded != nil {
		fmt.Fprintf(&text, "\nRecorded attempt %s: %s %s; %s", view.Recorded.AttemptID, view.Recorded.Destination, view.Recorded.Model, view.Recorded.Summary)
		for _, field := range []string{"thinking_effort", "show_thinking", "temperature", "max_tokens"} {
			label, value := inspectedControlValue(field, view.Recorded.Effective)
			fmt.Fprintf(&text, "\nRecorded %s: %s", label, value)
		}
	}
	c.notice("%s", text.String())
}
