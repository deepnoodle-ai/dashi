// Package toolcheck asks a decision model, through Decide, for a second
// opinion on tool calls.
//
// CommandRisk checks a shell command before it runs. When the command
// could likely cause severe harm that is hard to undo, such as rm -rf ~ or
// a force push to main, its hook holds the call for approval. Routine
// commands run without a hold.
//
// PromptInjection checks a tool result before the model reads it. When the
// result likely tries to take over the agent, or hides text from a person,
// its hook puts a warning before the result: treat it as untrusted data,
// and tell the user what it tried to get you to do.
//
// Both are advice, not a sandbox. They never approve a call; only a person
// or the host's own policy can. When Decide is not set up, such as with no
// key or a failed login, a check pauses for a minute and the call runs as
// if the check were not there: the command check's hook gives Dashi a
// hook_failed warning, and the content check's hook records the result as
// unchecked. The content check also pauses on a Decide server or
// connection error. When the command check fails for any other reason,
// such as a server error or a timeout, its hook holds the call for
// approval. A host that asks the user in its own BeforeTool hook calls
// Check from there, so the user sees one prompt.
//
// The checked text goes to the decision model's provider, with the key of
// the Decide client.
package toolcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/decide/internal/client"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/decide"
)

// defaultShellTools is what a nil ShellTools means.
var defaultShellTools = []string{workspace.ToolBash}

// Hook names, as the turn's hook results record them.
const (
	CommandRiskName     = "toolcheck-command-risk"
	PromptInjectionName = "toolcheck-prompt-injection"
)

// Verdict is what one check found for one call. Checked is false when the
// check did not read the call or could not answer. Requests and Usage are
// set even when the check failed.
type Verdict struct {
	// Call is the tool call checked: its ID, Name, and Args.
	Call    tool.Call
	Checked bool
	// Flagged is true when a probability reached the check's
	// MinProbability.
	Flagged bool
	// Probabilities holds P(yes) by question: QuestionSevere for
	// CommandRisk, QuestionInjection and QuestionHidden for
	// PromptInjection.
	Probabilities map[string]float64
	// Model is the resolved decision model version, such as "jev-1.13.0".
	Model     string
	RequestID string
	// Requests is how many requests the check sent to Decide, not counting
	// automatic retries.
	Requests int
	// Usage holds the tokens of every answer Decide gave, even when another
	// part of a long text failed.
	Usage decide.Usage
	// Partial is true when a result was too long to check whole: the check
	// read its first and last parts and skipped the middle. The
	// PromptInjection hook then tells the model to treat the whole result
	// as untrusted. CommandRisk never checks part of a command.
	Partial bool
	// Reason is one line for a hook decision, such as
	// "severe 94% (jev-1.13.0, request r_123)".
	Reason string
}

// ErrTooLarge is returned by CommandRisk for a command too long to check
// whole: more than 8 parts of 60 KiB, about 480 KB. Nothing was sent, and
// the check does not pause. PromptInjection never returns it; it checks
// the start and end of a long result instead.
var ErrTooLarge = errors.New("toolcheck: input too large to check")

// PausedError is returned when a check pauses, and while it stays paused:
// for a minute the check sends nothing and the call runs unchecked. A check
// pauses when Decide is not set up, such as no key or a failed login. The
// content check also pauses when Decide cannot be reached. PausedError
// unwraps to the error that started the pause.
type PausedError struct {
	// Until is when the check asks Decide again.
	Until time.Time
	// Cause is the error that started the pause.
	Cause error
}

func (e *PausedError) Error() string {
	return fmt.Sprintf("toolcheck: paused until %s after: %v",
		e.Until.Format(time.TimeOnly), e.Cause)
}

func (e *PausedError) Unwrap() error { return e.Cause }

// errNoClient marks a failure to make a Decide client, such as a missing
// key, which no input can cause.
var errNoClient = errors.New("toolcheck: no decide client")

