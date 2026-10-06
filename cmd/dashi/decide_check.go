package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/decide/defaultclient"
	"github.com/deepnoodle-ai/dashi/decide/toolcheck"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/decide"
)

// decideWait bounds how long a command-risk check may take, in every mode.
const decideWait = 3 * time.Second

// decidePauseFor is how long toolcheck pauses a check after an outage. The
// status area counts down from it until the check reports the pause's own
// end time.
const decidePauseFor = time.Minute

// decideRuntime runs the decide features for one process and keeps what
// /settings, the footer, and the status area show. It is nil when no
// feature is on.
type decideRuntime struct {
	settings decideSettings
	// client is the decide client every feature uses.
	client *decide.Client
	risk   toolcheck.CommandRisk
	// wait bounds each check; decideWait outside tests.
	wait time.Duration
	// warn shows one warning to the user.
	warn func(string)
	// outage is told about an outage of the prompt-injection check, so a
	// UI with no status area can say once that results go unchecked.
	outage func(feature string)
	// watchFailed is told about each failed turn-watcher check, with its
	// cause, so a UI with no status area can say once per cause that the
	// watcher could not check.
	watchFailed func(cause string)
	// showRow shows a tool row again, such as one whose result waits for
	// the prompt-injection check.
	showRow func(Block)
	now     func() time.Time

	// rowMu guards rows: the latest row of each running tool call, by
	// block ID, so a result check can show its state on the row.
	rowMu sync.Mutex
	rows  map[string]Block

	// provider is the provider the checks use, such as "typesafe".
	provider string
	// off says why every check is off for this run, such as "no
	// credentials", or is empty. Startup sets it from a setup error.
	off string

	mu     sync.Mutex
	totals decideTotals
	// commands, results, and watch count each feature's checks.
	commands decideCheckCounts
	results  decideCheckCounts
	watch    decideWatchCounts
	// model is the decision model that last answered.
	model string
	// asked reports that a check was attempted.
	asked bool
	// warned is set once a warning about an outage of unknown kind was
	// shown.
	warned bool
	paused map[string]decidePause
}

