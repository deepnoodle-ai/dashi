package listtool

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

func TestRecordShortensToFitDashisDataLimit(t *testing.T) {
	options := make([]decide.ChoiceOption, 10)
	for i := range options {
		options[i] = decide.Option(fmt.Sprintf("option_%d", i))
	}
	q := decide.Choice("Which?", options...)
	result := func(n int) Result {
		r := Result{List: "tickets", Requests: n, Model: "jev-1.13.0", Err: nil}
		for i := range n {
			r.Items = append(r.Items, ItemResult{
				ID:        fmt.Sprintf("T-%04d", i),
				RequestID: fmt.Sprintf("req_%d", i),
				Answers: map[string]decide.Answer{
					"kind": decidetest.ChoiceFor(q, 0.55, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05),
				},
			})
		}
		return r
	}
	for _, tc := range []struct {
		items int
		want  string // what the first item's answer holds
	}{
		{10, "probabilities"},
		{400, "p"},
		{3000, ""},
	} {
		data := record(result(tc.items))
		if len(data) == 0 || len(data) > maxDataBytes {
			t.Fatalf("%d items: %d bytes of Data", tc.items, len(data))
		}
		var got struct {
			Requests int
			Items    []struct{ Answers map[string]map[string]any }
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Requests != tc.items {
			t.Errorf("%d items: requests = %d", tc.items, got.Requests)
		}
		if tc.want == "" {
			if len(got.Items) != 0 {
				t.Errorf("%d items: kept %d items", tc.items, len(got.Items))
			}
			continue
		}
		if _, ok := got.Items[0].Answers["kind"][tc.want]; !ok || len(got.Items) != tc.items {
			t.Errorf("%d items: first answer = %v", tc.items, got.Items[0].Answers)
		}
	}
}
