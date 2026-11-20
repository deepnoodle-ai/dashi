package images

import (
	"errors"
	"fmt"
)

// Options holds controls specific to Meta's Images API. An unset field leaves
// Meta's default in effect. None of them changes the per-image price.
type Options struct {
	// ReasoningStrength is "low" (a single pass) or "high" (iterative
	// refinement, Meta's default).
	ReasoningStrength string `json:"reasoning_strength,omitempty"`
	// Temperature, 0 to 2, affects only Muse's planning step, not image
	// sampling.
	Temperature *float64 `json:"temperature,omitempty"`
	// ToolEnablement turns off the built-in tools Muse may use while
	// planning; each defaults to on.
	ToolEnablement *ToolEnablement `json:"tool_enablement,omitempty"`
	// User is a stable end-user ID Meta uses for abuse detection.
	User string `json:"user,omitempty"`
}

// ToolEnablement switches Muse's built-in planning tools. A nil field leaves
// the tool on.
type ToolEnablement struct {
	ImageSearch *bool `json:"enable_image_search,omitempty"`
	WebSearch   *bool `json:"enable_web_search,omitempty"`
	Shell       *bool `json:"enable_shell,omitempty"`
}

func (o Options) clone() Options {
	if o.Temperature != nil {
		value := *o.Temperature
		o.Temperature = &value
	}
	if o.ToolEnablement != nil {
		tools := *o.ToolEnablement
		for _, field := range []**bool{&tools.ImageSearch, &tools.WebSearch, &tools.Shell} {
			if *field != nil {
				value := **field
				*field = &value
			}
		}
		o.ToolEnablement = &tools
	}
	return o
}

func (o Options) fields() (map[string]any, error) {
	fields := make(map[string]any)
	switch o.ReasoningStrength {
	case "":
	case "low", "high":
		fields["reasoning_strength"] = o.ReasoningStrength
	default:
		return nil, fmt.Errorf("images: unsupported reasoning strength %q", o.ReasoningStrength)
	}
	if o.Temperature != nil {
		if *o.Temperature < 0 || *o.Temperature > 2 {
			return nil, errors.New("images: temperature must be between 0 and 2")
		}
		fields["temperature"] = *o.Temperature
	}
	if t := o.ToolEnablement; t != nil {
		tools := map[string]bool{}
		if t.ImageSearch != nil {
			tools["enable_image_search"] = *t.ImageSearch
		}
		if t.WebSearch != nil {
			tools["enable_web_search"] = *t.WebSearch
		}
		if t.Shell != nil {
			tools["enable_shell"] = *t.Shell
		}
		if len(tools) > 0 {
			fields["tool_enablement"] = tools
		}
	}
	if o.User != "" {
		fields["user"] = o.User
	}
	return fields, nil
}
