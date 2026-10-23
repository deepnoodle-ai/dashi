package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Gemini has no task budget, so the setting is omitted with a warning.
func TestTaskBudgetOmitted(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	input := sampleInput(false)
	input.Settings.TaskBudget = &llm.TaskBudget{Total: 40_000}
	prepared, err := newTestClient(t, server).Prepare(t.Context(), input)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	warnings := prepared.Warnings()
	if len(warnings) != 1 || warnings[0].Field != llm.SettingTaskBudget ||
		warnings[0].Code != llm.WarningSettingOmitted {
		t.Fatalf("warnings = %+v", warnings)
	}
	if strings.Contains(string(prepared.Body()), "budget") {
		t.Fatalf("body = %s", prepared.Body())
	}
}
