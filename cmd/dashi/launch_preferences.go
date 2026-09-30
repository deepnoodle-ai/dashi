package main

import (
	"errors"
	"fmt"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"strings"
)

// launchPreferences resolves convenience defaults only. Recorded Agent data
// remains the authority for an open turn.
type launchPreferences struct {
	Selection resolvedSelection
	Controls  resolvedControls
}

func resolveLaunchPreferences(o Options, registry, trusted map[string]configuredDestination, user, project, reopened preferenceFile, lookup func(string) (string, bool), origins map[string]string) (launchPreferences, error) {
	source := func(name string) string {
		if path := origins[name]; path != "" {
			return "environment file " + path + ": " + name
		}
		return "process environment: " + name
	}
	environment := modelSelection{}
	var environmentSources []string
	for _, name := range []string{"DASHI_PROVIDER", "DASHI_MODEL"} {
		value, present := lookup(name)
		if !present || value == "" {
			continue
		}
		if name == "DASHI_PROVIDER" {
			environment.Provider = value
		} else {
			environment.Model = value
		}
		environmentSources = append(environmentSources, source(name))
	}
	selectionTiers := []selectionTier{
		{modelSelection: modelSelection{Provider: o.Provider, Model: o.Model}, Source: "flags"},
		{modelSelection: environment, Source: strings.Join(environmentSources, "; ")},
		{modelSelection: reopened.modelSelection, Source: "reopened session"},
		{modelSelection: project.modelSelection, Source: project.Source},
		{modelSelection: user.modelSelection, Source: user.Source},
	}
	selected := false
	for _, tier := range selectionTiers {
		selected = selected || tier.Provider != "" || tier.Model != ""
	}
	if !selected {
		selectionTiers = append(selectionTiers, detectedSelection(registry, trusted, lookup))
	}
	selection, err := resolveSelection(registry, selectionTiers)
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
	tiers := []generationTier{{generationPreferences: o.Controls, Source: "flags"}}
	for _, control := range []struct{ name, flag string }{
		{"DASHI_THINKING_EFFORT", "--thinking-effort"},
		{"DASHI_SHOW_THINKING", "--show-thinking"},
		{"DASHI_TEMPERATURE", "--temperature"},
		{"DASHI_MAX_TOKENS", "--max-tokens"},
	} {
		value, present := lookup(control.name)
		if !present {
			continue
		}
		// The shared flag parser validates identical syntax, including omission.
		args := []string{control.flag + "=" + value}
		parsed, err := parseArgs(args)
		if err != nil {
			return launchPreferences{}, fmt.Errorf("invalid %s", control.name)
		}
		tiers = append(tiers, generationTier{generationPreferences: parsed.Controls, Source: source(control.name)})
	}
	tiers = append(tiers,
		generationTier{generationPreferences: reopened.generationPreferences, Source: "reopened session"},
		generationTier{generationPreferences: project.generationPreferences, Source: project.Source},
		generationTier{generationPreferences: user.generationPreferences, Source: user.Source},
	)
	controls, err := resolveControls(tiers)
	if err != nil {
		return launchPreferences{}, err
	}
	return launchPreferences{Selection: selection, Controls: controls}, nil
}

func loadReopenedPreferences(store *session.Store, dir string, o Options) (preferenceFile, error) {
	if !o.Resume {
		return preferenceFile{}, nil
	}
	var saved *session.Session
	var err error
	if o.ResumeID != "" {
		saved, err = store.Load(o.ResumeID)
	} else {
		saved, err = store.Latest(dir)
	}
	if errors.Is(err, session.ErrNotFound) && o.ResumeID == "" {
		return preferenceFile{}, nil
	}
	if err != nil {
		return preferenceFile{}, err
	}
	return reopenedPreferences(saved.NextTurn, saved.Model)
}
