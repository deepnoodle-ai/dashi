package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"text/tabwriter"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// modelInventory is an offline listing. It constructs no provider client
// and probes no endpoint. Project bindings need trust before key lookup.
func modelInventory(registry, user map[string]configuredDestination, lookup func(string) (string, bool), availableOnly bool, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "DESTINATION\tMODEL\tAVAILABILITY\tCONTEXT\tOUTPUT\tTOOLS\tREASONING\tSUMMARY\tEVIDENCE")
	for _, id := range slices.Sorted(slices.Values(mapKeys(registry))) {
		d := registry[id]
		availability := "needs model registration"
		if len(d.Definition.Models) != 0 {
			availability = "configured"
			if !d.trustedBy(user) {
				availability = "needs project binding trust"
			} else {
				key, err := d.credential(lookup, true)
				if err != nil {
					return err
				}
				if key == "" && (d.Definition.CredentialEnv != "" || len(d.BuiltinAliases) != 0) && d.ID != "ollama" {
					availability = "needs credential"
				}
			}
		}
		if availableOnly && availability != "configured" {
			continue
		}
		if len(d.Definition.Models) == 0 {
			fmt.Fprintf(w, "%s\t—\t%s\tunknown\tunknown\tunknown\tunknown\tunknown\tunknown\n", id, availability)
		}
		for _, model := range slices.Sorted(slices.Values(d.Definition.Models)) {
			info, known := d.modelInfo(model)
			context, output, tools, reasoning, summary, evidence := "unknown", "unknown", "unknown", "unknown", "unknown", "unknown"
			if known {
				context, output = knownLimit(info.Capabilities.ContextWindow), knownLimit(info.Capabilities.MaxOutputTokens)
				tools, reasoning = knownSupport(info.Capabilities.Features[llm.FeatureTools]), knownSupport(info.Capabilities.Features[llm.FeatureReasoningEffort])
				summary = knownSupport(info.Capabilities.Features[llm.FeatureReasoningSummary])
				evidence = info.Evidence.Source + " (" + info.Evidence.QualifiedAt.Format("2006-01-02") + ")"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, model, availability, context, output, tools, reasoning, summary, evidence)
		}
	}
	return w.Flush()
}
func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
func knownLimit(value int) string {
	if value == 0 {
		return "unknown"
	}
	return strconv.Itoa(value)
}
func knownSupport(value llm.Support) string {
	if value == "" {
		return "unknown"
	}
	return string(value)
}

func readStartupPreferences(path string, project bool) (preferenceFile, error) {
	p, err := readPreferenceFile(path, project)
	var failed *preferenceSourceError
	if errors.As(err, &failed) {
		// Discard partial decoded selectors/definitions. Nothing from this source
		// may influence a route or restore a lower-tier automatic selection.
		return preferenceFile{Source: path, Project: project, LoadError: failed.Error()}, nil
	}
	return p, err
}
func warnPreferenceSources(out io.Writer, sources ...preferenceFile) {
	for _, source := range sources {
		if source.LoadError != "" {
			fmt.Fprintf(out, "dashi: %s; source ignored. Select an explicit destination before work.\n", workspace.PrintableText(source.LoadError))
		}
	}
}
func loadDestinationFiles(home, workspace string) (preferenceFile, preferenceFile, map[string]configuredDestination, map[string]configuredDestination, error) {
	user, err := readStartupPreferences(filepath.Join(home, ".dashi", "settings.json"), false)
	if err != nil {
		return user, preferenceFile{}, nil, nil, err
	}
	project, err := readStartupPreferences(filepath.Join(workspace, ".dashi", "settings.json"), true)
	if err != nil {
		return user, project, nil, nil, err
	}
	trusted, err := mergeDestinations(user)
	if err != nil {
		return user, project, nil, nil, err
	}
	registry, err := mergeDestinations(user, project)
	return user, project, registry, trusted, err
}
func runModels(o Options, home, invocation string, out, stderr io.Writer) int {
	workspace, err := canonicalWorkspace(invocation, o.Workspace)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	if _, err := loadNamedEnvironment(invocation, o.EnvFiles, os.LookupEnv, os.Setenv); err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	user, project, registry, trusted, err := loadDestinationFiles(home, workspace)
	warnPreferenceSources(stderr, user, project)
	if err == nil {
		err = modelInventory(registry, trusted, os.LookupEnv, o.Available, out)
	}
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	return 0
}
