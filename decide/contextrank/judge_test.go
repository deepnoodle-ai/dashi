package contextrank_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/contextrank"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// unitTransport answers each question with the P(yes) of a marker in its
// item text, or 0.9, as a transport with validation off can, so a P(yes)
// can be NaN or outside 0 to 1.
type unitTransport map[string]float64

func (u unitTransport) SystemOne(_ context.Context, req *decide.Request) (*decide.Response, error) {
	resp := &decide.Response{Answers: map[string]decide.Answer{}, RequestID: "req_1"}
	for key, q := range req.Questions {
		instructions, _ := q.(*decide.NoulQuestion).Instructions.(string)
		p := 0.9
		for marker, s := range u {
			if strings.Contains(instructions, marker) {
				p = s
			}
		}
		resp.Answers[key] = decidetest.NoulAnswer(p)
	}
	return resp, nil
}

func (unitTransport) ListModels(context.Context) (*decide.ModelList, error) {
	return nil, errors.ErrUnsupported
}

// TestInvalidProbabilityIsUnanswered gives candidates a NaN and an
// out-of-range P(yes). With no floor, NaN passed every comparison and was
// chosen; both are unanswered instead.
func TestInvalidProbabilityIsUnanswered(t *testing.T) {
	client, err := decide.NewClient(
		decide.WithTransport(unitTransport{"nan": math.NaN(), "over": 1.5}),
		decide.WithValidation(false))
	if err != nil {
		t.Fatal(err)
	}
	var rec record
	ranker := &contextrank.Ranker{
		Client:         client,
		Source:         fixed(note("good", 50), note("nan", 50), note("over", 50)),
		MinProbability: -1,
		OnSelection:    rec.add,
	}
	model := dashitest.NewClient(dashitest.Reply("ok"))
	runner := dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()}
	if _, err := runner.Run(t.Context(), dashi.Turn{}, start("hi")); err != nil {
		t.Fatal(err)
	}
	if got := sent(model.Inputs()[0]); !slices.Equal(got, []string{"good"}) {
		t.Fatalf("model saw %v", got)
	}
	for _, id := range []string{"nan", "over"} {
		if c := causes(rec.all()[0])[id]; c != contextrank.CauseUnanswered {
			t.Fatalf("%s = %q", id, c)
		}
	}
}
