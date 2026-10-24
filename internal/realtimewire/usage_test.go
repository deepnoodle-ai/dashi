package realtimewire

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/realtime"
)

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// voicePricing rates audio apart from text, as mobius-cloud does, so an
// estimate exists only when every token has a modality.
var voicePricing = llm.Pricing{
	Currency: "USD", UpdatedAt: "2026-10-23",
	Base:     llm.Rates{Input: money.MustParseAmount("0.5"), Output: money.MustParseAmount("2"), CacheRead: money.MustParseAmount("0.5"), CacheWrite: money.MustParseAmount("0.5")},
	Modality: map[string]llm.Rates{"audio": {Input: money.MustParseAmount("3"), Output: money.MustParseAmount("12"), CacheRead: money.MustParseAmount("0.5"), CacheWrite: money.MustParseAmount("3")}},
}

// A live Grok response.done, output trimmed: usage sits beside the response.
const grokDone = `{"type":"response.done","event_id":"6f9e","response":{"id":"7b8d22cf","object":"realtime.response","output":[],"status":"completed","status_details":"unimplemented","usage":{}},"response_id":"7b8d22cf","usage":{"input_tokens":3,"input_token_details":{"text_tokens":3,"audio_tokens":0,"grok_tokens":0},"output_tokens":133,"output_token_details":{"text_tokens":9,"audio_tokens":124,"grok_tokens":0},"total_tokens":136,"output_audio_seconds":2.48,"billable_audio_seconds":1},"previous_item_id":null}`

func TestGrokUsageBesideResponse(t *testing.T) {
	s, _ := connect(t, false, fake(`{"type":"session.updated"}`, `{"type":"response.created","response":{"id":"7b8d22cf"}}`, grokDone))
	next(t, s, realtime.EventResponseStarted)
	u := next(t, s, realtime.EventUsage).Usage
	want := &llm.Usage{InputTokens: 3, OutputTokens: 133, ModalityTokens: map[string]llm.ModalityTokenUsage{"text": {InputTokens: 3, OutputTokens: 9}, "audio": {OutputTokens: 124}}}
	if !reflect.DeepEqual(u, want) {
		t.Fatalf("usage %+v", u)
	}
	if _, ok := voicePricing.Estimate(u); !ok {
		t.Fatal("no estimate")
	}
	next(t, s, realtime.EventResponseCompleted)
}
func TestGrokUsage(t *testing.T) {
	for _, c := range []struct {
		name, raw string
		want      *llm.Usage
	}{
		{"grok tokens keep their own modality", `{"input_tokens":5,"input_token_details":{"text_tokens":3,"grok_tokens":2},"output_tokens":4,"output_token_details":{"audio_tokens":4}}`,
			&llm.Usage{InputTokens: 5, OutputTokens: 4, ModalityTokens: map[string]llm.ModalityTokenUsage{"text": {InputTokens: 3}, "grok": {InputTokens: 2}, "audio": {OutputTokens: 4}}}},
		{"details short of the total", `{"input_tokens":5,"input_token_details":{"text_tokens":3},"output_tokens":4,"output_token_details":{"audio_tokens":4}}`,
			&llm.Usage{InputTokens: 5, OutputTokens: 4, ModalityTokens: map[string]llm.ModalityTokenUsage{"text": {InputTokens: 3}, "audio": {OutputTokens: 4}}, InputModalityTokenDetailsIncomplete: true}},
		{"details over the total are dropped", `{"input_tokens":2,"input_token_details":{"text_tokens":3},"output_tokens":4,"output_token_details":{"audio_tokens":4}}`,
			&llm.Usage{InputTokens: 2, OutputTokens: 4, InputModalityTokenDetailsIncomplete: true, OutputModalityTokenDetailsIncomplete: true}},
		{"no details (OpenAI style)", `{"input_tokens":12,"output_tokens":8}`,
			&llm.Usage{InputTokens: 12, OutputTokens: 8, InputModalityTokenDetailsIncomplete: true, OutputModalityTokenDetailsIncomplete: true}},
		{"no totals", `{}`, nil},
	} {
		u, err := grokUsage(decode(t, c.raw))
		if err != nil || !reflect.DeepEqual(u, c.want) {
			t.Errorf("%s: usage %+v error %v", c.name, u, err)
		}
	}
}

