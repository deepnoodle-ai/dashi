package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
)

// modelInventory is an offline listing. It constructs no provider client
// and probes no endpoint. Project bindings need trust before key lookup.
// Each destination gets a status line and a table of its models.
func modelInventory(registry, user map[string]configuredDestination, lookup func(string) (string, bool), availableOnly bool, out io.Writer) error {
	columns := []tui.TableColumn{{Title: "MODEL"}, {Title: "CONTEXT"}, {Title: "OUTPUT"}, {Title: "REASONING"}}
	first := true
	for _, id := range slices.Sorted(slices.Values(mapKeys(registry))) {
		d := registry[id]
		status := "needs model registration"
		if len(d.Definition.Models) != 0 {
			status = "ready"
			if !d.trustedBy(user) {
				status = "needs project binding trust"
			} else {
				key, err := d.credential(lookup, true)
				if err != nil {
					return err
				}
				if key == "" && (d.Definition.CredentialEnv != "" || len(d.BuiltinAliases) != 0) && d.ID != "ollama" {
					status = "needs " + strings.Join(credentialNames(d), " or ")
				}
			}
		}
		if availableOnly && status != "ready" {
			continue
		}
		if !first {
			fmt.Fprintln(out)
		}
		first = false
		fmt.Fprintf(out, "%s: %s\n", id, status)
		if len(d.Definition.Models) == 0 {
			continue
		}
		var rows [][]string
		for _, model := range slices.Sorted(slices.Values(d.Definition.Models)) {
			info, known := d.modelInfo(model)
			context, output, reasoning := "?", "?", "?"
			if known {
				context, output = tokenCount(info.Capabilities.ContextWindow), tokenCount(info.Capabilities.MaxOutputTokens)
				reasoning = supportWord(info.Capabilities.Features[llm.FeatureReasoningEffort])
			}
			rows = append(rows, []string{model, context, output, reasoning})
		}
		if err := tui.Fprintln(out, tui.Table(columns, nil).Rows(rows)); err != nil {
			return err
		}
	}
	return nil
}

// credentialNames are the variables that can hold d's credential.
func credentialNames(d configuredDestination) []string {
	if len(d.BuiltinAliases) != 0 {
		return d.BuiltinAliases
	}
	return []string{d.Definition.CredentialEnv}
}

// tokenCount writes a token limit short, such as 200K or 1M, or ? when it
// is unknown.
func tokenCount(n int) string {
	switch {
	case n == 0:
		return "?"
	case n >= 1_000_000:
		return strconv.FormatFloat(math.Round(float64(n)/100_000)/10, 'f', -1, 64) + "M"
	case n >= 1_000:
		return strconv.Itoa(int(math.Round(float64(n)/1_000))) + "K"
	}
	return strconv.Itoa(n)
}

// supportWord writes a feature's support as yes, no, or ? when unknown.
func supportWord(s llm.Support) string {
	switch s {
	case llm.SupportSupported:
		return "yes"
	case llm.SupportUnsupported:
		return "no"
	}
	return "?"
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
