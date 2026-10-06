package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/workspace"
)

// The decide features, named after the library types that do the work.
// Each one is off unless the user turns it on.
const (
	decideCommandRisk         = "command_risk"
	decidePromptInjection     = "prompt_injection"
	decideTurnWatcher         = "turn_watcher"
	decideSelectiveCompaction = "selective_compaction"
)

// decideFeatureNames lists every feature in the order /settings shows them.
var decideFeatureNames = []string{
	decideCommandRisk, decidePromptInjection, decideTurnWatcher, decideSelectiveCompaction,
}

// decideFeaturesBuilt are the features this version of the CLI runs. A
// feature that is named but not built stays off, with a warning.
var decideFeaturesBuilt = []string{decideCommandRisk, decidePromptInjection, decideTurnWatcher}

// laterFeatures names the features this version accepts but does not run.
func laterFeatures() string {
	var later []string
	for _, f := range decideFeatureNames {
		if !slices.Contains(decideFeaturesBuilt, f) {
			later = append(later, f)
		}
	}
	if len(later) == 0 {
		return ""
	}
	verb := " comes"
	if len(later) > 1 {
		verb = " come"
	}
	return " (" + strings.Join(later, ", ") + verb + " in a later release)"
}

// decideOff is the list value that turns every feature off.
const decideOff = "off"

// decideSettings is which decide features are on, and where that came
// from. The highest source present replaces the lower ones, so one source
// covers the whole list.
type decideSettings struct {
	// On holds the features turned on, including ones this version does
	// not build.
	On []string
	// Source is "--decide", "DASHI_DECIDE", the user settings file, or
	// empty when nothing set the list.
	Source string
}

// enabled reports whether feature is on and built.
func (s decideSettings) enabled(feature string) bool {
	return slices.Contains(s.On, feature) && slices.Contains(decideFeaturesBuilt, feature)
}

// any reports whether any built feature is on.
func (s decideSettings) any() bool {
	for _, f := range decideFeaturesBuilt {
		if s.enabled(f) {
			return true
		}
	}
	return false
}

// parseDecideList reads a --decide or DASHI_DECIDE value: feature names
// separated by commas, or "off".
func parseDecideList(value string) ([]string, error) {
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' })
	return checkDecideNames(fields)
}

// checkDecideNames validates feature names and drops repeats. "off" alone
// means none.
func checkDecideNames(names []string) ([]string, error) {
	out := []string{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		switch {
		case name == decideOff:
			if len(names) > 1 {
				return nil, fmt.Errorf("%q cannot be combined with feature names", decideOff)
			}
			return []string{}, nil
		case !slices.Contains(decideFeatureNames, name):
			return nil, fmt.Errorf("unknown decide feature %q; use %s, or %s%s",
				workspace.PrintableText(name), strings.Join(decideFeaturesBuilt, ", "), decideOff, laterFeatures())
		case !slices.Contains(out, name):
			out = append(out, name)
		}
	}
	return out, nil
}

// resolveDecide chooses the decide features: --decide (or DASHI_DECIDE,
// which Wonton binds to the flag), then the "decide" list in the user
// settings file. A project settings file never turns a feature on or off.
// envValue and envSet describe DASHI_DECIDE, only to name the source.
func resolveDecide(flagValue string, flagSet bool, envValue string, envSet bool, user preferenceFile) (decideSettings, error) {
	if flagSet {
		source := "--decide"
		if envSet && envValue == flagValue {
			source = "DASHI_DECIDE"
		}
		on, err := parseDecideList(flagValue)
		if err != nil {
			return decideSettings{}, fmt.Errorf("invalid %s: %w", source, err)
		}
		return decideSettings{On: on, Source: source}, nil
	}
	if len(user.Decide) == 0 {
		return decideSettings{}, nil
	}
	var names []string
	if err := json.Unmarshal(user.Decide, &names); err != nil {
		return decideSettings{}, fmt.Errorf("%s: \"decide\" must be a list of feature names, such as [%q]",
			user.Source, decideCommandRisk)
	}
	on, err := checkDecideNames(names)
	if err != nil {
		return decideSettings{}, fmt.Errorf("%s: %w", user.Source, err)
	}
	return decideSettings{On: on, Source: user.Source}, nil
}

// warnDecideSettings warns about a "decide" key in the project settings
// file, which is ignored, and about features this version does not build.
func warnDecideSettings(w io.Writer, s decideSettings, project preferenceFile) {
	if len(project.Decide) != 0 {
		fmt.Fprintf(w, "dashi: warning: %s: \"decide\" is ignored in a project settings file, "+
			"because it would send your commands out with your key. Use ~/.dashi/settings.json, "+
			"--decide, or DASHI_DECIDE.\n", workspace.PrintableText(project.Source))
	}
	for _, f := range s.On {
		if !slices.Contains(decideFeaturesBuilt, f) {
			fmt.Fprintf(w, "dashi: warning: the decide feature %s is not available in this version; it stays off.\n", f)
		}
	}
}
