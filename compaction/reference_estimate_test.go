package compaction

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestReferenceEstimateCountsEveryOccurrenceWithoutIO(t *testing.T) {
	ref := &content.Ref{ID: "not-a-path-or-resolvable-id", SHA256: strings.Repeat("a", 64), Size: 9}
	source := content.Source{Ref: ref, MIMEType: "image/png"}
	image := content.ImageBlock(source)
	m := llm.Message{Role: llm.RoleUser, Content: []llm.Content{
		llm.Image(source),
		{Kind: llm.ContentToolResult, ToolResult: &llm.ToolResult{CallID: "call", Output: tool.Output{Content: []content.Block{image, image}}}},
		{Kind: llm.ContentHostedUse, HostedUse: &llm.HostedUse{Output: []content.Block{image}}},
		{Kind: llm.ContentHostedResult, HostedResult: &llm.HostedResult{Output: []content.Block{image}}},
	}}
	before := m.Clone()
	digest, err := digestOf([]llm.Message{m})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := (len(encoded)+5*12)/bytesPerToken + 1
	if got := estimate(m); got != want {
		t.Fatalf("estimate=%d want=%d", got, want)
	}
	if !reflect.DeepEqual(m, before) {
		t.Fatal("estimate mutated recorded content")
	}
	after, err := digestOf([]llm.Message{m})
	if err != nil || after != digest {
		t.Fatal("estimate changed coverage digest", err)
	}
	m.Context = &llm.MessageContext{}
	if got := estimate(m); got != want {
		t.Fatalf("context changed estimate: %d", got)
	}
}

func TestReferenceEstimateSaturates(t *testing.T) {
	for _, size := range []int64{math.MaxInt64, math.MaxInt64 - 1} {
		m := llm.Message{Role: llm.RoleUser, Content: []llm.Content{llm.Image(content.Source{Ref: &content.Ref{ID: "huge", Size: size}, MIMEType: "image/png"})}}
		if got := estimate(m); got != math.MaxInt {
			t.Fatalf("size=%d estimate=%d", size, got)
		}
	}
	for _, size := range []int64{1, 2, 3, 4, 6, 7} {
		want := int((size+2)/3) * 4
		if got := referenceWireSize(size); got != want {
			t.Fatalf("size=%d wire=%d want=%d", size, got, want)
		}
	}
}
