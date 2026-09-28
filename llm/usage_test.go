// Usage normalization is adapted from Dive (65c393f), Apache-2.0.

package llm

import (
	"encoding/json"
	"math"
	"testing"
)

func TestUsageUnmarshalReasoningTokens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    int
	}{
		{
			name: "anthropic thinking",
			payload: `{"input_tokens":25,"output_tokens":348,` +
				`"output_tokens_details":{"thinking_tokens":312}}`,
			want: 312,
		},
		{
			name: "openai reasoning",
			payload: `{"input_tokens":25,"output_tokens":348,` +
				`"output_tokens_details":{"reasoning_tokens":123}}`,
			want: 123,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage Usage
			if err := json.Unmarshal(
				[]byte(tc.payload),
				&usage,
			); err != nil {
				t.Fatal(err)
			}
			if usage.ReasoningTokens != tc.want ||
				usage.OutputTokens != 348 {
				t.Fatalf(
					"reasoning remained a subset of output: %+v",
					usage,
				)
			}
		})
	}
}

func TestUsageUnmarshalRejectsLegacyResultFields(t *testing.T) {
	for _, raw := range []string{
		`{"input_tokens":10,"cost":{"total":0.25}}`,
		`{"input_tokens":10,"cost_estimate_unavailable":true}`,
		`{"input_tokens":10,"service_tier":"priority"}`,
		`{"input_tokens":10,"speed":"fast"}`,
	} {
		var usage Usage
		if err := json.Unmarshal([]byte(raw), &usage); err == nil {
			t.Fatalf(
				"legacy result fact disappeared from usage: %s",
				raw,
			)
		}
	}
}

func TestUsageUnmarshalNativeInputDetails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		payload   string
		wantInput int
		wantRead  int
		wantWrite int
	}{
		{
			name:      "responses cache read",
			payload:   `{"input_tokens":100,"input_tokens_details":{"cached_tokens":70}}`,
			wantInput: 30,
			wantRead:  70,
		},
		{
			name:      "responses cache write",
			payload:   `{"input_tokens":100,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":70}}`,
			wantInput: 10,
			wantRead:  20,
			wantWrite: 70,
		},
		{
			name:      "completions cache read",
			payload:   `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":70}}`,
			wantInput: 30,
			wantRead:  70,
		},
		{
			name:      "completions cache write",
			payload:   `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":20,"cache_write_tokens":70}}`,
			wantInput: 10,
			wantRead:  20,
			wantWrite: 70,
		},
		{
			name: "canonical buckets win",
			payload: `{"input_tokens":40,"cache_read_input_tokens":10,` +
				`"input_tokens_details":{"cached_tokens":30}}`,
			wantInput: 40,
			wantRead:  10,
		},
		{
			name: "canonical zero wins",
			payload: `{"input_tokens":40,"cache_read_input_tokens":0,` +
				`"input_tokens_details":{"cached_tokens":30}}`,
			wantInput: 40,
		},
		{
			name:     "cached count clamps above prompt",
			payload:  `{"input_tokens":10,"input_tokens_details":{"cached_tokens":20}}`,
			wantRead: 10,
		},
		{
			name:      "writes clamp to remaining prompt",
			payload:   `{"input_tokens":10,"input_tokens_details":{"cached_tokens":7,"cache_write_tokens":8}}`,
			wantRead:  7,
			wantWrite: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage Usage
			if err := json.Unmarshal(
				[]byte(tc.payload),
				&usage,
			); err != nil {
				t.Fatal(err)
			}
			if usage.InputTokens != tc.wantInput ||
				usage.CacheReadInputTokens != tc.wantRead ||
				usage.CacheCreationInputTokens != tc.wantWrite {
				t.Fatalf(
					"wrong disjoint input buckets: %+v",
					usage,
				)
			}
		})
	}
}

