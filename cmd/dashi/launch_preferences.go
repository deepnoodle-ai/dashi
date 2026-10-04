package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
)

// launchPreferences resolves convenience defaults only. Recorded Agent data
// remains the authority for an open turn.
type launchPreferences struct {
	Selection      resolvedSelection
	SelectionError string
	Controls       resolvedControls
}

// commandLineSource names the tier of flags and the DASHI_* variables bound
// to them. The command line resolves a flag over its variable.
const commandLineSource = "flags or DASHI_* variables"

func resolveLaunchPreferences(o Options, registry, trusted map[string]configuredDestination, user, project, reopened preferenceFile, lookup func(string) (string, bool)) (launchPreferences, error) {
	selectionTiers := []selectionTier{
		{modelSelection: modelSelection{Provider: o.Provider, Model: o.Model}, Source: commandLineSource},
		{modelSelection: reopened.modelSelection, Source: "reopened session"},
		{modelSelection: project.modelSelection, Source: project.Source},
		{modelSelection: user.modelSelection, Source: user.Source},
	}
	selected := false
	for _, tier := range selectionTiers {
		selected = selected || tier.Provider != "" || tier.Model != ""
	}
	selectionError := ""
	if (user.LoadError != "" || project.LoadError != "") && o.Provider == "" && o.Model == "" && reopened.Provider == "" && reopened.Model == "" {
		selectionError = "settings source unavailable; select an explicit destination with --provider, --model, or /model DESTINATION MODEL before work"
	}
	if !selected && selectionError == "" {
		selectionTiers = append(selectionTiers, detectedSelection(registry, trusted, lookup))
	}
	var selection resolvedSelection
	var err error
	if selectionError == "" {
		selection, err = resolveSelection(registry, selectionTiers)
	} else {
		if selected {
			_, err = resolveSelection(registry, selectionTiers)
		}
		selection.Source = "explicit destination required (settings source unavailable)"
	}
	if err != nil {
		return launchPreferences{}, err
	}
	if o.APIEndpoint != "" {
		if o.Provider == "" {
			return launchPreferences{}, fmt.Errorf("--api-endpoint requires an explicit --provider with registered API configuration")
		}
		if len(selection.Destination.BuiltinAliases) != 0 {
			return launchPreferences{}, fmt.Errorf("--api-endpoint requires a custom destination identity")
		}
		selection.Destination.Definition.Endpoint = o.APIEndpoint
		selection.Destination.Source = "--api-endpoint"
		if err := selection.Destination.validate(); err != nil {
			return launchPreferences{}, err
		}
	}
	tiers := []generationTier{
		{generationPreferences: o.Controls, Source: commandLineSource},
		{generationPreferences: reopened.generationPreferences, Source: "reopened session"},
		{generationPreferences: project.generationPreferences, Source: project.Source},
		{generationPreferences: user.generationPreferences, Source: user.Source},
	}
	controls, err := resolveControls(tiers)
	if err != nil {
		return launchPreferences{}, err
	}
	return launchPreferences{Selection: selection, Controls: controls, SelectionError: selectionError}, nil
}

func loadReopenedPreferences(store *sessionstore.Store, dir string, o Options) (preferenceFile, error) {
	if !o.Resume {
		return preferenceFile{}, nil
	}
	var saved *sessionstore.Session
	var err error
	if o.ResumeID != "" {
		saved, err = store.LoadScoped(context.Background(), dir, o.ResumeID)
	} else {
		saved, err = store.Latest(dir)
	}
	if errors.Is(err, sessionstore.ErrNotFound) && o.ResumeID == "" {
		return preferenceFile{}, nil
	}
	if err != nil {
		return preferenceFile{}, err
	}
	return reopenedPreferences(saved.NextTurn, saved.Model)
}
