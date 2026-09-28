package gemini_test

import (
	"testing"

	"github.com/deepnoodle-ai/kernel/internal/providertest"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/google/gemini"
	"github.com/deepnoodle-ai/kernel/tool"
)

// These are the Gemini cases of the cross-provider contract tests in the
// root module's providers package. They live here because the root module
// does not depend on this one.

func TestReminderPreparedWireAndEvidence(t *testing.T) {
	client, err := gemini.New(gemini.Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	providertest.ReminderRendered(t, client, llm.RoleUser)
}

func TestHostedConstructorsMarkOnlyKnownReadOnlyTools(t *testing.T) {
	tests := []struct {
		name     string
		makeTool func() (tool.Definition, error)
		readOnly bool
	}{
		{
			name: "search",
			makeTool: func() (tool.Definition, error) {
				return gemini.NewGoogleSearchTool(""), nil
			},
			readOnly: true,
		},
		{
			name: "URL context",
			makeTool: func() (tool.Definition, error) {
				return gemini.NewURLContextTool(""), nil
			},
			readOnly: true,
		},
		{
			name: "file search",
			makeTool: func() (tool.Definition, error) {
				return gemini.NewFileSearchTool(
					"",
					gemini.FileSearchOptions{StoreNames: []string{"store"}},
				)
			},
			readOnly: true,
		},
		{
			name: "code execution",
			makeTool: func() (tool.Definition, error) {
				return gemini.NewCodeExecutionTool(""), nil
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			definition, err := tc.makeTool()
			if err != nil {
				t.Fatal(err)
			}
			got := definition.Annotations.ReadOnly != nil &&
				*definition.Annotations.ReadOnly
			if got != tc.readOnly {
				t.Fatalf("read only=%t; want %t", got, tc.readOnly)
			}
		})
	}
}

func TestAgentFinalFactsAgreeAcrossTransports(t *testing.T) {
	const response = `{"responseId":"r1","modelVersion":"served","candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`
	providertest.FinalFactsAgree(t, providertest.TransportFixture{
		Complete: response,
		Stream:   providertest.Events(response),
		New: func(url string) llm.Client {
			client, err := gemini.New(gemini.Config{
				Provider:         "google",
				APIKey:           "test",
				Endpoint:         url + "/v1/models/{model}:generateContent",
				CapabilityPolicy: llm.CapabilityAdjust,
			})
			if err != nil {
				t.Fatal(err)
			}
			return client
		},
	})
}

func TestPublicEndpointUsesCatalogByDefault(t *testing.T) {
	client, err := gemini.New(gemini.Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	providertest.CatalogDefaults(t, client, "gemini-3.8-flash")
}

func TestEveryCatalogModelPreparesPlainRequest(t *testing.T) {
	client, err := gemini.New(gemini.Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	providertest.CatalogModelsPrepare(t, "google", client)
}

func TestVertexGetsNoCatalog(t *testing.T) {
	vertex, err := gemini.New(gemini.Config{
		Vertex:          true,
		Project:         "p",
		Location:        "us-central1",
		CredentialScope: "service-account",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vertex.ModelInfo("gemini-3.8-flash"); ok {
		t.Fatal("catalog facts applied to an undocumented destination")
	}
}
