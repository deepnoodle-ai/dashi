package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestPrepareClampsTemperatureUnderAdjust(t *testing.T) {
	var dispatched atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			dispatched.Add(1)
		},
	))
	defer server.Close()
	client, err := New(Config{
		Provider: "google",
		APIKey:   "test",
		Endpoint: server.URL + "/v1beta/models/{model}:generateContent",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput(false)
	temperature := 2.1
	input.Settings.Temperature = &temperature
	request, err := client.Prepare(context.Background(), input)
	if err != nil {
		t.Fatalf("default policy rejected a fittable temperature: %v", err)
	}
	if got := *request.Effective().Temperature; got != 2 {
		t.Fatalf("temperature = %g, want 2", got)
	}
	if !bytes.Contains(request.Body(), []byte(`"temperature":2`)) {
		t.Fatalf("body lacks the clamped temperature: %s", request.Body())
	}
	warnings := request.Warnings()
	if len(warnings) != 1 || warnings[0].Code != llm.WarningSettingClamped {
		t.Fatalf("clamp warnings = %+v", warnings)
	}
	raw, err := json.Marshal(request.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"capability_policy":"adjust"`)) {
		t.Fatalf("snapshot omitted the resolved policy: %s", raw)
	}
	if dispatched.Load() != 0 {
		t.Fatal("Prepare dispatched a request")
	}
}

func TestPrepareRejectsTemperatureUnderStrict(t *testing.T) {
	var dispatched atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			dispatched.Add(1)
		},
	))
	defer server.Close()
	client, err := New(Config{
		Provider:         "google",
		APIKey:           "test",
		Endpoint:         server.URL + "/v1beta/models/{model}:generateContent",
		CapabilityPolicy: llm.CapabilityStrict,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput(false)
	temperature := 2.1
	input.Settings.Temperature = &temperature
	_, err = client.Prepare(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "temperature") {
		t.Fatalf("strict policy did not reject temperature above 2: %v", err)
	}
	if dispatched.Load() != 0 {
		t.Fatal("a rejected preparation dispatched a request")
	}
}