// decideTotals are this run's decide requests and tokens, for every
// feature together.
type decideTotals struct {
	// Requests counts the checks sent to the provider.
	Requests     int `json:"requests"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// decideCheckCounts are one check's counts this run: Checked counts the
// calls or results the provider answered, Flagged the answers that
// flagged, and Unchecked the ones decide could not answer.
type decideCheckCounts struct {
	Checked   int `json:"checked"`
	Flagged   int `json:"flagged"`
	Unchecked int `json:"unchecked"`
}

// decideWatchCounts are the turn watcher's counts this run: Checked counts
// the checks the provider answered, Reminded the reminders the agent got,
// and Unchecked the checks decide could not answer.
type decideWatchCounts struct {
	Checked   int `json:"checked"`
	Reminded  int `json:"reminded"`
	Unchecked int `json:"unchecked"`
}

// decidePause is a check skipped after an outage until Until.
type decidePause struct {
	Until time.Time
	Cause string
}

// decideSetup is the library's default decide client, which chooses the
// provider from the environment. The CLI reads no decide variable.
type decideSetup struct {
	defaultclient.Setup
	// Err is a setup error, such as no credentials. It turns every check
	// off for the run.
	Err error
}

// loadDecideSetup loads the default decide client when a feature is on,
// before the first turn and without a request. unattended is set for a
// run nobody watches, such as dashi run. A setup error turns every check
// off; with approveAll and command_risk on in an unattended run, it stops
// the launch instead, since nobody would read a warning and the user asked
// for the guard. load is defaultclient.Load outside tests.
func loadDecideSetup(s decideSettings, unattended, approveAll bool, load func() (defaultclient.Setup, error)) (decideSetup, error) {
	if !s.any() {
		return decideSetup{}, nil
	}
	setup, err := load()
	if err == nil {
		return decideSetup{Setup: setup}, nil
	}
	if unattended && approveAll && s.enabled(decideCommandRisk) {
		return decideSetup{}, fmt.Errorf("decide command_risk is on, but its checks cannot run: %s. "+
			"Set the variable, or turn the check off with --decide off", setupErrorText(err))
	}
	return decideSetup{Setup: setup, Err: err}, nil
}

// warning is the one warning a setup error gives, or empty.
func (s decideSetup) warning() string {
	if s.Err == nil {
		return ""
	}
	return "decide checks are off for this run: " + setupErrorText(s.Err) + "."
}

// setupErrorText is a setup error as a person reads it, without the
// library's "decide: " prefix.
func setupErrorText(err error) string {
	return workspace.PrintableText(strings.TrimPrefix(err.Error(), "decide: "))
}

// offReason names a setup error in a few words, for the status area.
func offReason(s decideSetup) string {
	switch {
	case s.Err == nil:
		return ""
	case errors.Is(s.Err, decide.ErrNoAPIKey):
		return "no credentials"
	case errors.Is(s.Err, defaultclient.ErrUnknownProvider):
		return fmt.Sprintf("unknown DECIDE_PROVIDER %q", workspace.PrintableText(s.Provider))
	}
	return "not set up"
}

// newDecideRuntime returns the runtime for s, or nil when no built feature
// is on, so that nothing changes with every feature off. With a setup
// error, every check is off, and only /settings and the status area say
// so.
func newDecideRuntime(s decideSettings, setup decideSetup) *decideRuntime {
	if !s.any() {
		return nil
	}
	rt := &decideRuntime{
		settings: s, wait: decideWait, now: time.Now, paused: map[string]decidePause{},
		provider: setup.Provider, off: offReason(setup), client: setup.Client,
	}
	rt.risk = toolcheck.CommandRisk{Client: setup.Client, Timeout: decideWait}
	return rt
}

// runs reports whether feature is on and its checks can run.
func (rt *decideRuntime) runs(feature string) bool {
	return rt != nil && rt.off == "" && rt.settings.enabled(feature)
}

// checksCommands reports whether the command-risk check is on and can run.
func (rt *decideRuntime) checksCommands() bool {
	return rt.runs(decideCommandRisk)
}

// decideResult is what the command-risk check found for one call.
type decideResult struct {
	// Checked is true when the provider answered.
	Checked bool
	Flagged bool
	Severe  float64
	Model   string
	Request string
	// Cause says why the call is unchecked, such as "timeout".
	Cause string
	// Outage is true when Cause is an outage, which no command text can
	// cause; any other unchecked call is suspect.
	Outage bool
	// AnsweredFirst is true when the person answered before the check did.
	AnsweredFirst bool
}

// suspect reports a call decide could not answer for a reason the command
// itself can cause: a timeout, input too large, or a rejected request.
func (r decideResult) suspect() bool {
	return !r.Checked && !r.Outage && !r.AnsweredFirst
}

// holds reports whether the call must be asked about even in bypass mode.
func (r decideResult) holds() bool {
	return r.Flagged || r.suspect()
}

// percent formats p rounded down, as toolcheck does, so a value just under
// the threshold never reads as the threshold.
func percent(p float64) string {
	return fmt.Sprintf("%d%%", int(math.Floor(p*100+1e-9)))
}

// recordLine is the decide answer as the turn record keeps it, after
// "decide: " in the approval decision's Reason. renderDecide parses it.
func (r decideResult) recordLine() string {
	switch {
	case r.AnsweredFirst:
		return "not checked (answered first)"
	case !r.Checked:
		return "unchecked (" + r.Cause + ")"
	}
	flag := ""
	if r.Flagged {
		flag = " flagged"
	}
	return fmt.Sprintf("severe %s%s (%s, request %s)", percent(r.Severe), flag,
		workspace.PrintableText(r.Model), workspace.PrintableText(r.Request))
}

// rulesVersion names the rules that answered, for the approval decision.
func (r decideResult) rulesVersion() string {
	if r.Checked {
		return decideRulesPrefix + workspace.PrintableText(r.Model)
	}
	return decideRulesPrefix + "unchecked"
}

// screenLine is the answer as a prompt or tool row shows it.
func (r decideResult) screenLine() string {
	switch {
	case r.AnsweredFirst:
		return "decide: not checked (answered first)"
	case !r.Checked:
		return "decide: unchecked (" + r.Cause + ")"
	case r.Flagged:
		return "decide: severe " + percent(r.Severe) + " — flagged"
	}
	// Below the threshold, the line still ends in a verdict, so a person
	// who does not know the threshold reads it as an answer.
	return "decide: severe " + percent(r.Severe) + " — ok"
}

// check runs the command-risk check on plan within rt.wait, and records
// the result. A cancel of ctx means the person answered first.
func (rt *decideRuntime) check(ctx context.Context, plan dashi.ToolPlan) decideResult {
	rctx, cancel := context.WithTimeout(ctx, rt.wait)
	defer cancel()
	v, err := rt.risk.Check(rctx, plan)
	canceled := ctx.Err() != nil && errors.Is(err, context.Canceled)
	r, warning := rt.record(decideCommandRisk, &rt.commands, canceled, v, err)
	if warning != "" && rt.warn != nil {
		rt.warn(warning)
	}
	return r
}

// record turns a toolcheck verdict or error for feature into a result and
// updates counts, the totals, the provider state, and the pauses. canceled
// is set when the check stopped because nobody needs it any more, such as
// a person who answered first. warning is a warning to show once.
func (rt *decideRuntime) record(feature string, counts *decideCheckCounts, canceled bool, v toolcheck.Verdict, err error) (r decideResult, warning string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.asked = true
	if err == nil {
		rt.totals.Requests++
		rt.totals.InputTokens += v.Usage.InputTokens
		rt.totals.OutputTokens += v.Usage.OutputTokens
		counts.Checked++
		if v.Flagged {
			counts.Flagged++
		}
		rt.model = v.Model
		delete(rt.paused, feature)
		return decideResult{
			Checked: true, Flagged: v.Flagged, Severe: v.Probabilities[toolcheck.QuestionSevere],
			Model: v.Model, Request: v.RequestID,
		}, ""
	}
	if canceled {
		rt.totals.Requests++
		return decideResult{AnsweredFirst: true}, ""
	}
	r = decideResult{Cause: decideCause(err), Outage: toolcheck.IsOutage(err)}
	counts.Unchecked++
	paused, isPause := errors.AsType[*toolcheck.PausedError](err)
	if !isPause {
		rt.totals.Requests++
	}
	switch {
	case isPause:
		rt.paused[feature] = decidePause{Until: paused.Until, Cause: r.Cause}
	case r.Outage:
		rt.paused[feature] = decidePause{Until: rt.now().Add(decidePauseFor), Cause: r.Cause}
		if r.Cause == "error" && !rt.warned {
			// An outage with no known kind: say what it was, once.
			rt.warned = true
			warning = "decide: " + errorText(err)
		}
	}
	return r, warning
}

// errorText is the text of err without toolcheck's wrapping prefixes, so a
// warning reads as the provider's own message.
func errorText(err error) string {
	text := err.Error()
	if p, ok := errors.AsType[*toolcheck.PausedError](err); ok && p.Cause != nil {
		text = p.Cause.Error()
	}
	if i := strings.LastIndex(text, "decide: "); i >= 0 {
		text = text[i+len("decide: "):]
	}
	return workspace.PrintableText(text)
}

// decideCause names why decide could not answer, in a few words.
func decideCause(err error) string {
	return string(toolcheck.CauseOf(err))
}

// status is the status-area line: empty while every check answers.
func (rt *decideRuntime) status() string {
	if rt == nil {
		return ""
	}
	if rt.off != "" {
		return "decide: off (" + rt.off + ")"
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	now := rt.now()
	var parts []string
	for _, f := range decideFeatureNames {
		p, ok := rt.paused[f]
		if !ok {
			continue
		}
		left := p.Until.Sub(now)
		if left <= 0 {
			delete(rt.paused, f)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s paused (%s), resumes in %ds", f, p.Cause, int(math.Ceil(left.Seconds()))))
	}
	if len(parts) == 0 {
		return ""
	}
	return "decide: " + strings.Join(parts, "; ")
}

// pausedUntil returns when the pause of feature ends, if one is on.
func (rt *decideRuntime) pausedUntil(feature string) (time.Time, string, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	p, ok := rt.paused[feature]
	return p.Until, p.Cause, ok
}

// providerState is the provider line of /settings. It names the provider,
// because that is where the commands go.
func (rt *decideRuntime) providerState() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	provider := workspace.PrintableText(rt.provider)
	switch {
	case rt.off != "":
		return "off: " + rt.off
	case rt.model != "":
		return "ready: " + provider + " " + workspace.PrintableText(rt.model)
	case rt.asked:
		return provider + ", no answer yet"
	}
	return provider + ", not used yet"
}

// pendingCheck is a command-risk check that runs while a prompt shows.
type pendingCheck struct {
	done   chan struct{}
	cancel context.CancelFunc
	result decideResult
}

// startCheck runs the check in the background.
func (rt *decideRuntime) startCheck(ctx context.Context, plan dashi.ToolPlan) *pendingCheck {
	ctx, cancel := context.WithCancel(ctx)
	p := &pendingCheck{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(p.done)
		p.result = rt.check(ctx, plan)
	}()
	return p
}

// doneCheck is a check whose result is known before the prompt shows.
func doneCheck(r decideResult) *pendingCheck {
	p := &pendingCheck{done: make(chan struct{}), cancel: func() {}, result: r}
	close(p.done)
	return p
}

// Result returns the result once the check is done.
func (p *pendingCheck) Result() (decideResult, bool) {
	if p == nil {
		return decideResult{}, false
	}
	select {
	case <-p.done:
		return p.result, true
	default:
		return decideResult{}, false
	}
}

// finish stops a check the person answered before, and returns the result:
// the answer if it came first, or "answered first".
func (p *pendingCheck) finish() decideResult {
	p.cancel()
	<-p.done
	return p.result
}

// flagged reports whether the check is done and flagged the command.
func (p *pendingCheck) flagged() bool {
	r, ok := p.Result()
	return ok && r.Flagged
}

// decideRulesPrefix starts the RulesVersion of a checked approval. The
// renderer finds checked calls by it.
const decideRulesPrefix = "decide/"

// decideReasonSep joins the decide line to the policy's own reason.
const decideReasonSep = "; decide: "

// withDecide appends the decide line to an approval decision's reason. A
// denial keeps its own text first, so the model reads why it was refused.
func withDecide(reason, line string) string {
	if reason == "" {
		return "decide: " + line
	}
	return reason + decideReasonSep + line
}

// splitDecide splits a recorded reason into the policy's reason and the
// decide line. ok is false when the call was not checked.
func splitDecide(v dashi.ToolVerdict) (reason, line string, ok bool) {
	if !strings.HasPrefix(v.RulesVersion, decideRulesPrefix) {
		return v.Reason, "", false
	}
	if rest, found := strings.CutPrefix(v.Reason, "decide: "); found {
		return "", rest, true
	}
	i := strings.LastIndex(v.Reason, decideReasonSep)
	if i < 0 {
		return v.Reason, "", false
	}
	return v.Reason[:i], v.Reason[i+len(decideReasonSep):], true
}

// What a decide mark on a tool row checked: the shell command before it
// ran, or the result before the model read it.
const (
	decideCheckCommand = "command"
	decideCheckResult  = "result"
)

// decideMark is one decide answer on a tool row: a tick in the row's head,
// "decide ✓ command", or a line under it, "decide command: severe 90% —
// flagged". A row can carry a command mark and a result mark.
type decideMark struct {
	// Check is decideCheckCommand or decideCheckResult.
	Check string
	// OK is true when decide checked and did not flag, and the agent got
	// nothing to read.
	OK      bool
	Flagged bool
	// Text is the answer, such as "severe 52% — ok".
	Text string
}

// tick reports whether a row in state shows the mark as a tick. A denied
// row shows the answer as a line instead, so a ✓ never sits next to the
// person's denial.
func (m *decideMark) tick(state string) bool {
	return m != nil && m.OK && state != StateDenied
}

// line is the line a row in state shows under it, or empty.
func (m *decideMark) line(state string) string {
	if m == nil || m.tick(state) {
		return ""
	}
	return "decide " + m.Check + ": " + m.Text
}

// decideTicks is the tick of a row's passed checks, such as "decide ✓
// command, result", or empty.
func decideTicks(b Block) string {
	var checks []string
	for _, m := range []*decideMark{b.Decide, b.DecideResult} {
		if m.tick(b.State) {
			checks = append(checks, m.Check)
		}
	}
	if len(checks) == 0 {
		return ""
	}
	return "decide ✓ " + strings.Join(checks, ", ")
}

// decideLines are the lines under a row: each check that did not pass.
func decideLines(b Block) []decideMark {
	var lines []decideMark
	for _, m := range []*decideMark{b.Decide, b.DecideResult} {
		if text := m.line(b.State); text != "" {
			lines = append(lines, decideMark{Check: m.Check, Flagged: m.Flagged, Text: text})
		}
	}
	return lines
}

// markFromRecord reads the decide mark of a call from its recorded
// verdict, so a reopened session shows what the live run showed.
func markFromRecord(v dashi.ToolVerdict) *decideMark {
	_, line, ok := splitDecide(v)
	if !ok {
		return nil
	}
	m := &decideMark{Check: decideCheckCommand, Text: line}
	if rest, checked := strings.CutPrefix(line, "severe "); checked {
		answer, _, _ := strings.Cut(rest, " (")
		if p, flagged := strings.CutSuffix(answer, " flagged"); flagged {
			m.Flagged, m.Text = true, "severe "+p+" — flagged"
		} else {
			m.OK, m.Text = true, "severe "+answer+" — ok"
		}
	}
	return m
}

// decideCounts counts the checked, flagged, and unchecked calls of a turn
// from its record.
func decideCounts(v dashi.TurnView) (checked, flagged, unchecked int) {
	ids := make([]string, 0, len(v.Tools))
	for id := range v.Tools {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		m := markFromRecord(v.Tools[id].Verdict)
		switch {
		case m == nil:
		case m.OK:
			checked++
		case m.Flagged:
			checked++
			flagged++
		case strings.HasPrefix(m.Text, "unchecked"):
			unchecked++
		}
	}
	return checked, flagged, unchecked
}

// decideSlot carries one BeforeTool call's plan to the approval prompt,
// and the prompt's decide result back to the decision.
type decideSlot struct {
	plan   dashi.ToolPlan
	mu     sync.Mutex
	result *decideResult
}

type decideSlotKey struct{}

func (s *decideSlot) set(r decideResult) {
	s.mu.Lock()
	s.result = &r
	s.mu.Unlock()
}

func (s *decideSlot) take() (decideResult, bool) {
	if s == nil {
		return decideResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.result == nil {
		return decideResult{}, false
	}
	return *s.result, true
}

// decideSlotFrom returns the slot of the BeforeTool call that asks.
func decideSlotFrom(ctx context.Context) *decideSlot {
	s, _ := ctx.Value(decideSlotKey{}).(*decideSlot)
	return s
}

// checkedCommand returns the slot to check when r is the workspace's own
// bash tool and the command-risk check is on. The policy fills Command only
// for its own bash, so an MCP tool named bash is never checked as a shell.
func (rt *decideRuntime) checkedCommand(ctx context.Context, r ApprovalRequest) *decideSlot {
	if !rt.checksCommands() || r.Tool != workspace.ToolBash || r.Command == "" {
		return nil
	}
	slot := decideSlotFrom(ctx)
	if slot == nil || slot.plan.Call.Name != workspace.ToolBash {
		return nil
	}
	return slot
}

// decideInspection is the decide section of /settings.
type decideInspection struct {
	Features map[string]decideFeatureState `json:"features"`
	// CommandRiskScope says what the command-risk check reads.
	CommandRiskScope string `json:"command_risk_scope"`
	// BypassMode is set with --dangerously-skip-permissions or run --yes,
	// and Unchecked then says what runs without a prompt or a check.
	BypassMode bool   `json:"bypass_mode"`
	Unchecked  string `json:"bypass_unchecked,omitempty"`
	Provider   string `json:"provider"`
	decideTotals
	// Counts holds this run's counts of each feature that is on.
	Counts decideFeatureCounts `json:"counts"`
}

// decideFeatureCounts holds the counts of the features that are on.
type decideFeatureCounts struct {
	CommandRisk     *decideCheckCounts `json:"command_risk,omitempty"`
	PromptInjection *decideCheckCounts `json:"prompt_injection,omitempty"`
	TurnWatcher     *decideWatchCounts `json:"turn_watcher,omitempty"`
}

// decideFeatureState is one feature in /settings.
type decideFeatureState struct {
	On bool `json:"on"`
	// Requested is set when the user named the feature, even one this
	// version does not run.
	Requested bool `json:"requested"`
	// Source is what turned it on, or "default" when off by default.
	Source string `json:"source"`
	// Available is false for a feature this version does not run.
	Available bool `json:"available"`
}

// decideScopes says what each feature reads, for /settings.
var decideScopes = map[string]string{
	decideCommandRisk:     "shell commands only",
	decidePromptInjection: "MCP results, and gh, curl, wget, http output",
	decideTurnWatcher:     "a turn's repeated calls",
}

// inspectDecide builds the decide section of /settings.
func (c *Controller) inspectDecide() *decideInspection {
	s := c.decideOn
	view := &decideInspection{
		Features:         map[string]decideFeatureState{},
		CommandRiskScope: decideScopes[decideCommandRisk],
		BypassMode:       c.approveAll,
		Provider:         "not used yet",
	}
	if c.approveAll {
		view.Unchecked = "write_file, edit_file, and MCP calls run without a prompt or a check"
		if s.enabled(decideCommandRisk) {
			view.Unchecked += "; while a check is paused, shell commands run unchecked too"
		}
	}
	for _, f := range decideFeatureNames {
		state := decideFeatureState{
			On:        s.enabled(f),
			Requested: slices.Contains(s.On, f),
			Source:    "default",
			Available: slices.Contains(decideFeaturesBuilt, f),
		}
		if s.Source != "" {
			state.Source = s.Source
		}
		view.Features[f] = state
	}
	if rt := c.decide; rt != nil {
		view.Provider = rt.providerState()
		rt.mu.Lock()
		view.decideTotals = rt.totals
		if s.enabled(decideCommandRisk) {
			counts := rt.commands
			view.Counts.CommandRisk = &counts
		}
		if s.enabled(decidePromptInjection) {
			counts := rt.results
			view.Counts.PromptInjection = &counts
		}
		if s.enabled(decideTurnWatcher) {
			counts := rt.watch
			view.Counts.TurnWatcher = &counts
		}
		rt.mu.Unlock()
	}
	return view
}

// writeDecideInspection writes the decide section of /settings as text.
func writeDecideInspection(text *strings.Builder, view decideInspection) {
	text.WriteString("\ndecide:")
	for _, f := range decideFeatureNames {
		state := view.Features[f]
		value := "off"
		switch {
		case state.On:
			value = "on (" + workspace.PrintableText(state.Source) + ")"
		case state.Requested && !state.Available:
			value = "off (not available in this version)"
		case state.Source != "default":
			value = "off (" + workspace.PrintableText(state.Source) + ")"
		}
		fmt.Fprintf(text, "\n  %s: %s", f, value)
		if scope := decideScopes[f]; scope != "" && (state.On || f == decideCommandRisk) {
			fmt.Fprintf(text, "; %s", scope)
		}
	}
	if view.Unchecked != "" {
		fmt.Fprintf(text, "\n  bypass mode: %s", view.Unchecked)
	}
	fmt.Fprintf(text, "\n  provider: %s", view.Provider)
	t := view.decideTotals
	fmt.Fprintf(text, "\n  this run: %d %s, %s input and %s output tokens",
		t.Requests, plural(t.Requests, "request"), groupDigits(t.InputTokens), groupDigits(t.OutputTokens))
	if c := view.Counts.CommandRisk; c != nil {
		fmt.Fprintf(text, "\n    command_risk: %d checked, %d flagged, %d unchecked", c.Checked, c.Flagged, c.Unchecked)
	}
	if c := view.Counts.PromptInjection; c != nil {
		fmt.Fprintf(text, "\n    prompt_injection: %d checked, %d flagged, %d unchecked", c.Checked, c.Flagged, c.Unchecked)
	}
	if c := view.Counts.TurnWatcher; c != nil {
		fmt.Fprintf(text, "\n    turn_watcher: %d checked, %d %s, %d unchecked",
			c.Checked, c.Reminded, plural(c.Reminded, "reminder"), c.Unchecked)
	}
}
