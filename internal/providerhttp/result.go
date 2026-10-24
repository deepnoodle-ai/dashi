package providerhttp

import "github.com/deepnoodle-ai/dashi/llm"

// NotSentResult is the Result a client returns with an error raised before
// dispatch. Like any failed call's Result, it names the requested model and
// repeats the prepared request's warnings.
func NotSentResult(prepared llm.Request) llm.Result {
	return llm.Result{
		Requested: llm.ModelRef{
			Provider: prepared.Origin().Provider,
			Model:    prepared.Input().Model,
		},
		Message:  llm.Message{Role: llm.RoleAssistant},
		Warnings: prepared.Warnings(),
	}
}