// IsPaused reports whether err is a *PausedError: the check is paused and
// the call ran unchecked. Any other error may come from the input, which
// an attacker can shape. A host can ask the user about the call in that
// case.
func IsPaused(err error) bool {
	_, ok := errors.AsType[*PausedError](err)
	return ok
}

// notSetUp reports whether err means Decide is not set up: no client, no
// key, or a failed login. No input can cause these. Both checks pause on
// these.
func notSetUp(err error) bool {
	return errors.Is(err, errNoClient) ||
		errors.Is(err, decide.ErrNoAPIKey) ||
		errors.Is(err, decide.ErrAuth)
}

// contentPauses reports whether the content check pauses on err: Decide
// is not set up, or it could not be reached (a server error, or a network
// error that is not a timeout). A timeout, a rate limit, or
// a rejected request does not count, since the input can cause them: a
// long result is checked in several parts, so it sets how many requests
// the check sends.
func contentPauses(err error) bool {
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled),
		errors.Is(err, decide.ErrRateLimited):
		return false
	case notSetUp(err), errors.Is(err, decide.ErrServer):
		return true
	}
	if _, ok := errors.AsType[*decide.APIError](err); ok {
		return false // another 4xx: the request itself was refused
	}
	switch {
	case errors.Is(err, decide.ErrValidation),
		errors.Is(err, decide.ErrInvalidRequest),
		errors.Is(err, decide.ErrInvalidAnswer),
		errors.Is(err, decide.ErrDecode):
		return false
	}
	if ne, ok := errors.AsType[net.Error](err); ok {
		return !ne.Timeout()
	}
	return false
}

// pauseFor is how long a check stays paused.
const pauseFor = time.Minute

// now is the clock; tests replace it.
var now = time.Now

// pauseKey names one check kind on one client. A pause is
// process-wide, so every config and turn sharing a client shares a pause.
// Each kind pauses on its own, so a failing content check never pauses the
// command check.
type pauseKey struct {
	client *decide.Client
	kind   string
}

var pauses = struct {
	sync.Mutex
	m map[pauseKey]*PausedError
}{m: map[pauseKey]*PausedError{}}

// paused returns the pause in effect for key, or nil.
func paused(key pauseKey) *PausedError {
	pauses.Lock()
	defer pauses.Unlock()
	p := pauses.m[key]
	if p == nil {
		return nil
	}
	if !now().Before(p.Until) {
		delete(pauses.m, key)
		return nil
	}
	return p
}

// pause starts a pause for key and returns it. It first drops every pause
// that has ended, so a host that makes a client per tenant or per request
// does not keep old clients in the table.
func pause(key pauseKey, cause error) *PausedError {
	pauses.Lock()
	defer pauses.Unlock()
	t := now()
	for k, p := range pauses.m {
		if !t.Before(p.Until) {
			delete(pauses.m, k)
		}
	}
	pauses.m[key] = &PausedError{Until: t.Add(pauseFor), Cause: cause}
	return pauses.m[key]
}

// Limits on the text one check sends.
const (
	// headParts and tailParts are how many parts of a long text one check
	// sends: the first headParts and the last tailParts. They bound the
	// paid requests one hostile page can cause.
	headParts = 4
	tailParts = 4
	// partBytes is the most text, as encoded in JSON, in one request. It
	// keeps a part and its questions within the decision model's limit of
	// 32,000 tokens, as decide's own command does with 64 KiB items.
	partBytes = 60 << 10
	// parallelParts is how many parts are asked at once.
	parallelParts = 4
)

// asked is what Decide answered for one text: P(yes) by question key.
type asked struct {
	probabilities map[string]float64
	model         string
	requestID     string
	usage         decide.Usage
	requests      int
	// partial is true when the middle of the text was not sent.
	partial bool
}

