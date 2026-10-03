package contextrank

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi/decide/internal/state"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/patterns/fanout"
)

// Decide's request limits, copied from its documentation: at most 64
// questions per request, and 64,000 tokens per request. Bytes are counted,
// at one token or less each, leaving room for JSON framing.
const (
	maxQuestions    = 64
	maxRequestSize  = 56_000
	parallelWorkers = 4
)

// The question asked about each candidate.
const (
	question = "Would this saved item help the assistant answer the user's " +
		"latest message, given the exchange in the state? The item:\n"
	criteriaTrue  = "The answer should use facts, preferences, or context from this item."
	criteriaFalse = "The item is unrelated, or the answer does not need it."
)

// judged is what the Decide requests returned in total.
type judged struct {
	model      string
	requestIDs []string
	usage      decide.Usage
}

// judge asks Decide about each option, in parallel requests that each fit
// Decide's limits, and sets each option's P(yes) or CauseUnanswered. Any
// failed request fails the whole ranking, since its candidates cannot be
// judged; an invalid answer drops only its candidate.
func judge(
	ctx context.Context,
	c *decide.Client,
	exchange string,
	asked []*option,
) (judged, error) {
	chunks := split(len(exchange), asked)
	results, err := fanout.Map(ctx, c, chunks, parallelWorkers, func(
		_ context.Context, _ int, chunk []*option,
	) (*decide.Request, error) {
		req := decide.NewRequest(exchange)
		for i, o := range chunk {
			req.Questions[key(i)] = ask(o)
		}
		return req, nil
	})
	var out judged
	var models []string
	var errs []error
	for _, r := range results {
		if resp := r.Response; resp != nil {
			if resp.RequestID != "" {
				out.requestIDs = append(out.requestIDs, resp.RequestID)
			}
			out.usage.InputTokens += resp.Usage.InputTokens
			out.usage.OutputTokens += resp.Usage.OutputTokens
			if resp.Model != "" && !slices.Contains(models, resp.Model) {
				models = append(models, resp.Model)
			}
			for i, o := range chunks[r.Index] {
				o.result.Probability, o.result.Cause = answer(resp, key(i))
			}
			continue
		}
		if r.Err != nil {
			errs = append(errs, r.Err)
		}
	}
	slices.Sort(models)
	out.model = strings.Join(models, ",")
	if err != nil {
		return out, err
	}
	return out, errors.Join(errs...)
}

// split groups options into requests of at most maxQuestions questions and
// about maxRequestSize bytes, the state included.
func split(stateSize int, asked []*option) [][]*option {
	var chunks [][]*option
	var chunk []*option
	size := stateSize
	for _, o := range asked {
		n := len(question) + len(text(o.Candidate)) + 256
		if len(chunk) == maxQuestions || len(chunk) > 0 && size+n > maxRequestSize {
			chunks = append(chunks, chunk)
			chunk, size = nil, stateSize
		}
		chunk = append(chunk, o)
		size += n
	}
	if len(chunk) > 0 {
		chunks = append(chunks, chunk)
	}
	return chunks
}

// key is a question's key: its position in the request, since Decide
// limits the characters of a question key and host IDs may use others.
func key(i int) string {
	return "item_" + strconv.Itoa(i)
}

func ask(o *option) *decide.NoulQuestion {
	return decide.Noul(question+text(o.Candidate),
		decide.NoulTrue(criteriaTrue),
		decide.NoulFalse(criteriaFalse),
	)
}

// text is what Decide reads of a candidate.
func text(c Candidate) string {
	if c.Text != "" {
		return state.Cut(c.Text, maxTextBytes)
	}
	return state.Cut(c.Content, maxTextBytes)
}

// answer returns the P(yes) for key, or CauseUnanswered when the response
// holds no valid answer for it.
func answer(resp *decide.Response, key string) (float64, Cause) {
	if resp.Invalid[key] != nil {
		return 0, CauseUnanswered
	}
	a, ok := resp.Answers[key].(*decide.NoulAnswer)
	if !ok || a == nil {
		return 0, CauseUnanswered
	}
	return a.Noul, ""
}
