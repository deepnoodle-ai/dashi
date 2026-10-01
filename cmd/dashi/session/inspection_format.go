package session

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// PrintableEndpoint displays identity without userinfo, query, or fragment.
// It accepts only a complete HTTP(S) endpoint and never echoes an invalid URL.
func PrintableEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return "unavailable"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return workspace.PrintableText(u.String())
}

func amounts(values map[string]money.Amount, lowerBound bool) string {
	var parts []string
	for _, currency := range slices.Sorted(maps.Keys(values)) {
		parts = append(parts, values[currency].String()+" "+currency)
	}
	if len(parts) == 0 {
		return "no recorded amount"
	}
	prefix := ""
	if lowerBound {
		prefix = "at least "
	}
	return prefix + strings.Join(parts, ", ")
}

// FormatAccounting keeps monetary and token completeness independent.
func FormatAccounting(a Accounting) string {
	var out strings.Builder
	spend := a.Spend
	fmt.Fprintf(&out, "Model money: %s\nTool money: %s", amounts(spend.Cost, a.Incomplete || spend.CostUnknown > 0 || spend.CostPartial > 0), amounts(spend.ToolCost, a.Incomplete || spend.CostUnknown > 0))
	countLabel := ""
	if a.Incomplete || a.CountsIncomplete {
		countLabel = "at least "
	}
	fmt.Fprintf(&out, "\nModel calls: %s%d; unresolved model intents: %s%d", countLabel, a.ModelCalls, countLabel, a.UnresolvedModelIntents)
	fmt.Fprintf(&out, "\nTokens: uncached input %d; cache reads %d; cache writes %d; output %d", spend.Usage.InputTokens, spend.Usage.CacheReadInputTokens, spend.Usage.CacheCreationInputTokens, spend.Usage.OutputTokens)
	if a.Incomplete || spend.UsageIncomplete {
		out.WriteString("\nUsage incomplete; retained token counts are not complete totals.")
	}
	if spend.Estimated > 0 {
		fmt.Fprintf(&out, "\nEstimated usage calls: %d", spend.Estimated)
	}
	if spend.Usage.CacheCreationInputTokensUnavailable {
		out.WriteString("\nCache-write reporting unavailable.")
	}
	if spend.Usage.TotalTokenCountMismatch {
		out.WriteString("\nProvider token totals mismatch.")
	}
	if spend.CostPartial > 0 {
		fmt.Fprintf(&out, "\nPartial charges: %d", spend.CostPartial)
	}
	if spend.CostUnknown > 0 {
		fmt.Fprintf(&out, "\nUnknown charges: %d", spend.CostUnknown)
	}
	if a.Incomplete {
		fmt.Fprintf(&out, "\nRecorded accounting incomplete; omitted Turns: %d.", a.OmittedTurns)
	}
	return workspace.PrintableText(out.String())
}

// FormatInspection reports facts only. The controller adds current ownership,
// live state, and validated target-specific guidance during later integration.
func FormatInspection(report Inspection) string {
	var out strings.Builder
	m := report.Metadata
	fmt.Fprintf(&out, "Session %s: %s\nWorkspace: %s", m.ID, m.Title, m.Dir)
	if report.RecordUnavailable {
		out.WriteString("\nRecorded snapshot: Turn index unavailable")
	} else {
		fmt.Fprintf(&out, "\nRecorded snapshot: %d readable Turns of %d indexed", report.ReadableTurns, report.IndexedTurns)
	}
	if report.RecordUnavailable {
		out.WriteString("\nTurn records unavailable; latest request and stop unknown.")
	}
	if report.Request == nil {
		out.WriteString("\nNo readable recorded request.")
	} else {
		fmt.Fprintf(&out, "\nLast request (%s at revision %d): %s", report.Request.TurnID, report.Request.Revision, report.Request.Text)
	}
	if report.RequestIncomplete {
		out.WriteString("\nNewer request facts may be missing.")
	}
	if e := report.Latest; e != nil {
		fmt.Fprintf(&out, "\nLatest execution: %s at revision %d", e.TurnID, e.Revision)
		if e.Summary {
			out.WriteString(" (summary Turn)")
		}
		if !e.Readable {
			out.WriteString("\nLatest execution unreadable; stop and destination unavailable.")
		} else {
			fmt.Fprintf(&out, "\nRecorded destination: %s; API: %s; model: %s", known(e.Destination.Provider), known(e.Destination.API), known(e.Destination.Model))
			if e.Stop.Projected {
				fmt.Fprintf(&out, "\nNo run stop recorded; replay observation: %s", known(string(e.Stop.Kind)))
			} else {
				fmt.Fprintf(&out, "\nRecorded stop: %s", known(string(e.Stop.Kind)))
			}
			if e.Stop.Limit != nil {
				fmt.Fprintf(&out, " (%s)", e.Stop.Limit.Kind)
			}
			for _, p := range e.Pending {
				fmt.Fprintf(&out, "\nPending observation: %s %s %s", p.Kind, p.ID, p.ToolStatus)
			}
			out.WriteString("\nLatest-Turn accounting:\n" + FormatAccounting(e.Accounting))
			c := e.Context
			if c.Available {
				fmt.Fprintf(&out, "\nLatest model request %s (%s): %d / %d tokens, %.1f%% (%s)", c.AttemptID, known(string(c.Purpose)), c.Tokens, c.Window, 100*float64(c.Tokens)/float64(c.Window), c.WindowSource)
			} else {
				fmt.Fprintf(&out, "\nLatest model request context unavailable: %s", c.UnavailableReason)
				if c.Estimated {
					out.WriteString(" (usage estimated; no exact percentage)")
				}
			}
		}
	}
	if report.NextDestinationUnavailable {
		out.WriteString("\nSaved next-Turn preferences unavailable.")
	} else {
		fmt.Fprintf(&out, "\nSaved next-Turn destination: %s; model: %s", known(report.NextDestination.Provider), known(report.NextDestination.Model))
	}
	if report.RecordUnavailable {
		out.WriteString("\nSession accounting unavailable; recorded boundary and omitted Turn count unknown.")
	} else {
		out.WriteString("\nSession accounting (own Turns, including summaries):\n" + FormatAccounting(report.Accounting))
	}
	for _, problem := range report.Problems {
		fmt.Fprintf(&out, "\nWarning %s %s: %s", problem.SessionID, problem.TurnID, problem.Message)
	}
	return workspace.PrintableText(out.String())
}

func known(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

// RecordedStopSummary has no executable action or live-worker assertion.
// Recovery instructions belong to the controller's separate ownership adapter.
func RecordedStopSummary(e *Execution) string {
	if e == nil {
		return "no recorded execution"
	}
	if !e.Readable {
		return "latest record unreadable"
	}
	if e.Stop.Projected {
		return "no run stop recorded; worker liveness unknown"
	}
	switch e.Stop.Kind {
	case dashi.StopCompleted:
		return "recorded completed; reopen for a new request"
	case dashi.StopInterrupted:
		return "recorded interrupted; reopen for recovery checks"
	case dashi.StopWaiting:
		return "recorded waiting; reopen to inspect pending work"
	case dashi.StopUncertain:
		return "recorded uncertain; reopen and verify effects"
	case dashi.StopLimited:
		return "recorded limited; reopen to inspect the limit"
	default:
		return "recorded " + known(string(e.Stop.Kind)) + "; reopen for inspection"
	}
}
