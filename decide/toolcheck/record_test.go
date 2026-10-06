package toolcheck_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/toolcheck"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// ReadResultRecord reads back each kind of record the PromptInjection hook
// writes, so a host draws a reopened turn as the live one.
func TestReadResultRecord(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		setup func(*fixture)
		want  toolcheck.ResultRecord
	}{
		{"passed", page, func(f *fixture) {
			f.srv.Answer("injection", decidetest.NoulAnswer(0.03))
			f.srv.Answer("hidden", decidetest.NoulAnswer(0.02))
		}, toolcheck.ResultRecord{Checked: true, Model: "jev-1.13.0", RequestID: "req_1",
			Probabilities: map[string]float64{toolcheck.QuestionInjection: 0.03, toolcheck.QuestionHidden: 0.02}}},
		{"flagged", attack, func(f *fixture) {
			f.srv.Answer("injection", decidetest.NoulAnswer(0.96))
			f.srv.Answer("hidden", decidetest.NoulAnswer(0.03))
		}, toolcheck.ResultRecord{Checked: true, Flagged: true, Warned: true, Model: "jev-1.13.0", RequestID: "req_1",
			Probabilities: map[string]float64{toolcheck.QuestionInjection: 0.96, toolcheck.QuestionHidden: 0.03}}},
		{"partial", strings.Repeat("plain text\n", 100_000), func(f *fixture) {
			f.srv.Answer("injection", decidetest.NoulAnswer(0.04))
			f.srv.Answer("hidden", decidetest.NoulAnswer(0.01))
		}, toolcheck.ResultRecord{Checked: true, Partial: true, Warned: true, Model: "jev-1.13.0", RequestID: "req_1",
			Probabilities: map[string]float64{toolcheck.QuestionInjection: 0.04, toolcheck.QuestionHidden: 0.01}}},
		{"rate limited", attack, func(f *fixture) {
			f.srv.FailNext(http.StatusTooManyRequests)
		}, toolcheck.ResultRecord{Warned: true, Cause: toolcheck.CauseRateLimited}},
		{"outage", attack, func(f *fixture) {
			f.srv.FailNext(http.StatusInternalServerError)
		}, toolcheck.ResultRecord{Cause: toolcheck.CauseServerError}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			out, _ := run(t, tool.List{fetchTool(tc.text)},
				toolcheck.PromptInjection{Client: f.client}.Hooks(),
				dashitest.CallTool("call-1", "fetch_page", `{}`),
				dashitest.Reply("done"),
			)
			results := hookResults(out, toolcheck.PromptInjectionName)
			if len(results) != 1 {
				t.Fatalf("hook results = %+v", results)
			}
			got, ok := toolcheck.ReadResultRecord(results[0])
			if !ok {
				t.Fatalf("could not read %+v", results[0])
			}
			if tc.want.RequestID != "" {
				// The fake server numbers requests; a long text sends several.
				if got.RequestID == "" {
					t.Fatal("no request ID")
				}
				tc.want.RequestID = got.RequestID
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("record = %+v\nwant %+v", got, tc.want)
			}
		})
	}
	if _, ok := toolcheck.ReadResultRecord(dashi.HookResult{Name: "other", Verdict: dashi.VerdictReplace,
		Reason: "flagged: injection 96%, hidden 3% (m, request r)"}); ok {
		t.Fatal("read another hook's result")
	}
}
