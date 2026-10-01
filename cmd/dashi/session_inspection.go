package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// The owner snapshot contributes host facts only. All recorded facts come from
// the single Store scan, including recovery observations for the latest Turn.
type inspectionOwner struct {
	ID    string
	Owned bool
	Mode  Mode
}

func (e *Engine) inspectionMetadata() (*sessionstore.Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess == nil {
		return nil, false
	}
	s := *e.sess
	s.NextTurn = slices.Clone(s.NextTurn)
	return &s, e.unlock != nil
}

func (e *Engine) RenameCurrent(title string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess == nil || e.unlock == nil {
		return errors.New("the current session is not owned by this process")
	}
	updated, err := e.cfg.Store.Rename(e.sess, title)
	if err != nil {
		return err
	}
	e.sess = updated
	return nil
}

func (c *Controller) renameSession(title string) {
	if c.Mode() != ModeIdle {
		c.notice("/rename requires idle input. Submit it again after the current work ends.")
		return
	}
	c.async(func(ctx context.Context) {
		if ctx.Err() != nil {
			return
		}
		if err := c.eng.RenameCurrent(title); err != nil {
			c.emit(warnBlock("Session was not renamed: %v.", err))
			return
		}
		s, _ := c.eng.inspectionMetadata()
		c.notice("Session %s renamed to %s.", s.ID, s.Title)
	})
}

// Status has its own cancellation generation. It does not change input mode or
// replace a running Turn's cancellation function.
func (c *Controller) cancelInspection() {
	c.mu.Lock()
	if c.statusCancel != nil {
		c.statusCancel()
		c.statusGeneration++
	}
	c.mu.Unlock()
}

func (c *Controller) inspectSession(id string) {
	c.mu.Lock()
	if c.statusCancel != nil {
		c.statusCancel()
	}
	ctx, cancel := context.WithCancel(c.base)
	c.statusCancel = cancel
	c.statusGeneration++
	generation := c.statusGeneration
	c.mu.Unlock()
	current, _ := c.eng.inspectionMetadata()
	if id == "" && current != nil {
		id = current.ID
	}
	c.notice("Loading recorded status for session %s…", id)
	c.goWork(func() {
		defer cancel()
		var report sessionstore.Inspection
		var err error
		if current != nil && id == current.ID && !c.store.Saved(current) {
			report.Metadata = *current
		} else {
			report, err = c.store.Inspect(ctx, c.dir, id)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.mu.Lock()
			if generation == c.statusGeneration && ctx.Err() == nil {
				c.emit(warnBlock("Session %s status unavailable: %v.", id, err))
			}
			c.mu.Unlock()
			return
		}
		text := sessionstore.FormatInspection(report) + "\n" + c.inspectionDestinations(report)
		c.mu.Lock()
		if generation == c.statusGeneration && ctx.Err() == nil {
			owned, lease := c.eng.inspectionMetadata()
			owner := inspectionOwner{Mode: c.mode}
			if owned != nil {
				owner.ID, owner.Owned = owned.ID, lease
			}
			c.notice("%s\n%s", text, inspectionGuidance(report, owner))
		}
		c.mu.Unlock()
	})
}

func inspectionGuidance(report sessionstore.Inspection, owner inspectionOwner) string {
	id := report.Metadata.ID
	prefix := "Session " + id + ": "
	if !owner.Owned || owner.ID != id {
		return prefix + "worker liveness unknown. Reopen this target while idle with /sessions " + id + " and choose it, or exit and run dashi --resume " + id + ". Acquiring its lock and passing recovery checks are required before any action."
	}
	switch owner.Mode {
	case ModeRunning:
		return prefix + "this process is running work. Wait for it to stop, or press Ctrl-C to interrupt it before another action."
	case ModeApproval:
		return prefix + "this process waits for an approval or answer. Answer the displayed dialog, or interrupt it before another action. No permission is inferred."
	case ModeCommand:
		return prefix + "this process is handling a command. Wait for idle input before another action."
	}
	e := report.Latest
	if report.RecordUnavailable || e != nil && !e.Readable {
		return prefix + "latest execution unavailable. Restore or inspect the damaged record before attempting recovery. No continuation is offered."
	}
	if e == nil {
		return prefix + "owned by this process and idle. No recorded request. Send its first request before naming it."
	}
	for _, p := range e.Pending {
		if p.UnsafeEffect {
			return prefix + "verify the exact external effect for call " + p.ID + ". It may still be running. Only after confirming it finished, use /done " + p.ID + "; otherwise /discard discards this Turn without proving the effect ended."
		}
	}
	switch e.Next {
	case dashi.NextNone:
		return prefix + "owned and idle. Send a new request using the next-Turn preferences."
	case dashi.NextRaiseModelCalls:
		return prefix + "owned and idle. Use /continue to raise the model-call limit for this target."
	case dashi.NextContinue:
		return prefix + "owned and idle. Press Enter to continue this target. Recovery and recorded destination checks still apply."
	case dashi.NextRecover:
		if e.ReadyToContinue {
			return prefix + "owned and idle. No run stop was recorded. Press Enter to recover this target after existing checks."
		}
	case dashi.NextFix:
		if e.ReadyToContinue && e.Stop.Kind == dashi.StopFailed {
			return prefix + "owned and idle. Review the recorded failure before pressing Enter to retry this target. Recorded destination checks still apply."
		}
	case dashi.NextResolvePending:
		return prefix + "owned and idle. Pending results or approvals are not ready to continue. Resolve them through existing recovery checks, or /discard this open Turn. Permission is not granted."
	}
	return prefix + "owned and idle. Resolve the recorded cause or verify pending outcomes before recovery. /discard discards this target's open Turn."
}

