package tool

import (
	"encoding/json"
	"testing"
)

func TestCallValidatePreservesIDThroughJSON(t *testing.T) {
	tests := []struct {
		name  string
		id    string
		valid bool
	}{
		{name: "ASCII", id: "call-1", valid: true},
		{name: "Unicode", id: "工具:1", valid: true},
		{name: "invalid UTF-8", id: "call-\xff"},
		{name: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := Call{
				ID:   tt.id,
				Name: "act",
				Args: json.RawMessage(`{}`),
			}
			if err := call.Validate(); (err == nil) != tt.valid {
				t.Fatalf(
					"call ID validation disagrees with JSON safety: %v",
					err,
				)
			}
			if !tt.valid {
				return
			}
			data, err := json.Marshal(call)
			if err != nil {
				t.Fatal(err)
			}
			var restored Call
			if err := json.Unmarshal(
				data,
				&restored,
			); err != nil {
				t.Fatal(err)
			}
			if restored.ID != call.ID {
				t.Fatal("JSON changed the original call identity")
			}
		})
	}
}
