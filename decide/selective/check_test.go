package selective

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestCheckPairing(t *testing.T) {
	input := []llm.Message{user("go"), call("c1", "x"), result("c1", "ok"), call("c2", "y")}
	tests := []struct {
		name string
		keep []llm.Message
		want string
	}{
		{"whole", input, ""},
		{"exchange dropped", input[:1], ""},
		{"unanswered call kept", []llm.Message{input[3]}, ""},
		{"result without call", []llm.Message{input[0], input[2]}, "before its call"},
		{"result before call", []llm.Message{input[2], input[1]}, "before its call"},
		{"call without result", []llm.Message{input[0], input[1]}, "lose its result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPairing(input, tt.keep)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
