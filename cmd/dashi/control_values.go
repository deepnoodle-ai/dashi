package main

import (
	"github.com/deepnoodle-ai/dashi/llm"
	"strconv"
)

func controlPreferenceValue(field string, settings llm.Settings) (string, string) {
	return inspectedControlValue(field, inspectControls(settings, false))
}
func inspectedControlValue(field string, settings inspectedControls) (string, string) {
	switch field {
	case "thinking_effort":
		if settings.Effort == "" {
			return "reasoning effort", "omit"
		}
		return "reasoning effort", settings.Effort
	case "show_thinking":
		if settings.ReasoningSummary == nil {
			return "public summaries", "unset"
		}
		if *settings.ReasoningSummary {
			return "public summaries", "on"
		}
		return "public summaries", "off"
	case "temperature":
		if settings.Temperature == nil {
			return "temperature", "omit"
		}
		return "temperature", strconv.FormatFloat(*settings.Temperature, 'g', -1, 64)
	case "max_tokens":
		if settings.MaxTokens == 0 {
			return "output limit", "library default"
		}
		return "output limit", strconv.Itoa(settings.MaxTokens) + " tokens"
	}
	return field, "unknown"
}
