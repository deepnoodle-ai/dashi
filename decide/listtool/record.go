package listtool

import (
	"encoding/json"

	"github.com/deepnoodle-ai/decide"
)

// callRecord is a call's Outcome.Data: the facts of its Result as JSON.
type callRecord struct {
	List            string       `json:"list"`
	Query           string       `json:"query,omitempty"`
	Model           string       `json:"model,omitempty"`
	Requests        int          `json:"requests"`
	Usage           decide.Usage `json:"usage"`
	Status          string       `json:"status,omitempty"`
	NoneProbability *float64     `json:"none_probability,omitempty"`
	Items           []itemRecord `json:"items,omitempty"`
	Error           string       `json:"error,omitempty"`
}

type itemRecord struct {
	ID string `json:"id"`
	// Answers is map[string]decide.Answer with every probability, or, when
	// that is too large, map[string]likelyAnswer.
	Answers     any      `json:"answers,omitempty"`
	Probability *float64 `json:"probability,omitempty"`
	RequestID   string   `json:"request_id,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// likelyAnswer is a question's most likely answer and its probability.
type likelyAnswer struct {
	Answer string  `json:"answer"`
	P      float64 `json:"p"`
}

// shortPickItems is how many pick_item items the shorter record keeps.
const shortPickItems = 20

// record returns r as Outcome.Data. It keeps every probability when that fits
// under Dashi's Data limit. Otherwise it keeps each judged item's most
// likely answers (or pick_item's most likely items), and when even that is
// too large, only the counts. Failed items keep their request IDs.
func record(r Result) json.RawMessage {
	base := callRecord{
		List:     r.List,
		Query:    r.Query,
		Model:    r.Model,
		Requests: r.Requests,
		Usage:    decide.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens},
		Status:   r.Status,
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
	full, short := base, base
	full.Items = make([]itemRecord, len(r.Items))
	for i, it := range r.Items {
		full.Items[i] = itemRecord{ID: it.ID, RequestID: it.RequestID}
		if it.Err != nil {
			full.Items[i].Error = it.Err.Error()
		}
		if it.Answers != nil {
			full.Items[i].Answers = it.Answers
		}
		if r.Status != "" {
			full.Items[i].Probability = new(it.Probability)
		}
	}
	if r.Status != "" {
		short.Items = full.Items[:min(len(full.Items), shortPickItems)]
	} else {
		short.Items = make([]itemRecord, len(full.Items))
		for i, it := range full.Items {
			short.Items[i] = it
			if r.Items[i].Answers == nil {
				continue
			}
			answers := make(map[string]likelyAnswer, len(r.Items[i].Answers))
			for key, a := range r.Items[i].Answers {
				answer, p := likely(a)
				answers[key] = likelyAnswer{answer, round(p)}
			}
			short.Items[i].Answers = answers
		}
	}
	return encode(full, short, base)
}
