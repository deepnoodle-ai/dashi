// Ported from Dive (65c393f), Apache-2.0; adapted for Dashi.

package llm

import "testing"

func TestClassifyStopReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   StopKind
	}{
		{name: "end_turn", reason: "end_turn", want: StopFinished},
		{name: "stop", reason: "stop", want: StopFinished},
		{
			name:   "stop_sequence",
			reason: "stop_sequence",
			want:   StopFinished,
		},
		{name: "tool_use", reason: "tool_use", want: StopToolUse},
		{name: "tool_calls", reason: "tool_calls", want: StopToolUse},
		{
			name:   "max_tokens",
			reason: "max_tokens",
			want:   StopOutputLimit,
		},
		{name: "length", reason: "length", want: StopOutputLimit},
		{
			name:   "model_length",
			reason: "model_length",
			want:   StopContextLimit,
		},
		{
			name:   "model_context_window_exceeded",
			reason: "model_context_window_exceeded",
			want:   StopContextLimit,
		},
		{name: "refusal", reason: "refusal", want: StopRefusal},
		{
			name:   "content_filter",
			reason: "content_filter",
			want:   StopRefusal,
		},
		{name: "safety", reason: "safety", want: StopRefusal},
		{name: "SAFETY", reason: "SAFETY", want: StopRefusal},
		{name: "pause_turn", reason: "pause_turn", want: StopPause},
		{
			name:   "incomplete",
			reason: "incomplete",
			want:   StopIncomplete,
		},
		{name: "cancelled", reason: "cancelled", want: StopIncomplete},
		{
			name:   "unexpected_tool_call",
			reason: "unexpected_tool_call",
			want:   StopIncomplete,
		},
		{
			name:   "too_many_tool_calls",
			reason: "too_many_tool_calls",
			want:   StopIncomplete,
		},
		{name: "other", reason: "other", want: StopOther},
		{name: "language", reason: "language", want: StopOther},
		{
			name:   "some_new_reason",
			reason: "some_new_reason",
			want:   StopOther,
		},
		{name: "empty", reason: "", want: StopOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyStopReason(tt.reason); got != tt.want {
				t.Fatalf(
					"stop kind for %q = %s, want %s",
					tt.reason,
					got,
					tt.want,
				)
			}
		})
	}
}