func TestUsageCopyAndAddPreserveMeasurements(t *testing.T) {
	usage := &Usage{
		InputTokens:     10,
		OutputTokens:    20,
		ReasoningTokens: 3,
		ModalityTokens: map[string]ModalityTokenUsage{
			"text": {InputTokens: 10, OutputTokens: 20},
		},
	}
	if err := usage.Add(&Usage{
		InputTokens:                         5,
		OutputTokens:                        7,
		CacheReadInputTokens:                11,
		ReasoningTokens:                     4,
		TotalTokenCountMismatch:             true,
		InputModalityTokenDetailsIncomplete: true,
		ModalityTokens: map[string]ModalityTokenUsage{
			"text":  {InputTokens: 5, OutputTokens: 7},
			"audio": {CacheReadInputTokens: 11},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 15 || usage.OutputTokens != 27 ||
		usage.CacheReadInputTokens != 11 ||
		usage.ReasoningTokens != 7 ||
		usage.TotalInputTokens() != 26 {
		t.Fatalf("usage aggregation lost token counts: %+v", usage)
	}
	if !usage.TotalTokenCountMismatch ||
		!usage.InputModalityTokenDetailsIncomplete {
		t.Fatal("usage aggregation lost measurement quality flags")
	}
	copy := usage.Copy()
	copy.ModalityTokens["text"] = ModalityTokenUsage{InputTokens: 999}
	if usage.ModalityTokens["text"].InputTokens != 15 {
		t.Fatal("usage copy shared modality measurements")
	}
	encoded, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Usage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.TotalTokenCountMismatch ||
		decoded.TotalInputTokens() != 26 {
		t.Fatal("measurement quality or disjoint input lost in JSON")
	}
}

func TestUsageValidateRejectsOverflowingBuckets(t *testing.T) {
	tests := []struct {
		name  string
		usage Usage
	}{
		{
			name: "input buckets",
			usage: Usage{
				InputTokens:          math.MaxInt,
				CacheReadInputTokens: 1,
			},
		},
		{
			name: "total tokens",
			usage: Usage{
				InputTokens:  math.MaxInt,
				OutputTokens: 1,
			},
		},
		{
			name: "modality buckets",
			usage: Usage{
				ModalityTokens: map[string]ModalityTokenUsage{
					"audio": {
						InputTokens:  math.MaxInt,
						OutputTokens: 1,
					},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.usage.Validate(); err == nil {
				t.Fatal("overflowing usage was accepted")
			}
			if tc.name == "input buckets" &&
				tc.usage.TotalInputTokens() != math.MaxInt {
				t.Fatal("unvalidated input total wrapped")
			}
		})
	}
}

func TestUsageAddRejectsCumulativeOverflowWithoutMutation(t *testing.T) {
	tests := []struct {
		name  string
		start Usage
		add   Usage
	}{
		{
			name:  "one counter across calls",
			start: Usage{InputTokens: math.MaxInt - 1},
			add:   Usage{InputTokens: 2},
		},
		{
			name:  "disjoint counters across calls",
			start: Usage{InputTokens: math.MaxInt - 1},
			add:   Usage{OutputTokens: 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			usage := tc.start.Copy()
			if err := usage.Add(&tc.add); err == nil {
				t.Fatal("cumulative token overflow was accepted")
			}
			if usage.InputTokens != tc.start.InputTokens ||
				usage.OutputTokens != tc.start.OutputTokens {
				t.Fatal("failed addition changed the accumulated usage")
			}
		})
	}
}

func TestUsageAbsorbCumulativeFrames(t *testing.T) {
	usage := Usage{
		InputTokens:              14,
		CacheCreationInputTokens: 8012,
		CacheReadInputTokens:     120000,
		OutputTokens:             4,
		ModalityTokens: map[string]ModalityTokenUsage{
			"text": {InputTokens: 14, OutputTokens: 4},
		},
	}
	usage.Absorb(&Usage{
		OutputTokens:            7,
		TotalTokenCountMismatch: true,
		ModalityTokens: map[string]ModalityTokenUsage{
			"text": {OutputTokens: 7},
		},
	})
	if usage.TotalInputTokens() != 128026 || usage.OutputTokens != 7 ||
		usage.ModalityTokens["text"].InputTokens != 14 ||
		usage.ModalityTokens["text"].OutputTokens != 7 ||
		!usage.TotalTokenCountMismatch {
		t.Fatalf(
			"cumulative frame erased or summed fields: %+v",
			usage,
		)
	}
}
