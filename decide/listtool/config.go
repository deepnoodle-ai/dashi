package listtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/decide/internal/client"
	"github.com/deepnoodle-ai/dashi/decide/internal/state"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
)

// Config configures both tools. Its zero value works once Lists holds at
// least one list and TYPESAFE_API_KEY is set.
type Config struct {
	// Client sends the requests. Nil means decide.NewClient() from the
	// environment. Pin the decision model with decide.WithModel, because
	// an alias such as jev-latest can move.
	Client *decide.Client
	// Lists are the lists the tools can read: at least one, with unique
	// names. Both tools take the same lists.
	Lists []AnyList

	// MaxQuestions is the most questions one judge_items call may ask.
	// Zero means 8. It is at most 64; a larger or negative value means 64.
	MaxQuestions int
	// MaxRequests is the most Decide requests one judge_items call may
	// send, one per item. Zero means 500. Negative means no limit.
	MaxRequests int
	// MaxTurnRequests is the most Decide requests the judge_items calls of
	// one turn may send together. Zero means 2,000. Negative means no
	// limit.
	MaxTurnRequests int
	// MaxItemBytes is how much of an item's Text judge_items sends.
	// The rest of a longer item is not judged, and the result counts the
	// items cut. Zero means 64 KiB. Negative means no limit.
	MaxItemBytes int
	// Concurrency is how many requests one judge_items call sends at
	// once. Zero or less means 8.
	Concurrency int

	// MinProbability, when above zero, turns on automatic picks: pick_item
	// returns the status picked when the top item's probability is at
	// least MinProbability and passes MinMargin. Zero means the model
	// always chooses from the candidates. Set it after you calibrate.
	MinProbability float64
	// MinMargin is how far the top item's probability must be above the
	// next most likely answer, "none of these" included, for an automatic
	// pick. Zero means no check.
	MinMargin float64

	// Price turns a call's usage into Outcome.Cost, which Dashi adds to
	// Spend.ToolCost. model is the resolved model, such as "jev-1.13.0".
	// Nil means no Cost; usage is in Outcome.Data either way. A Cost with
	// a negative amount or a blank currency is dropped, and Outcome.Data
	// then holds "cost_dropped": true.
	Price func(model string, u decide.Usage) *tool.Cost
	// OnResult receives every call's full Result, failed and cancelled
	// calls too, after the call ends. It runs on the call's goroutine
	// with a context that is not cancelled, and should return quickly.
	OnResult func(ctx context.Context, call dashi.CallRef, r Result)
}

// Result is everything one call learned. OnResult receives it; Outcome.Data
// holds the same facts as JSON, shortened when large.
type Result struct {
	// Tool is "judge_items" or "pick_item".
	Tool string
	// List is the list's name.
	List string
	// Query is the judge_items query, or the pick_item looking_for text.
	Query string
	// Items holds every judged item for judge_items, in list order. For
	// pick_item it holds every item, most likely first.
	Items []ItemResult
	// Requests counts the Decide requests sent, failed ones included.
	Requests int
	// Usage is the token usage of every response, added up.
	Usage decide.Usage
	// Model is the resolved model, such as "jev-1.13.0".
	Model string
	// RequestIDs holds Decide's ID for every request that got one, failed
	// requests included: one per item in list order for judge_items, and
	// the one request for pick_item.
	RequestIDs []string
	// Status is pick_item's status, as the model saw it. It is empty for
	// judge_items and when the call failed.
	Status PickStatus
	// NoneProbability is pick_item's probability that none of the items
	// is the one looking_for describes.
	NoneProbability float64
	// Err says why the call failed, if it did. It is the context's error
	// when the call was cancelled. A judge_items call where only some
	// items failed has a nil Err.
	Err error
}

// ItemResult is one item's answers.
type ItemResult struct {
	// ID is the item's ID in the host.
	ID string
	// Answers holds the judge_items answer to each question, by question
	// key: a *decide.NoulAnswer for yes_no, with every probability. It is
	// nil for pick_item and for a failed item.
	Answers map[string]decide.Answer
	// Probability is pick_item's probability that this item is the one
	// looking_for describes. It is zero for judge_items.
	Probability float64
	// RequestID is Decide's ID for the request that judged the item, kept
	// for failed items too when Decide gave one. It is empty for
	// pick_item, whose one request ID is in Result.RequestIDs.
	RequestID string
	// Err is why this item failed, if it did.
	Err error
}

// PickStatus is the status of a pick_item result.
type PickStatus string