func TestGoogleUsage(t *testing.T) {
	for _, c := range []struct {
		name, raw string
		want      *llm.Usage
	}{
		{"Vertex text turn with thoughts", `{"candidatesTokenCount":15,"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":15}],"promptTokenCount":2243,"promptTokensDetails":[{"modality":"TEXT","tokenCount":2243}],"thoughtsTokenCount":85,"totalTokenCount":2343}`,
			&llm.Usage{InputTokens: 2243, OutputTokens: 100, ReasoningTokens: 85, CacheCreationInputTokensUnavailable: true, ModalityTokens: map[string]llm.ModalityTokenUsage{"text": {InputTokens: 2243, OutputTokens: 100}}}},
		{"Developer API audio turn", `{"promptTokenCount":120,"promptTokensDetails":[{"modality":"TEXT","tokenCount":100},{"modality":"AUDIO","tokenCount":20}],"responseTokenCount":60,"responseTokensDetails":[{"modality":"AUDIO","tokenCount":60}],"totalTokenCount":180}`,
			&llm.Usage{InputTokens: 120, OutputTokens: 60, CacheCreationInputTokensUnavailable: true, ModalityTokens: map[string]llm.ModalityTokenUsage{"text": {InputTokens: 100}, "audio": {InputTokens: 20, OutputTokens: 60}}}},
		{"cached prompt", `{"promptTokenCount":120,"promptTokensDetails":[{"modality":"TEXT","tokenCount":100},{"modality":"AUDIO","tokenCount":20}],"cachedContentTokenCount":80,"cacheTokensDetails":[{"modality":"TEXT","tokenCount":80}],"responseTokenCount":6,"responseTokensDetails":[{"modality":"AUDIO","tokenCount":6}]}`,
			&llm.Usage{InputTokens: 40, CacheReadInputTokens: 80, OutputTokens: 6, CacheCreationInputTokensUnavailable: true, ModalityTokens: map[string]llm.ModalityTokenUsage{"text": {InputTokens: 20, CacheReadInputTokens: 80}, "audio": {InputTokens: 20, OutputTokens: 6}}}},
		{"no details", `{"promptTokenCount":36,"responseTokenCount":56}`,
			&llm.Usage{InputTokens: 36, OutputTokens: 56, CacheCreationInputTokensUnavailable: true, InputModalityTokenDetailsIncomplete: true, OutputModalityTokenDetailsIncomplete: true}},
		{"total mismatch", `{"promptTokenCount":3,"responseTokenCount":4,"totalTokenCount":9}`,
			&llm.Usage{InputTokens: 3, OutputTokens: 4, CacheCreationInputTokensUnavailable: true, TotalTokenCountMismatch: true, InputModalityTokenDetailsIncomplete: true, OutputModalityTokenDetailsIncomplete: true}},
		{"no output count", `{"promptTokenCount":3}`, nil},
	} {
		u, err := googleUsage(decode(t, c.raw))
		if err != nil || !reflect.DeepEqual(u, c.want) {
			t.Errorf("%s: usage %+v error %v", c.name, u, err)
		}
	}
}

// Thoughts are output, so the realtime and batch Gemini adapters price a
// turn alike, and the estimate needs no unattributed tokens.
func TestGoogleUsagePricesThoughtsAsTextOutput(t *testing.T) {
	u, err := googleUsage(decode(t, `{"candidatesTokenCount":15,"candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":15}],"promptTokenCount":10,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":10}],"thoughtsTokenCount":5}`))
	if err != nil {
		t.Fatal(err)
	}
	cost, ok := voicePricing.Estimate(u)
	// 10 audio in at $3, 15 audio out at $12, 5 thought tokens out at $2 (per MTok).
	if want := money.MustParseAmount("0.00022"); !ok || cost.Total != want {
		t.Fatalf("cost %+v ok %v", cost, ok)
	}
}

func TestUsageRejectsImpossibleCounts(t *testing.T) {
	if _, err := googleUsage(decode(t, `{"promptTokenCount":-1,"responseTokenCount":1}`)); err == nil {
		t.Fatal("negative accepted")
	}
	if _, err := grokUsage(decode(t, `{"input_tokens":1,"output_tokens":1,"output_token_details":{"audio_tokens":2e9}}`)); err == nil {
		t.Fatal("huge detail accepted")
	}
}
