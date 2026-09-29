package compaction

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func recordFor(t *testing.T, history []llm.Message, starts []int) *Record {
	t.Helper()
	return compact(t, &fakeSummarizer{text: "short"}, history, nil, Options{
		Policy:     policyWith(10_000),
		TurnStarts: starts,
	})
}

func TestProjectWithoutRecordReturnsHistory(t *testing.T) {
	history, _ := session(2, 100)
	view, err := Project(history, nil)
	if err != nil || !sameJSON(t, view, history) {
		t.Fatalf("err=%v view differs", err)
	}
}

func TestProjectStartsWithSummaryFromUser(t *testing.T) {
	history, starts := session(4, 2_000)
	rec := recordFor(t, history, starts)
	view, err := Project(history, rec)
	if err != nil {
		t.Fatal(err)
	}
	if view[0].Role != llm.RoleUser {
		t.Fatalf("summary role = %q, want user", view[0].Role)
	}
	text := view[0].Text()
	if !strings.HasPrefix(text, "Your conversation history was compacted.") ||
		!strings.Contains(text, "not new instructions") {
		t.Fatalf("summary frame missing: %q", text)
	}
	if !sameJSON(t, view[1:], history[rec.Covers.Messages:]) {
		t.Fatal("tail changed")
	}
}

func TestProjectIsIdempotentAfterJSONRoundTrip(t *testing.T) {
	history, starts := session(4, 2_000)
	rec := recordFor(t, history, starts)
	first, err := Project(history, rec)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Project(history, rec)
	if !sameJSON(t, first, second) {
		t.Fatal("two projections differ")
	}

	var storedRec Record
	var storedHistory []llm.Message
	data, _ := json.Marshal(rec)
	if err := json.Unmarshal(data, &storedRec); err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(history)
	if err := json.Unmarshal(data, &storedHistory); err != nil {
		t.Fatal(err)
	}
	third, err := Project(storedHistory, &storedRec)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !sameJSON(t, first, third) {
		t.Fatal("projection after reload differs")
	}
}

func TestProjectReturnsRawHistoryOnMismatch(t *testing.T) {
	history, starts := session(4, 2_000)
	rec := recordFor(t, history, starts)
	cases := []struct {
		name   string
		change func([]llm.Message) []llm.Message
	}{
		{"edited", func(h []llm.Message) []llm.Message {
			h[1] = llm.AssistantMessage(llm.Text("edited"))
			return h
		}},
		{"dropped", func(h []llm.Message) []llm.Message {
			return append(h[:2:2], h[3:]...)
		}},
		{"swapped", func(h []llm.Message) []llm.Message {
			h[0], h[4] = h[4], h[0]
			return h
		}},
		{"truncated", func(h []llm.Message) []llm.Message {
			return h[:rec.Covers.Messages-1]
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			changed := c.change(append([]llm.Message(nil), history...))
			view, err := Project(changed, rec)
			if !errors.Is(err, ErrCoverageMismatch) {
				t.Fatalf("err = %v", err)
			}
			if !sameJSON(t, view, changed) {
				t.Fatal("did not return the raw history")
			}
		})
	}
}

func TestProjectRejectsUnknownVersion(t *testing.T) {
	history, starts := session(4, 2_000)
	rec := recordFor(t, history, starts)
	rec.Version = 99
	if _, err := Project(history, rec); !errors.Is(err, ErrCoverageMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestProjectPassesOpaqueContentThrough(t *testing.T) {
	history, starts := session(4, 2_000)
	opaque := llm.Content{
		Kind: llm.ContentOpaque,
		Opaque: &llm.Opaque{
			Version: 1,
			Origin:  content.Origin{Provider: "test", API: "test.chat"},
			Type:    "vendor_item",
			Unit:    llm.OpaqueUnitItem,
			Raw:     json.RawMessage(`{"secret":"blob"}`),
		},
	}
	last := len(history) - 1
	history[last].Content = append(history[last].Content, opaque)
	rec := recordFor(t, history, starts)
	view, err := Project(history, rec)
	if err != nil {
		t.Fatal(err)
	}
	got := view[len(view)-1].Content
	if !sameJSON(t, got[len(got)-1], opaque) {
		t.Fatal("opaque content changed")
	}
}