// ask sends text to Decide with the questions, in parts when it is long,
// bounded by timeout and the caller's deadline. field names the text in
// the state the decision model reads. A text too long to send whole is
// sent as its start and end, or, when whole is set, returns ErrTooLarge.
// When pauseOn(err) is true for a failure, ask starts a pause for kind
// and returns the *PausedError; no other failure starts a pause.
func ask(
	ctx context.Context,
	given *decide.Client,
	kind string,
	timeout time.Duration,
	field, text string,
	questions []question,
	whole bool,
	pauseOn func(error) bool,
) (asked, error) {
	// A canceled caller gets its own error, never a pause.
	if err := ctx.Err(); err != nil {
		return asked{}, err
	}
	parts, partial := pick(text, partBytes)
	if partial && whole {
		return asked{}, ErrTooLarge
	}
	c, err := client.Default(given)
	key := pauseKey{client: c, kind: kind}
	if p := paused(key); p != nil {
		return asked{}, p
	}
	if err != nil {
		return asked{}, pause(key, fmt.Errorf("%w: %w", errNoClient, err))
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	a, err := askParts(rctx, c, field, parts, questions)
	if err != nil {
		err = fmt.Errorf("toolcheck: %s: %w", kind, err)
		// The caller's deadline or cancel never starts a pause.
		if ctx.Err() == nil && pauseOn(err) {
			return a, pause(key, err)
		}
		return a, err
	}
	a.partial = partial
	return a, nil
}

// askParts asks about each part, a few at once, and combines the answers:
// each question takes the highest P(yes) of any part, so a text is flagged
// when any part is. The model and request are the part with the highest
// P(yes); the usage and requests are the sums. The first error, in part
// order, wins.
func askParts(
	ctx context.Context,
	c *decide.Client,
	field string,
	parts []string,
	questions []question,
) (asked, error) {
	results := make([]asked, len(parts))
	errs := make([]error, len(parts))
	sem := make(chan struct{}, parallelParts)
	var wg sync.WaitGroup
	for i, part := range parts {
		state := map[string]string{field: part}
		if len(parts) > 1 {
			state["part"] = fmt.Sprintf("%d of %d", i+1, len(parts))
		}
		wg.Go(func() {
			select {
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()
			results[i], errs[i] = askOne(ctx, c, state, questions)
		})
	}
	wg.Wait()
	out := asked{probabilities: map[string]float64{}}
	best := -1.0
	for _, r := range results {
		top := 0.0
		for _, q := range questions {
			p := r.probabilities[q.key]
			out.probabilities[q.key] = max(out.probabilities[q.key], p)
			top = max(top, p)
		}
		if top > best {
			best, out.model, out.requestID = top, r.model, r.requestID
		}
		out.usage.InputTokens += r.usage.InputTokens
		out.usage.OutputTokens += r.usage.OutputTokens
		out.requests += r.requests
	}
	return out, firstError(errs)
}

func firstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// askOne sends one request. Every question must have a valid answer: a
// missing, mistyped, or out-of-range answer is an error that wraps
// decide.ErrInvalidAnswer, never a passed check.
func askOne(
	ctx context.Context,
	c *decide.Client,
	state map[string]string,
	questions []question,
) (asked, error) {
	if err := ctx.Err(); err != nil {
		return asked{}, err
	}
	req := decide.NewRequest(state)
	for _, q := range questions {
		decide.Ask(req, q.key, q.q)
	}
	resp, err := c.SystemOne(ctx, req)
	out := asked{requests: 1}
	if resp != nil {
		out.usage = resp.Usage
	}
	if err != nil {
		return out, err
	}
	out.probabilities = make(map[string]float64, len(questions))
	out.model, out.requestID = resp.Model, resp.RequestID
	for _, q := range questions {
		a, ok := resp.Answers[q.key].(*decide.NoulAnswer)
		if !ok || a == nil || math.IsNaN(a.Noul) || a.Noul < 0 || a.Noul > 1 {
			return out, fmt.Errorf("%w: no valid %s answer (request %s)",
				decide.ErrInvalidAnswer, q.key, resp.RequestID)
		}
		out.probabilities[q.key] = a.Noul
	}
	return out, nil
}

// pick returns the parts of text that one check sends, each at most limit
// bytes as encoded in JSON. A text of headParts+tailParts parts or fewer
// is sent whole. A longer one sends its first headParts and last tailParts
// parts, and partial is true. It never cuts more of the text than it
// sends, so a huge text costs little.
func pick(text string, limit int) (parts []string, partial bool) {
	head, rest := splitN(text, limit, headParts+tailParts)
	if rest == "" {
		return head, false
	}
	head = head[:headParts]
	mid := 0
	for _, p := range head {
		mid += len(p)
	}
	// A part holds at most limit bytes, and JSON never encodes a character
	// in fewer bytes than UTF-8, so the last tailParts*limit bytes hold at
	// least the last tailParts parts.
	start := max(mid, len(text)-tailParts*limit)
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	tail := split(text[start:], limit)
	if len(tail) > tailParts {
		tail = tail[len(tail)-tailParts:]
	}
	return append(head, tail...), true
}

// split cuts text into parts whose JSON encoding is at most limit bytes,
// on whole characters, preferring to cut after a newline in the second
// half of a part.
func split(text string, limit int) []string {
	parts, _ := splitN(text, limit, -1)
	return parts
}

// splitN is split that stops after n parts, when n is positive, and
// returns the text it did not cut.
func splitN(text string, limit, n int) ([]string, string) {
	var parts []string
	for text != "" && len(parts) != n {
		size, cut, lastBreak := 0, len(text), -1
		for i, r := range text {
			n := jsonLen(r)
			if size+n > limit {
				cut = i
				break
			}
			size += n
			if r == '\n' {
				lastBreak = i + 1
			}
		}
		if cut < len(text) && lastBreak > cut/2 {
			cut = lastBreak
		}
		parts = append(parts, text[:cut])
		text = text[cut:]
	}
	if len(parts) == 0 {
		return []string{""}, ""
	}
	return parts, text
}

// jsonLen is how many bytes encoding/json writes for r inside a string.
func jsonLen(r rune) int {
	switch {
	case r == '"', r == '\\', r == '\n', r == '\r', r == '\t':
		return 2
	case r < 0x20, r == '<', r == '>', r == '&', r == ' ', r == ' ',
		r == utf8.RuneError:
		return 6
	}
	return utf8.RuneLen(r)
}

// percent formats p as a whole percentage, rounded down, so a value just
// under a threshold never reads as the threshold.
func percent(p float64) string {
	return fmt.Sprintf("%d%%", int(math.Floor(p*100+1e-9)))
}

// partialWords marks, in a Reason, a text checked at its start and end only.
const partialWords = "start and end only"

// reason is the one-line Reason: each probability, then the model and
// request.
func reason(a asked, questions []question) string {
	var b strings.Builder
	for i, q := range questions {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(q.key + " " + percent(a.probabilities[q.key]))
	}
	if a.partial {
		b.WriteString(", " + partialWords)
	}
	fmt.Fprintf(&b, " (%s, request %s)", a.model, a.requestID)
	return b.String()
}

// flagged reports whether any probability reaches min. A negative min
// never flags.
func flagged(a asked, questions []question, min float64) bool {
	if min < 0 {
		return false
	}
	for _, q := range questions {
		if a.probabilities[q.key] >= min {
			return true
		}
	}
	return false
}

// shellCommand returns the command a call to one of shells runs, or false
// when the call is not to a shell tool or has no command.
func shellCommand(call tool.Call, shells []string) (string, bool) {
	if shells == nil {
		shells = defaultShellTools
	}
	if !slices.Contains(shells, call.Name) {
		return "", false
	}
	// A map matches keys exactly, as a shell executor does; a struct would
	// also take "Command", and a later case alias would hide the command
	// that runs. A repeated key keeps its last value, as most decoders do.
	var args map[string]any
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return "", false
	}
	command, ok := args["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return "", false
	}
	return command, true
}
