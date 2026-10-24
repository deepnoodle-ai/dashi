package gemini

import (
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func readTestStream(t *testing.T, chunks ...string) (llm.Result, []llm.Delta, error) {
	t.Helper()
	var body strings.Builder
	for _, chunk := range chunks {
		body.WriteString("data: " + chunk + "\n\n")
	}
	var deltas []llm.Delta
	wire := codec{origin: meterOrigin(t)}
	result, err := wire.readStream(
		t.Context(),
		io.NopCloser(strings.NewReader(body.String())),
		llm.Result{Requested: llm.ModelRef{Provider: "google", Model: "requested"}},
		func(d llm.Delta) { deltas = append(deltas, d) },
	)
	return result, deltas, err
}

// A signed code part after text starts its own part; only a text fragment
// extends the text before it.
func TestStreamKeepsSignedCodePartAfterText(t *testing.T) {
	result, _, err := readTestStream(
		t,
		`{"responseId":"r1","candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"executableCode":{"language":"PYTHON","code":"1"},"thoughtSignature":"c2ln"}]},"finishReason":"STOP"}]}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	content := result.Message.Content
	if len(content) != 2 || content[0].Block == nil ||
		content[1].ProviderToolUse == nil {
		t.Fatalf("code part was lost: %+v", content)
	}
}

// A stream whose assembled response fails to decode reports the failure
// instead of an empty result without an error.
func TestStreamReportsDecodeFailure(t *testing.T) {
	_, _, err := readTestStream(
		t,
		`{"responseId":"r1","candidates":[{"content":{"parts":[{"functionCall":{"name":""}}]},"finishReason":"STOP"}]}`,
	)
	if e, ok := err.(*llm.Error); !ok || e.Category != llm.ErrorProtocol {
		t.Fatalf("err = %v", err)
	}
}

// Delta indexes match the parts of the assembled message: text fragments of
// one part share its index, and each other part takes the next.
func TestStreamDeltaIndexesMatchMessageParts(t *testing.T) {
	result, deltas, err := readTestStream(
		t,
		`{"responseId":"r1","candidates":[{"content":{"parts":[{"text":"Hel"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"lo"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"search","args":{}}}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	var indexes []int
	for _, d := range deltas {
		if d.Kind != llm.DeltaUsage {
			indexes = append(indexes, d.Index)
		}
	}
	if len(result.Message.Content) != 3 ||
		len(indexes) != 4 ||
		indexes[0] != 0 || indexes[1] != 0 || indexes[2] != 1 || indexes[3] != 2 {
		t.Fatalf("indexes = %v, parts = %d", indexes, len(result.Message.Content))
	}
}

// An empty stream invents no message provenance or served model.
func TestStreamEmptyHasNoProvenance(t *testing.T) {
	result, _, err := readTestStream(t)
	if err == nil {
		t.Fatal("empty stream succeeded")
	}
	if result.Message.Source != nil || result.Served != (llm.ModelRef{}) {
		t.Fatalf("empty stream invented provenance: %+v", result)
	}
}
