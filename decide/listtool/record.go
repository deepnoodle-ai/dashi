package listtool

import (
	"encoding/json"
	"sort"

	"github.com/deepnoodle-ai/decide"
)

// callRecord is a call's Outcome.Data: the facts of its Result as JSON.
type callRecord struct {
	List            string       `json:"list"`
	Query           string       `json:"query,omitempty"`
	Model           string       `json:"model,omitempty"`
	Requests        int          `json:"requests"`
	Usage           decide.Usage `json:"usage"`
	RequestIDs      []string     `json:"request_ids,omitempty"`
	Status          PickStatus   `json:"status,omitempty"`
	NoneProbability *float64     `json:"none_probability,omitempty"`
	Items           []itemRecord `json:"items,omitempty"`
	DroppedItems    int          `json:"dropped_items,omitempty"`
	CostDropped     bool         `json:"cost_dropped,omitempty"`
	Error           string       `json:"error,omitempty"`
}

type itemRecord struct {
	ID string `json:"id"`
	// Answers is map[string]decide.Answer with every probability, or, when
	// that is too large, map[string]likelyAnswer.
	Answers     any      `json:"answers,omitempty"`
	Probability *float64 `json:"probability,omitempty"`
	// RequestID is kept for a failed item only; the call's request IDs are
	// in callRecord.RequestIDs.
	RequestID string `json:"request_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// likelyAnswer is a question's most likely answer and its probability.
type likelyAnswer struct {
	Answer string  `json:"answer"`
	P      float64 `json:"p"`
}

// record returns r as Outcome.Data, in the first of these forms that fits
// under Dashi's Data limit:
//
//  1. every item with every probability, and the call's request IDs;
//  2. each judged item's most likely answers and their probabilities,
//     and the request IDs;
//  3. as many items of form 2 as fit, in list order, with no request ID
//     list, and dropped_items counting the items left out.
//
// Every form keeps the call's facts, and failed items keep their errors
// and request IDs. At the default MaxRequests, 500 items with two
// questions each, form 3 keeps every item (about 50 KB) and drops only
// the request ID list.
func record(r Result, costDropped bool) json.RawMessage {
	base := callRecord{
		List:        r.List,
		Query:       r.Query,
		Model:       r.Model,
		Requests:    r.Requests,
		Usage:       decide.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens},
		RequestIDs:  r.RequestIDs,
		Status:      r.Status,
		CostDropped: costDropped,
	}
	if r.Err != nil {
		base.Error = r.Err.Error()
	}
	if r.Status != "" {
		base.NoneProbability = new(r.NoneProbability)
	}
	if len(r.Items) == 0 {
		return encode(base)
	}
	full := make([]itemRecord, len(r.Items))
	likely := make([]itemRecord, len(r.Items))
	for i, it := range r.Items {
		full[i] = itemRecord{ID: it.ID}
		if it.Err != nil {
			full[i].Error, full[i].RequestID = it.Err.Error(), it.RequestID
		}
		if r.Status != "" {
			full[i].Probability = new(it.Probability)
		}
		likely[i] = full[i]
		if it.Answers != nil {
			full[i].Answers = it.Answers
			likely[i].Answers = likelyAnswers(it.Answers)
		}
	}
	withItems := func(items []itemRecord, dropped int) callRecord {
		rec := base
		rec.Items, rec.DroppedItems = items, dropped
		return rec
	}
	if b := encode(withItems(full, 0), withItems(likely, 0)); b != nil {
		return b
	}
	base.RequestIDs = nil
	return fitItems(likely, func(items []itemRecord, dropped int) json.RawMessage {
		return encode(withItems(items, dropped))
	})
}

// fitItems keeps every failed item and as many other items as fit, in list
// order, and returns the encoded record. When not even the failed items
// fit, it keeps no items.
func fitItems(items []itemRecord, enc func(kept []itemRecord, dropped int) json.RawMessage) json.RawMessage {
	answered := 0
	for _, it := range items {
		if it.Error == "" {
			answered++
		}
	}
	keep := func(n int) json.RawMessage {
		var kept []itemRecord
		taken := 0
		for _, it := range items {
			if it.Error == "" {
				if taken == n {
					continue
				}
				taken++
			}
			kept = append(kept, it)
		}
		return enc(kept, len(items)-len(kept))
	}
	// The largest n whose record fits.
	n := sort.Search(answered+1, func(n int) bool { return keep(n) == nil }) - 1
	if n < 0 {
		return enc(nil, len(items))
	}
	return keep(n)
}

func likelyAnswers(answers map[string]decide.Answer) map[string]likelyAnswer {
	out := make(map[string]likelyAnswer, len(answers))
	for key, a := range answers {
		answer, p := likely(a)
		out[key] = likelyAnswer{answer, round(p)}
	}
	return out
}

// encode returns the first form of the record that fits under Dashi's
// Data limit, or nil when none does.
func encode(forms ...any) json.RawMessage {
	for _, f := range forms {
		b, err := json.Marshal(f)
		if err == nil && len(b) <= maxDataBytes {
			return b
		}
	}
	return nil
}
