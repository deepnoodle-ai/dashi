package gemini

import (
	"context"
	"fmt"
	"github.com/deepnoodle-ai/dashi/llm"
	"io"
	"net/http"
	"strings"
)

func ExampleNew_keylessDestination() {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		fmt.Println("API key sent:", r.Header.Get("X-Goog-Api-Key") != "")
		body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"fixture answer"}]},"finishReason":"STOP"}]}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	client, err := New(Config{Provider: "local", Endpoint: "http://127.0.0.1:8080/v1beta/models/{model}:generateContent", Client: &http.Client{Transport: transport}})
	if err != nil {
		panic(err)
	}
	request, err := client.Prepare(context.Background(), llm.Input{Model: "local/model", Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}})
	if err != nil {
		panic(err)
	}
	result, err := client.Send(context.Background(), llm.Call{Request: request})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Message.Text())
	// Output:
	// API key sent: false
	// fixture answer
}
