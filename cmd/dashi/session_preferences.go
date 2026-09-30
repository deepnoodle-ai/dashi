package main

// selectionSourceTier retains the previous pair and source when interactive
// preferences override it. Values remain next-turn intent, never recorded facts.
func selectionSourceTier(selection resolvedSelection) selectionTier {
	source := selection.Source
	if source == "" {
		source = "unknown"
	}
	return selectionTier{modelSelection: modelSelection{Provider: selection.Destination.ID, Model: selection.Model}, Source: source}
}

// selectedSessionPreferences reuses startup precedence for the selected session.
// Interactive changes remain the highest tier until this invocation exits.
func (p *cliPreferences) selectedSessionPreferences(saved preferenceFile) (launchPreferences, error) {
	p.mu.RLock()
	current := p.launch
	p.mu.RUnlock()
	var candidate launchPreferences
	var err error
	if p.resolveSession != nil {
		candidate, err = p.resolveSession(saved)
	} else {
		// Embedders and keyless fixtures can omit a startup resolver. Their current
		// defaults are below a selected session's explicit saved preferences.
		candidate.Selection, err = resolveSelection(p.routes.registry, []selectionTier{{modelSelection: saved.modelSelection, Source: "reopened session"}, selectionSourceTier(current.Selection)})
		if err == nil {
			base, parseErr := reopenedPreferences(p.metadata(), "")
			if parseErr != nil {
				return candidate, parseErr
			}
			candidate.Controls, err = resolveControls([]generationTier{{generationPreferences: saved.generationPreferences, Source: "reopened session"}, {generationPreferences: base.generationPreferences, Source: "current defaults"}})
		}
	}
	if err != nil {
		return candidate, err
	}
	if current.Selection.Source == "session change" {
		selected := current.Selection
		selected.Overridden = append(append([]selectionTier{selectionSourceTier(candidate.Selection)}, candidate.Selection.Overridden...), selected.Overridden...)
		candidate.Selection = selected
		candidate.SelectionError = ""
	}
	for _, field := range []string{"thinking_effort", "show_thinking", "temperature", "max_tokens"} {
		source := current.Controls.Sources[field]
		if source.Winning != "session change" {
			continue
		}
		prior := candidate.Controls.Sources[field]
		if prior.Winning != "" {
			source.Overridden = append(append([]string{prior.Winning}, prior.Overridden...), source.Overridden...)
		}
		candidate.Controls.Sources[field] = source
		switch field {
		case "thinking_effort":
			candidate.Controls.Settings.Reasoning = current.Controls.Settings.Reasoning
		case "show_thinking":
			candidate.Controls.Settings.ReasoningSummary = current.Controls.Settings.ReasoningSummary
			candidate.Controls.ShowThinking = current.Controls.ShowThinking
		case "temperature":
			candidate.Controls.Settings.Temperature = current.Controls.Settings.Temperature
		case "max_tokens":
			candidate.Controls.Settings.MaxTokens = current.Controls.Settings.MaxTokens
		}
	}
	return candidate, nil
}
