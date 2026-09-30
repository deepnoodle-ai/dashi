package main

import (
	"fmt"
	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"strings"
)

// Recorded preparation facts are available at this boundary before dispatch.
func modelPreparationBlocks(step dashi.Step) []Block {
	if step.Model == nil {
		return nil
	}
	fact := step.Model
	var lines []string
	for _, adjustment := range fact.Adjustments {
		if adjustment.Action == llm.AdjustmentDefault {
			continue
		}
		label := string(adjustment.Field)
		if adjustment.Field == llm.SettingReasoningSummary {
			label = "public summaries"
		}
		lines = append(lines, fmt.Sprintf("%s %s → %s (%s). %s", label, adjustment.Requested, adjustment.Effective, adjustment.Action, adjustment.Reason))
	}
	for _, change := range fact.ContentChanges {
		if warning, ok := change.Warning(); ok {
			lines = append(lines, warning.Message)
		}
	}
	if len(fact.Unverified) > 0 {
		lines = append(lines, fmt.Sprintf("Unverified support: %v.", fact.Unverified))
	}
	if len(lines) == 0 {
		return nil
	}
	return []Block{{ID: "preparation:" + fact.AttemptID, Kind: KindNotice, Text: strings.Join(lines, "\n"), Done: true}}
}
