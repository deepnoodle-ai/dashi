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
	decideCommandRisk     = "command_risk"
	decidePromptInjection = "prompt_injection"
	decideTurnWatcher     = "turn_watcher"
)

// decideFeatureNames lists every feature in the order /settings shows them.
var decideFeatureNames = []string{decideCommandRisk, decidePromptInjection, decideTurnWatcher}

// decideOff is the list value that turns every feature off.
const decideOff = "off"

// decideSettings is which decide features are on, and where that came
// from. The highest source present replaces the lower ones, so one source
// covers the whole list.
type decideSettings struct {
	// On holds the features turned on.
	On []string
	// Source is "--decide", "DASHI_DECIDE", the user settings file, or
	// empty when nothing set the list.
	Source string
}

// enabled reports whether feature is on.
func (s decideSettings) enabled(feature string) bool {
	return slices.Contains(s.On, feature)
}

// any reports whether any feature is on.
func (s decideSettings) any() bool { return len(s.On) > 0 }

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
			return nil, fmt.Errorf("unknown decide feature %q; use %s, or %s",
				workspace.PrintableText(name), strings.Join(decideFeatureNames, ", "), decideOff)
		case !slices.Contains(out, name):
			out = append(out, name)
		}
	}
	return out, nil
}

// resolveDecide chooses the decide features: --decide (or DASHI_DECIDE,
// which Wonton binds to the flag), then the "decide" list in the user
// settings file. A project settings file never turns a feature on or off.
// envValue and envSet describe DASHI_DECIDE, only to name the source. An
// unknown name in the settings file is ignored with a warning to w, so a
// removed feature does not stop dashi from starting.
func resolveDecide(w io.Writer, flagValue string, flagSet bool, envValue string, envSet bool, user preferenceFile) (decideSettings, error) {
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
	names = slices.DeleteFunc(names, func(name string) bool {
		name = strings.TrimSpace(name)
		if name == decideOff || slices.Contains(decideFeatureNames, name) {
			return false
		}
		fmt.Fprintf(w, "dashi: warning: %s: unknown decide feature %q is ignored\n",
			workspace.PrintableText(user.Source), workspace.PrintableText(name))
		return true
	})
	on, err := checkDecideNames(names)
	if err != nil {
		return decideSettings{}, fmt.Errorf("%s: %w", user.Source, err)
	}
	return decideSettings{On: on, Source: user.Source}, nil
}

// warnDecideSettings warns about a "decide" key in the project settings
// file, which is ignored.
func warnDecideSettings(w io.Writer, project preferenceFile) {
	if len(project.Decide) != 0 {
		fmt.Fprintf(w, "dashi: warning: %s: \"decide\" is ignored in a project settings file, "+
			"because it would send your commands out with your key. Use ~/.dashi/settings.json, "+
			"--decide, or DASHI_DECIDE.\n", workspace.PrintableText(project.Source))
	}
}
