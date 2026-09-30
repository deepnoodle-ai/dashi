package responses

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
)

// This keyless transport is fixture evidence, not a live qualification.
func summaryExampleClient(support llm.Support, policy llm.CapabilityPolicy) *Client {
	endpoint := "https://fixture.test/v1/responses"
	transport := summaryTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"response","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"public summary"}]}]}`)), Request: r}, nil
	})
	client, err := New(Config{Provider: "fixture", Endpoint: endpoint, Client: &http.Client{Transport: transport}, Models: map[string]llm.ModelInfo{"fixture": summaryModel(endpoint, support)}, CapabilityPolicy: policy})
	if err != nil {
		panic(err)
	}
	return client
}

func ExampleClient_Prepare_reasoningSummary() {
	ctx := context.Background()
	client := summaryExampleClient(llm.SupportSupported, llm.CapabilityAdjust)
	input := llm.Input{Model: "fixture", Messages: []llm.Message{llm.UserMessage(llm.Text("Explain the result."))}, Settings: llm.Settings{ReasoningSummary: new(true)}}
	request, err := client.Prepare(ctx, input)
	if err != nil {
		panic(err)
	}
	fmt.Println("requested:", *request.Effective().ReasoningSummary)
	result, err := client.Send(ctx, llm.Call{Request: request})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Message.Content[0].Reasoning.Text)
	unsupported := summaryExampleClient(llm.SupportUnsupported, llm.CapabilityAdjust)
	omitted, err := unsupported.Prepare(ctx, input)
	if err != nil {
		panic(err)
	}
	fmt.Println("omitted:", omitted.Effective().ReasoningSummary == nil)
	strict := summaryExampleClient(llm.SupportUnsupported, llm.CapabilityStrict)
	_, err = strict.Prepare(ctx, input)
	fmt.Println("strict rejected:", err != nil)
	// Output:
	// requested: true
	// public summary
	// omitted: true
	// strict rejected: true
}