// Statuses of a pick_item result.
const (
	// PickStatusRanked: the result lists the most likely items, and the
	// model chooses.
	PickStatusRanked PickStatus = "ranked"
	// PickStatusPicked: the top item passed MinProbability and MinMargin,
	// and the tool chose it.
	PickStatusPicked PickStatus = "picked"
	// PickStatusNoMatch: "none of these" is at least as likely as the top
	// item.
	PickStatusNoMatch PickStatus = "no_match"
)

// Defaults and caps. The tool result limit is the turn's, read at call
// time; these bound everything else.
const (
	defaultMaxQuestions    = 8
	capMaxQuestions        = 64
	defaultMaxRequests     = 500
	defaultMaxTurnRequests = 2000
	defaultMaxItemBytes    = 64 << 10
	defaultConcurrency     = 8
	// defaultResultBytes is Dashi's default tool result limit, used when
	// the tool runs outside a runner.
	defaultResultBytes = 64 << 10
	// maxDataBytes is Dashi's limit on Outcome.Data; larger Data is dropped.
	maxDataBytes = 64 << 10
	// requestBytes bounds the user's message and looking_for in a
	// pick_item request.
	requestBytes = 4 << 10
	// optionBytes bounds each item's Text in a pick_item request.
	optionBytes = 1 << 10
	// shownTextBytes bounds each candidate's text in a pick_item result.
	shownTextBytes = 160
	// pickLimit is Decide's limit on candidates in one Pick.
	pickLimit = 254
)

// settings is a Config with its defaults applied.
type settings struct {
	client          *decide.Client
	lists           []AnyList
	maxQuestions    int
	maxRequests     int // math.MaxInt for no limit
	maxTurnRequests int // math.MaxInt for no limit
	maxItemBytes    int // zero for no limit
	concurrency     int
	minProbability  float64
	minMargin       float64
	price           func(string, decide.Usage) *tool.Cost
	onResult        func(context.Context, dashi.CallRef, Result)
}

func (cfg Config) resolve(toolName string) (*settings, error) {
	c, err := client.Default(cfg.Client)
	if err != nil {
		return nil, fmt.Errorf("listtool: %s: %w", toolName, err)
	}
	if len(cfg.Lists) == 0 {
		return nil, fmt.Errorf("listtool: %s: Config.Lists is empty", toolName)
	}
	seen := map[string]bool{}
	for _, l := range cfg.Lists {
		if l == nil {
			return nil, fmt.Errorf("listtool: %s: Config.Lists holds a nil list", toolName)
		}
		if err := l.check(); err != nil {
			return nil, fmt.Errorf("listtool: %s: %w", toolName, err)
		}
		if seen[l.name()] {
			return nil, fmt.Errorf("listtool: %s: list %q is listed twice", toolName, l.name())
		}
		seen[l.name()] = true
	}
	s := &settings{
		client:          c,
		lists:           append([]AnyList(nil), cfg.Lists...),
		maxQuestions:    cfg.MaxQuestions,
		maxRequests:     limit(cfg.MaxRequests, defaultMaxRequests),
		maxTurnRequests: limit(cfg.MaxTurnRequests, defaultMaxTurnRequests),
		maxItemBytes:    limit(cfg.MaxItemBytes, defaultMaxItemBytes),
		concurrency:     cfg.Concurrency,
		minProbability:  cfg.MinProbability,
		minMargin:       cfg.MinMargin,
		price:           cfg.Price,
		onResult:        cfg.OnResult,
	}
	switch {
	case s.maxQuestions == 0:
		s.maxQuestions = defaultMaxQuestions
	case s.maxQuestions < 0 || s.maxQuestions > capMaxQuestions:
		s.maxQuestions = capMaxQuestions
	}
	if s.maxItemBytes == math.MaxInt {
		s.maxItemBytes = 0 // state.Cut reads zero as no limit
	}
	if s.concurrency <= 0 {
		s.concurrency = defaultConcurrency
	}
	return s, nil
}

// limit applies a default to zero and turns a negative value into no
// limit.
func limit(n, def int) int {
	switch {
	case n == 0:
		return def
	case n < 0:
		return math.MaxInt
	}
	return n
}

func (s *settings) find(name string) (AnyList, bool) {
	for _, l := range s.lists {
		if l.name() == name {
			return l, true
		}
	}
	return nil, false
}

func (s *settings) names() []string {
	names := make([]string, len(s.lists))
	for i, l := range s.lists {
		names[i] = l.name()
	}
	return names
}

