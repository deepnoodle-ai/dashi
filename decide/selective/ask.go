package selective

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"

	"github.com/deepnoodle-ai/dashi/internal/compactionrules"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/patterns/compact"
	"github.com/deepnoodle-ai/decide/patterns/fanout"
)

// State size limits, in bytes of JSON-encoded strings. Decide's request limits are in
// tokens, and a token is at least one byte, so bytes bound them. The state
// stays well under compact.Jev113's StateAndQuestion, leaving room for a
// segment in each question.
const (
	taskBytes    = 3_000
	requestBytes = 3_000
	recentBytes  = 8_000
)

// focus is the state Decide judges each segment against.
type focus struct {
	Task           string `json:"task"`
	LatestRequest  string `json:"latest_request"`
	RecentMessages string `json:"recent_messages"`
}

// focusState returns the encoded state: the task (the first of the user's
// own words), the latest request (the last of them), and the end of the
// tail as text.
func focusState(messages []llm.Message, cut int, p *plan) json.RawMessage {
	var f focus
	for _, m := range messages {
		if m, ok := compactionrules.WithoutReminders(m); ok && ownWords(m) {
			f.Task = cutEncoded(m.Text(), taskBytes, false)
			break
		}
	}
	for _, m := range slices.Backward(messages) {
		if m, ok := compactionrules.WithoutReminders(m); ok && ownWords(m) {
			f.LatestRequest = cutEncoded(m.Text(), requestBytes, false)
			break
		}
	}
	f.RecentMessages = cutEncoded(render(messages[cut:]), recentBytes, true)
	b, _ := json.Marshal(f)
	return b
}

// cutEncoded keeps a prefix, or a suffix when keepEnd is true, whose JSON
// string fits maxBytes. JSON quotes and escaping count toward the limit.
func cutEncoded(text string, maxBytes int, keepEnd bool) string {
	part := func(n int) string {
		if !keepEnd {
			return text[:runeStart(text, n)]
		}
		start := len(text) - n
		for start < len(text) && runeStart(text, start) != start {
			start++
		}
		return text[start:]
	}
	end := sort.Search(len(text)+1, func(n int) bool {
		encoded, _ := json.Marshal(part(n))
		return len(encoded) > maxBytes
	})
	if end == 0 {
		return ""
	}
	return part(end - 1)
}

// questions asks Decide about segments against one state.
type questions struct {
	state    json.RawMessage
	verbatim bool // ask whether the exact text is needed
	limits   compact.Limits
}

// answer is Decide's judgment of one segment.
type answer struct {
	scores      compact.Scores
	shortJudged bool
}

func neededKey(i int) string   { return "s" + strconv.Itoa(i) + ".needed" }
func verbatimKey(i int) string { return "s" + strconv.Itoa(i) + ".verbatim" }

// batch is one Decide request and the segments it asks about.
type batch struct {
	req  *decide.Request
	segs []int
}

// ask sends the questions about segs, packed into requests under the
// limits, with at most workers at once, and returns an answer per segment.
// A segment too large to ask about whole is asked about by its short form;
// one too large either way is not scored. A failed request fails the call;
// an invalid answer leaves its segment unscored. Usage, request IDs, and
// models go into sel, also when the call fails.
func (q questions) ask(
	ctx context.Context,
	client *decide.Client,
	segs []segment,
	workers int,
	sel *Selection,
) ([]answer, error) {
	if q.limits == (compact.Limits{}) {
		q.limits = compact.Jev113
	}
	answers := make([]answer, len(segs))
	batches, err := q.pack(segs, answers, client.DefaultModel())
	if err != nil {
		return nil, err
	}
	results, failed := fanout.Map(ctx, client, batches, workers,
		func(_ context.Context, _ int, b *batch) (*decide.Request, error) { return b.req, nil })
	models := map[string]bool{}
	for n, r := range results {
		if r.Started {
			sel.Requests++
		}
		if r.Response != nil {
			sel.Usage.InputTokens += r.Response.Usage.InputTokens
			sel.Usage.OutputTokens += r.Response.Usage.OutputTokens
			if r.Response.RequestID != "" {
				sel.RequestIDs = append(sel.RequestIDs, r.Response.RequestID)
			}
			if r.Response.Model != "" {
				models[r.Response.Model] = true
			}
		}
		if r.Err != nil && !errors.Is(r.Err, decide.ErrInvalidAnswer) || r.Response == nil {
			if failed == nil {
				failed = fmt.Errorf("request %d: %w", n, cmpErr(r.Err))
			}
			continue
		}
		for _, i := range batches[n].segs {
			answers[i].scores = read(batches[n].req, r.Response, i)
		}
	}
	sel.Models = slices.Sorted(maps.Keys(models))
	if failed != nil {
		return nil, failed
	}
	return answers, nil
}

