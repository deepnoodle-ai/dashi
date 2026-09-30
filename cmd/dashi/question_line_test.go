package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/workspace"
)

func TestLineMultipleQuestionRepromptsAndPreservesExactLabels(t *testing.T) {
	for _, tc := range []struct{ answer, want string }{{"1,2", `{"selections":["a,b","c"]}`}, {"other: my answer", `{"text":"my answer"}`}} {
		t.Run(tc.answer, func(t *testing.T) {
			cfg := testConfig(t, callTool("ask", workspace.ToolAskUser, `{"question":"Which?","multiple":true,"choices":[{"label":"a,b"},{"label":"c"}]}`), say("Done."))
			var output bytes.Buffer
			ui := newLineUI(&output)
			a, err := buildApp(t.Context(), cfg, ui)
			if err != nil {
				t.Fatal(err)
			}
			defer a.eng.Close()
			ui.ctrl = a.ctrl
			if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
				t.Fatal(err)
			}
			ui.SetLive()
			if err = ui.serveLines(strings.NewReader("ask\n1,1\n"+tc.answer+"\n"), true); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "distinct") {
				t.Fatalf("invalid answer was not rejected: %s", output.String())
			}
			h := &harness{t: t, app: a, store: cfg.Store}
			var answer string
			for _, step := range h.turn(1).Steps {
				if step.Tool != nil && step.Tool.Call.ID == "ask" {
					answer = outputText(step.Tool.Outcome)
				}
			}
			if answer != tc.want {
				t.Fatalf("model answer %q, want %q", answer, tc.want)
			}
		})
	}
}
