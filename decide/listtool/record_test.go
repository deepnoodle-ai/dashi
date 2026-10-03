package listtool

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// recordShape is the part of a record these tests read.
type recordShape struct {
	Requests     int
	RequestIDs   []string `json:"request_ids"`
	DroppedItems int      `json:"dropped_items"`
	Items        []struct {
		ID        string
		Answers   map[string]map[string]any
		RequestID string `json:"request_id"`
		Error     string
	}
}

func decodeRecord(t *testing.T, data json.RawMessage) recordShape {
	t.Helper()
	if len(data) == 0 || len(data) > maxDataBytes {
		t.Fatalf("%d bytes of Data", len(data))
	}
	var got recordShape
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// requestID is as long as a real Decide request ID.
func requestID(i int) string { return fmt.Sprintf("req_01JAB3XK9Z7Q4M2N8P5R%08d", i) }

func TestRecordShortensToFitDashisDataLimit(t *testing.T) {
	options := make([]decide.ChoiceOption, 10)
	for i := range options {
		options[i] = decide.Option(fmt.Sprintf("option_%d", i))
	}
	q := decide.Choice("Which?", options...)
	result := func(n int) Result {
		r := Result{List: "tickets", Requests: n, Model: "jev-1.13.0"}
		for i := range n {
			r.RequestIDs = append(r.RequestIDs, requestID(i))
			it := ItemResult{ID: fmt.Sprintf("T-%04d", i), RequestID: requestID(i)}
			if i == n-1 {
				it.Err = errors.New("Decide timed out")
			} else {
				it.Answers = map[string]decide.Answer{
					"kind": decidetest.ChoiceFor(q, 0.55, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05),
				}
			}
			r.Items = append(r.Items, it)
		}
		return r
	}
	for _, tc := range []struct {
		items   int
		want    string // what the first item's answer holds
		ids     bool   // whether the request ID list is kept
		dropped bool   // whether items are dropped
	}{
		{10, "probabilities", true, false},
		{400, "p", true, false},
		{3000, "p", false, true},
	} {
		got := decodeRecord(t, record(result(tc.items), false))
		if got.Requests != tc.items {
			t.Errorf("%d items: requests = %d", tc.items, got.Requests)
		}
		if _, ok := got.Items[0].Answers["kind"][tc.want]; !ok {
			t.Errorf("%d items: first answer = %v", tc.items, got.Items[0].Answers)
		}
		if (len(got.RequestIDs) == tc.items) != tc.ids || got.Items[0].RequestID != "" {
			t.Errorf("%d items: %d request IDs, first item's %q", tc.items, len(got.RequestIDs), got.Items[0].RequestID)
		}
		if len(got.Items)+got.DroppedItems != tc.items || (got.DroppedItems > 0) != tc.dropped {
			t.Errorf("%d items: kept %d, dropped %d", tc.items, len(got.Items), got.DroppedItems)
		}
		last := got.Items[len(got.Items)-1]
		if last.Error == "" || last.RequestID != requestID(tc.items-1) {
			t.Errorf("%d items: the failed item is %+v", tc.items, last)
		}
	}
}

// At the default limits, MaxRequests 500 with two yes_no questions, the
// record keeps every item's most likely answers. It leaves out the list
// of request IDs to make room.
func TestRecordKeepsEveryItemAtDefaultLimits(t *testing.T) {
	r := Result{List: "tickets", Query: "status:open", Model: "jev-1.13.0", Requests: defaultMaxRequests}
	for i := range defaultMaxRequests {
		r.RequestIDs = append(r.RequestIDs, requestID(i))
		r.Items = append(r.Items, ItemResult{
			ID:        fmt.Sprintf("T-%05d", i),
			RequestID: requestID(i),
			Answers: map[string]decide.Answer{
				"billing": decidetest.NoulAnswer(0.9734512345678912),
				"urgent":  decidetest.NoulAnswer(0.0123456789012345),
			},
		})
	}
	data := record(r, false)
	got := decodeRecord(t, data)
	t.Logf("%d items, 2 questions: %d bytes", len(r.Items), len(data))
	if len(got.Items) != defaultMaxRequests || got.DroppedItems != 0 {
		t.Fatalf("kept %d items and %d request IDs, dropped %d",
			len(got.Items), len(got.RequestIDs), got.DroppedItems)
	}
	for _, it := range got.Items {
		if len(it.Answers) != 2 {
			t.Fatalf("item %s kept answers %v", it.ID, it.Answers)
		}
	}
}

func TestRecordSaysWhenACostWasDropped(t *testing.T) {
	var got struct {
		CostDropped bool `json:"cost_dropped"`
	}
	if err := json.Unmarshal(record(Result{List: "tickets"}, true), &got); err != nil || !got.CostDropped {
		t.Errorf("cost_dropped = %v, %v", got.CostDropped, err)
	}
}