func cmpErr(err error) error {
	if err == nil {
		return errors.New("no response")
	}
	return err
}

// pack builds the requests. A segment whose questions do not fit is asked
// about by its short form, and otherwise gets an unscored compact.ErrTooLarge
// in answers.
func (q questions) pack(segs []segment, answers []answer, model string) ([]*batch, error) {
	stateSize := len(q.state)
	if stateSize > q.limits.StateAndQuestion {
		return nil, fmt.Errorf("%w: the state is %d bytes", compact.ErrTooLarge, stateSize)
	}
	newRequest := func() *decide.Request {
		// Freeze the model so the size counts what the client sends.
		return decide.NewRequest(q.state, decide.WithRequestModel(model))
	}
	fits := func(qs map[string]decide.Question) (bool, error) {
		longest := 0
		for key, question := range qs {
			b, err := json.Marshal(question)
			if err != nil {
				return false, err
			}
			longest = max(longest, len(strconv.Quote(key))+1+len(b))
		}
		req := newRequest()
		maps.Copy(req.Questions, qs)
		size, err := requestSize(req)
		return err == nil && stateSize+longest <= q.limits.StateAndQuestion &&
			size <= q.limits.Request, err
	}
	var batches []*batch
	var cur *batch
	for i, s := range segs {
		text := render(s.whole)
		qs := map[string]decide.Question{neededKey(i): neededQuestion(text)}
		if q.verbatim && s.short != nil {
			qs[verbatimKey(i)] = decide.Noul(map[string]string{
				"question":   compact.DefaultVerbatim,
				"segment":    text,
				"short_form": render(s.short),
			})
		}
		ok, err := fits(qs)
		if err != nil {
			return nil, err
		}
		if !ok && len(qs) > 1 {
			// Without the second copy of the text, the whole segment may
			// fit; Select then prefers it whole.
			qs = map[string]decide.Question{neededKey(i): neededQuestion(text)}
			if ok, err = fits(qs); err != nil {
				return nil, err
			}
		}
		if !ok && s.short != nil {
			qs = map[string]decide.Question{neededKey(i): neededQuestion(render(s.short))}
			answers[i].shortJudged = true
			if ok, err = fits(qs); err != nil {
				return nil, err
			}
		}
		if !ok {
			answers[i].scores.Err = fmt.Errorf("%w: segment %d", compact.ErrTooLarge, i)
			continue
		}
		if cur != nil {
			maps.Copy(cur.req.Questions, qs)
			size, err := requestSize(cur.req)
			if err != nil {
				return nil, err
			}
			if size > q.limits.Request {
				for key := range qs {
					delete(cur.req.Questions, key)
				}
				cur = nil
			}
		}
		if cur == nil {
			cur = &batch{req: newRequest()}
			maps.Copy(cur.req.Questions, qs)
			batches = append(batches, cur)
		}
		cur.segs = append(cur.segs, i)
	}
	return batches, nil
}

func neededQuestion(text string) decide.Question {
	return decide.Noul(map[string]string{
		"question": compact.DefaultNeeded,
		"segment":  text,
	})
}

func requestSize(req *decide.Request) (int, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("encode request: %w", err)
	}
	return len(b), nil
}

// read returns segment i's scores from resp, the answer to req, or an
// unscored error when an answer is missing or invalid.
func read(req *decide.Request, resp *decide.Response, i int) compact.Scores {
	var s compact.Scores
	for _, key := range []string{neededKey(i), verbatimKey(i)} {
		if _, asked := req.Questions[key]; !asked {
			continue
		}
		noul, ok := resp.Answers[key].(*decide.NoulAnswer)
		if !ok || noul == nil || resp.Invalid[key] != nil {
			return compact.Scores{Err: fmt.Errorf("segment %d: no valid answer to %s", i, key)}
		}
		// Validation tolerates noise just outside [0,1]; Select does not.
		p := min(max(noul.Noul, 0), 1)
		if key == neededKey(i) {
			s.Needed = p
		} else {
			s.Verbatim, s.HasVerbatim = p, true
		}
	}
	return s
}