// Endpoint identity is displayed only when a current definition proves the
// recorded fingerprint. This is pure metadata work, never client construction.
func (c *Controller) inspectionDestinations(report sessionstore.Inspection) string {
	p := c.eng.cfg.Preferences
	if p == nil {
		return "Recorded binding endpoint: unavailable (not stored in public identity)."
	}
	var lines []string
	if e := report.Latest; e != nil && e.Readable {
		endpoint := "unavailable (recorded binding cannot be verified)"
		if d, ok := p.routes.registry[e.Destination.Provider]; ok {
			fingerprint, err := d.fingerprint()
			if err == nil && e.Destination.Revision == cliRouteRevision+fingerprint && e.Destination.API == d.Definition.API {
				endpoint = sessionstore.PrintableEndpoint(d.Definition.Endpoint) + " (matching recorded binding)"
			}
		}
		lines = append(lines, "Recorded binding endpoint: "+endpoint)
	}
	if !report.NextDestinationUnavailable {
		if d, ok := p.routes.registry[report.NextDestination.Provider]; ok {
			lines = append(lines, fmt.Sprintf("Configured saved next-Turn API: %s; endpoint: %s", d.Definition.API, sessionstore.PrintableEndpoint(d.Definition.Endpoint)))
		}
	}
	current, owned := c.eng.inspectionMetadata()
	if owned && current != nil && current.ID == report.Metadata.ID {
		p.mu.RLock()
		selected := p.launch.Selection
		p.mu.RUnlock()
		lines = append(lines, fmt.Sprintf("This invocation's next-Turn selection: %s; API: %s; model: %s; endpoint: %s", selected.Destination.ID, selected.Destination.Definition.API, selected.Model, sessionstore.PrintableEndpoint(selected.Destination.Definition.Endpoint)))
	}
	return workspace.PrintableText(strings.Join(lines, "\n"))
}

func sessionCard(match sessionstore.SearchMatch, owner inspectionOwner) string {
	r := match.Card
	var text strings.Builder
	fmt.Fprintf(&text, "Session %s · %s\nUpdated: %s", r.Metadata.ID, orDefault(r.Metadata.Title, "(no title)"), r.Metadata.Updated.Local().Format("2006-01-02 15:04"))
	if r.Request == nil {
		text.WriteString("\nLast request unavailable.")
	} else {
		fmt.Fprintf(&text, "\nLast request (%s): %s", r.Request.TurnID, r.Request.Text)
	}
	if r.RequestIncomplete {
		text.WriteString("\nNewer request facts may be missing.")
	}
	fmt.Fprintf(&text, "\nStatus: %s", sessionstore.RecordedStopSummary(r.Latest))
	if r.RecordUnavailable || r.OmittedTurns > 0 {
		text.WriteString("\nRecorded facts incomplete.")
	}
	if match.Match != nil {
		fmt.Fprintf(&text, "\nText match (%s · %s): %s", match.Match.TurnID, match.Match.Role, match.Match.Text)
	}
	text.WriteString("\n" + inspectionGuidance(r, owner))
	return workspace.PrintableText(text.String())
}

func (c *Controller) sessions(ctx context.Context, queries ...string) {
	query := ""
	if len(queries) > 0 {
		query = queries[0]
	}
	c.notice("Searching recorded workspace sessions…")
	result, err := c.store.Search(ctx, c.dir, query)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		c.emit(warnBlock("Sessions cannot be searched: %v.", err))
		return
	}
	for _, problem := range result.Problems {
		c.emit(warnBlock("Session %s %s: %s.", problem.SessionID, problem.TurnID, problem.Message))
	}
	if result.TextMatchesMayBeMissing {
		c.notice("Some records are unreadable; text matches may be missing.")
	}
	if result.Truncated {
		c.notice("Showing the first 50 matches. Refine /sessions QUERY to find more.")
	}
	if len(result.Matches) == 0 {
		if result.Sessions == 0 {
			c.notice("There are no saved sessions for this workspace.")
		} else {
			c.notice("No sessions match %q in this workspace.", query)
		}
		return
	}
	current, owned := c.eng.inspectionMetadata()
	owner := inspectionOwner{Owned: owned, Mode: ModeCommand}
	if current != nil {
		owner.ID = current.ID
	}
	options, details := make([]string, len(result.Matches)), make([]string, len(result.Matches))
	for i, match := range result.Matches {
		options[i] = workspace.PrintableText(match.Card.Metadata.ID + "  " + orDefault(match.Card.Metadata.Title, "(no title)"))
		details[i] = sessionCard(match, owner)
	}
	var idx int
	var ok bool
	if chooser, supports := c.ui.(interface {
		ChooseSessions(context.Context, []string, []string) (int, bool)
	}); supports {
		idx, ok = chooser.ChooseSessions(ctx, options, details)
	} else if _, supports := c.ui.(Chooser); supports {
		idx, ok = c.choose(ctx, "Workspace sessions", options)
	} else {
		for _, detail := range details {
			c.notice("%s", detail)
		}
		c.notice("Reopen a listed ID with dashi --resume ID after exiting. Existing locks and recovery checks apply.")
		return
	}
	if !ok || ctx.Err() != nil || idx < 0 || idx >= len(result.Matches) {
		return
	}
	id := result.Matches[idx].Card.Metadata.ID
	if c.keepQueued("/sessions") {
		return
	}
	c.openSession(id)
}
