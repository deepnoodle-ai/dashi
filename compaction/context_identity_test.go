package compaction

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestContextIdentityPreservesLegacyCoverageAndEstimates(t *testing.T) {
	history, starts := session(6, 400)
	rec := compact(t, &fakeSummarizer{text: "short"}, history, nil, Options{Policy: policyWith(20000), TurnStarts: starts, TailFraction: 0.1})
	before, err := digestOf(history)
	if err != nil {
		t.Fatal(err)
	}
	for i := range history {
		oldEstimate := estimate(history[i])
		history[i].Context = &llm.MessageContext{Origin: "runtime", Name: "persistent context slot with a deliberately long name"}
		if estimate(history[i]) != oldEstimate {
			t.Fatal("descriptive identity changed token estimate")
		}
	}
	after, err := digestOf(history)
	if err != nil || before != after {
		t.Fatalf("identity changed digest %s %s %v", before, after, err)
	}
	if _, err := Project(history, rec); err != nil {
		t.Fatalf("old compaction record lost coverage: %v", err)
	}
	// Coverage comparison must not strip metadata from caller-owned messages.
	if history[0].Context == nil {
		t.Fatal("digest changed input")
	}
	history[0].Content = []llm.Content{llm.Text("changed")}
	changed, err := digestOf(history)
	if err != nil || changed == before {
		t.Fatalf("model-visible change lost: %s %v", changed, err)
	}
}