// listsText is the end of each tool's description: one line per list.
func (s *settings) listsText() string {
	var b strings.Builder
	b.WriteString("\n\nLists:")
	for _, l := range s.lists {
		fmt.Fprintf(&b, "\n- %s: %s", l.name(), l.describe())
	}
	return b.String()
}

// finish ends a call: it passes r to OnResult, then returns the outcome
// with r as its Data. A cancelled call returns the context's error, so
// Dashi records no Cost and only OnResult sees the spend. Any other
// failure is an ordinary failed outcome that keeps the Cost and Data.
func (s *settings) finish(ctx context.Context, call tool.Call, r Result, output string) (tool.Outcome, error) {
	if s.onResult != nil {
		ref, ok := dashi.CurrentCall(ctx)
		if !ok {
			ref = dashi.CallRef{CallID: call.ID}
		}
		s.onResult(context.WithoutCancel(ctx), ref, r)
	}
	if r.Err != nil && ctx.Err() != nil {
		return tool.Outcome{}, ctx.Err()
	}
	o := tool.Succeeded(output)
	if r.Err != nil {
		o = tool.Failed(output)
	}
	var dropped bool
	o.Cost, dropped = s.cost(r)
	o.Data = record(r, dropped)
	return o, nil
}

// cost prices r's usage. It drops a Cost that Outcome.Validate would
// refuse, so a bad price never stops the turn, and reports the drop.
func (s *settings) cost(r Result) (c *tool.Cost, dropped bool) {
	if s.price == nil || r.Requests == 0 {
		return nil, false
	}
	c = s.price(r.Model, r.Usage)
	if c == nil {
		return nil, false
	}
	if c.Amount < 0 || c.Currency == "" || strings.TrimSpace(c.Currency) != c.Currency {
		return nil, true
	}
	return &tool.Cost{Amount: c.Amount, Currency: c.Currency}, false
}

// resultBytes is the turn's tool result limit, or zero for no limit.
// Output under it reaches the model whole, and Dashi keeps its Data.
func resultBytes(ctx context.Context) int {
	view, ok := dashi.CurrentTurn(ctx)
	if !ok {
		return defaultResultBytes
	}
	switch n := view.Agent.Limits.ToolResultBytes; {
	case n == 0:
		return defaultResultBytes
	case n < 0:
		return 0
	default:
		return n
	}
}

// userRequest is the user's latest message in the turn, cut short.
func userRequest(ctx context.Context) string {
	view, ok := dashi.CurrentTurn(ctx)
	if !ok {
		return ""
	}
	return state.Request(view, requestBytes)
}

// reason says in a few words why a Decide request failed.
func reason(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "Decide timed out"
	case errors.Is(err, decide.ErrAuth), errors.Is(err, decide.ErrNoAPIKey):
		return "Decide refused the API key"
	case errors.Is(err, decide.ErrRateLimited):
		return "Decide is rate limited"
	case errors.Is(err, decide.ErrServer), errors.As(err, &netErr):
		return "Decide is unavailable"
	case errors.Is(err, decide.ErrInvalidAnswer):
		return "Decide's answer did not fit the question"
	}
	text := strings.TrimPrefix(err.Error(), "decide: ")
	return "Decide failed: " + state.Cut(text, 200)
}

// answerError returns the first structural failure in a response's
// answers, or nil. An answer whose numbers only disagree a little, such
// as probabilities that sum to 0.98, is still used.
func answerError(resp *decide.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp == nil || len(resp.Invalid) == 0 {
		return err
	}
	for _, ae := range resp.Invalid {
		if !errors.Is(ae, decide.ErrInconsistentAnswer) || ae.Err != nil {
			return err
		}
	}
	return nil
}

// addUsage adds b's token counts to a.
func addUsage(a *decide.Usage, b decide.Usage) {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
}

// round keeps two decimal places, enough for the model to compare.
func round(p float64) float64 { return math.Round(p*100) / 100 }

// count formats n with thousands separators, such as 1,204.
func count(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + count(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// schema returns an object schema with props and the required names.
func schema(props map[string]any, required ...string) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	})
	if err != nil {
		panic(err)
	}
	return b
}

// executor adapts a function to tool.Executor.
type executor func(context.Context, tool.Call) (tool.Outcome, error)

func (f executor) Execute(ctx context.Context, c tool.Call) (tool.Outcome, error) {
	return f(ctx, c)
}
