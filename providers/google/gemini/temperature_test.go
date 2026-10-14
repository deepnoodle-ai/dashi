package gemini

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
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
	input.Settings.Temperature = new(2.1)
	request, err := client.Prepare(t.Context(), input)
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
	input.Settings.Temperature = new(2.1)
	_, err = client.Prepare(t.Context(), input)
	if err == nil || !strings.Contains(err.Error(), "temperature") {
		t.Fatalf("strict policy did not reject temperature above 2: %v", err)
	}
	if dispatched.Load() != 0 {
		t.Fatal("a rejected preparation dispatched a request")
	}
}

// A model known only from the built-in catalog is fitted to the catalog's
// limits, as a model supplied in Config.Models is.
func TestPrepareFitsCatalogModel(t *testing.T) {
	client, err := New(Config{APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.ModelInfo("gemini-4-argon"); !ok {
		t.Fatal("catalog lacks gemini-4-argon")
	}
	input := sampleInput(false)
	input.Model = "gemini-4-argon"
	input.Settings.Temperature = new(0.5)
	input.Settings.MaxTokens = 100_000
	request, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatalf("default policy rejected fittable settings: %v", err)
	}
	effective := request.Effective()
	if effective.Temperature != nil {
		t.Fatalf("temperature = %g, want it removed", *effective.Temperature)
	}
	if effective.MaxTokens != 65_536 {
		t.Fatalf("max tokens = %d, want 65536", effective.MaxTokens)
	}
	if bytes.Contains(request.Body(), []byte(`"temperature"`)) {
		t.Fatalf("body kept an unsupported temperature: %s", request.Body())
	}
	if len(request.Warnings()) != 2 {
		t.Fatalf("warnings = %+v, want one per fitted setting", request.Warnings())
	}
}
