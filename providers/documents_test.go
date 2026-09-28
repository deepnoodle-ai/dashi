package providers_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

// wireBody decodes a prepared request's exact body.
func wireBody(t *testing.T, r llm.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(r.Body(), &body); err != nil {
		t.Fatalf("decode prepared body: %v", err)
	}
	return body
}

// wireDocuments returns every document block in a Messages body, in order,
// including those inside tool results.
func wireDocuments(body map[string]any) []map[string]any {
	var docs []map[string]any
	var walk func(blocks []any)
	walk = func(blocks []any) {
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			switch block["type"] {
			case "document":
				docs = append(docs, block)
			case "tool_result":
				inner, _ := block["content"].([]any)
				walk(inner)
			}
		}
	}
	for _, m := range body["messages"].([]any) {
		blocks, _ := m.(map[string]any)["content"].([]any)
		walk(blocks)
	}
	return docs
}

func anthropicClient(t *testing.T, url string, policy llm.CapabilityPolicy) *messages.Client {
	t.Helper()
	return newMessages(t, messages.Config{
		Provider:         "anthropic",
		CapabilityPolicy: policy,
		APIKey:           "test",
		Endpoint:         url,
	})
}

// documentInput carries a user document and a second document returned by a
// tool, so citations must reach both.
func documentInput() llm.Input {
	r := modelInput()
	r.Messages[0].Content = append(
		r.Messages[0].Content,
		llm.Document(content.Source{Data: []byte("The sky is blue.")}, "sky.txt"),
	)
	r.Messages = append(
		r.Messages,
		llm.AssistantMessage(llm.Content{
			Kind: llm.ContentToolCall,
			ToolCall: &tool.Call{
				ID:   "call-1",
				Name: "read",
				Args: json.RawMessage(`{}`),
			},
		}),
		llm.Message{
			Role: llm.RoleUser,
			Content: []llm.Content{{
				Kind: llm.ContentToolResult,
				ToolResult: &llm.ToolResult{
					CallID: "call-1",
					Output: tool.Output{Content: []content.Block{
						content.DocumentBlock(content.Source{
							Data:     []byte("%PDF-1.4 fixture"),
							MIMEType: "application/pdf",
						}, "report.pdf"),
					}},
				},
			}},
		},
	)
	return r
}

func TestMessagesEncodesTextAndPDFDocumentsWithCitations(t *testing.T) {
	p := anthropicClient(t, "http://unused.invalid", llm.CapabilityAdjust)
	r := documentInput()
	r.Settings.Citations = true
	prepared, err := p.Prepare(t.Context(), r)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	docs := wireDocuments(wireBody(t, prepared))
	if len(docs) != 2 {
		t.Fatalf("documents = %d, want 2", len(docs))
	}
	text := docs[0]["source"].(map[string]any)
	if text["type"] != "text" ||
		text["media_type"] != "text/plain" ||
		text["data"] != "The sky is blue." {
		t.Fatalf("text source = %v", text)
	}
	pdf := docs[1]["source"].(map[string]any)
	if pdf["type"] != "base64" || pdf["media_type"] != "application/pdf" {
		t.Fatalf("pdf source = %v", pdf)
	}
	for i, doc := range docs {
		if mustJSON(doc["citations"]) != `{"enabled":true}` {
			t.Fatalf("document %d citations = %v", i, doc["citations"])
		}
	}
	if len(prepared.Warnings()) != 0 {
		t.Fatalf("warnings = %v", prepared.Warnings())
	}
	// The text document had no MIME type; detection is recorded, and the
	// caller's input keeps the empty value.
	var detected bool
	for _, c := range prepared.ContentChanges() {
		detected = detected || c.Action == llm.ContentChangeDetectMIMEType &&
			c.MessageIndex == 0 &&
			c.ContentIndex == 1 &&
			strings.Contains(c.Reason, "text/plain")
	}
	if !detected {
		t.Fatalf("changes = %+v", prepared.ContentChanges())
	}
	source := prepared.Input().Messages[0].Content[1].Block.Document.Source
	if source.MIMEType != "" {
		t.Fatal("preparation rewrote the caller's MIME type")
	}
}

func TestMessagesSendsTextLikeDocumentsAsPlainText(t *testing.T) {
	for _, tc := range []struct{ title, data string }{
		{title: "notes.md", data: "# Notes"},
		{title: "table.csv", data: "a,b\n1,2"},
		{title: "data.json", data: `{"a":1}`},
	} {
		t.Run(tc.title, func(t *testing.T) {
			p := anthropicClient(t, "http://unused.invalid", llm.CapabilityAdjust)
			r := modelInput()
			r.Messages[0].Content = append(
				r.Messages[0].Content,
				llm.Document(content.Source{Data: []byte(tc.data)}, tc.title),
			)
			prepared, err := p.Prepare(t.Context(), r)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			source := wireDocuments(wireBody(t, prepared))[0]["source"].(map[string]any)
			if source["type"] != "text" ||
				source["media_type"] != "text/plain" ||
				source["data"] != tc.data {
				t.Fatalf("source = %v", source)
			}
		})
	}
}

func TestMessagesOmitsCitationsWhenNotRequested(t *testing.T) {
	p := anthropicClient(t, "http://unused.invalid", llm.CapabilityAdjust)
	prepared, err := p.Prepare(t.Context(), documentInput())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, doc := range wireDocuments(wireBody(t, prepared)) {
		if _, ok := doc["citations"]; ok {
			t.Fatalf("unrequested citations: %v", doc)
		}
	}
}

func TestMessagesRejectsUnsupportedMediaAtPrepare(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block llm.Content
	}{
		{
			name: "binary document",
			block: llm.Document(content.Source{
				Data:     []byte{0x50, 0x4b, 0x03, 0x04},
				MIMEType: "application/zip",
			}, ""),
		},
		{
			name:  "unidentified document",
			block: llm.Document(content.Source{Data: []byte{0, 1, 2}}, ""),
		},
		{
			name: "invalid UTF-8 text",
			block: llm.Document(content.Source{
				Data:     []byte{0xff, 0xfe},
				MIMEType: "text/plain",
			}, ""),
		},
		{
			name: "non-PDF URL",
			block: llm.Document(content.Source{
				URL:      "https://example.com/a.docx",
				MIMEType: "application/msword",
			}, ""),
		},
		{
			name: "unsupported image",
			block: llm.Image(content.Source{
				Data:     []byte{1},
				MIMEType: "image/tiff",
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := anthropicClient(t, "http://unused.invalid", llm.CapabilityAdjust)
			r := modelInput()
			r.Messages[0].Content = append(r.Messages[0].Content, tc.block)
			_, err := p.Prepare(t.Context(), r)
			e, ok := errors.AsType[*llm.Error](err)
			if !ok || e.Category != llm.ErrorUnsupported {
				t.Fatalf("err = %v, want unsupported", err)
			}
		})
	}
}

func TestOtherAPIsOmitCitationsWithWarning(t *testing.T) {
	clients := map[string]func(llm.CapabilityPolicy) llm.Client{
		"responses": func(policy llm.CapabilityPolicy) llm.Client {
			return newResponses(t, responses.Config{
				Provider:         "openai",
				CapabilityPolicy: policy,
				APIKey:           "test",
				Endpoint:         "http://unused.invalid",
			})
		},
		"completions": func(policy llm.CapabilityPolicy) llm.Client {
			p, err := completions.New(completions.Config{
				Provider:         "openai",
				CapabilityPolicy: policy,
				APIKey:           "test",
				Endpoint:         "http://unused.invalid",
			})
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
	}
	for name, newClient := range clients {
		t.Run(name, func(t *testing.T) {
			r := modelInput()
			r.Settings.Citations = true
			r.Messages[0].Content = append(
				r.Messages[0].Content,
				llm.Document(content.Source{
					Data:     []byte("%PDF-1.4"),
					MIMEType: "application/pdf",
				}, "a.pdf"),
			)
			prepared, err := newClient(llm.CapabilityAdjust).Prepare(t.Context(), r)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			warnings := prepared.Warnings()
			if len(warnings) != 1 ||
				warnings[0].Code != llm.WarningSettingOmitted ||
				warnings[0].Field != llm.SettingCitations {
				t.Fatalf("warnings = %+v", warnings)
			}
			if prepared.Snapshot().Effective.Citations {
				t.Fatal("effective settings kept citations")
			}

			// Without a document the setting has no effect and no warning.
			plain := modelInput()
			plain.Settings.Citations = true
			prepared, err = newClient(llm.CapabilityAdjust).Prepare(t.Context(), plain)
			if err != nil || len(prepared.Warnings()) != 0 {
				t.Fatalf("no-document request: err=%v warnings=%v", err, prepared.Warnings())
			}

			_, err = newClient(llm.CapabilityStrict).Prepare(t.Context(), r)
			if e, ok := errors.AsType[*llm.Error](err); !ok ||
				e.Category != llm.ErrorUnsupported {
				t.Fatalf("strict err = %v, want unsupported", err)
			}
		})
	}
}

func TestUnknownMIMETypeWarns(t *testing.T) {
	p := newResponses(t, responses.Config{
		Provider: "openai",
		APIKey:   "test",
		Endpoint: "http://unused.invalid",
	})
	r := modelInput()
	r.Messages = append(
		r.Messages,
		llm.AssistantMessage(llm.Content{
			Kind: llm.ContentToolCall,
			ToolCall: &tool.Call{
				ID:   "call-1",
				Name: "read",
				Args: json.RawMessage(`{}`),
			},
		}),
		llm.Message{
			Role: llm.RoleUser,
			Content: []llm.Content{{
				Kind: llm.ContentToolResult,
				ToolResult: &llm.ToolResult{
					CallID: "call-1",
					Output: tool.Output{Content: []content.Block{
						content.DocumentBlock(content.Source{
							Data: []byte("%PDF-1.4"),
						}, "a"),
						content.DocumentBlock(content.Source{
							Data: []byte{0, 1, 2},
						}, "blob"),
					}},
				},
			}},
		},
	)
	prepared, err := p.Prepare(t.Context(), r)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	var actions []llm.ContentChangeAction
	for _, c := range prepared.ContentChanges() {
		if c.MessageIndex == 2 {
			actions = append(actions, c.Action)
		}
	}
	if len(actions) != 2 ||
		actions[0] != llm.ContentChangeDetectMIMEType ||
		actions[1] != llm.ContentChangeUnknownMIMEType {
		t.Fatalf("tool result changes = %v", actions)
	}
	warnings := prepared.Warnings()
	if len(warnings) != 1 || warnings[0].Code != llm.WarningMIMETypeUnknown {
		t.Fatalf("warnings = %+v", warnings)
	}
	wire := string(prepared.Body())
	if !strings.Contains(wire, `"filename":"a.pdf"`) ||
		!strings.Contains(wire, "application/octet-stream") {
		t.Fatalf("body = %s", wire)
	}
}

func TestMessagesDecodesDocumentCitations(t *testing.T) {
	const response = `{"id":"msg-1","type":"message","model":"served",` +
		`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},` +
		`"content":[` +
		`{"type":"text","text":"The sky is blue.","citations":[` +
		`{"type":"char_location","cited_text":"The sky is blue.","document_index":0,` +
		`"document_title":"sky.txt","start_char_index":0,"end_char_index":16}]},` +
		`{"type":"text","text":" Risks rose.","citations":[` +
		`{"type":"page_location","cited_text":"Risks rose.","document_index":1,` +
		`"document_title":"report.pdf","start_page_number":2,"end_page_number":3},` +
		`{"type":"content_block_location","cited_text":"x","document_index":1,` +
		`"document_title":"report.pdf","start_block_index":0,"end_block_index":1},` +
		`{"type":"search_result_location","source":"https://example.com/r",` +
		`"title":"Result","cited_text":"y","search_result_index":0,` +
		`"start_block_index":0,"end_block_index":1}]}]}`
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(response))
	})
	p := anthropicClient(t, server.URL, llm.CapabilityAdjust)
	r := documentInput()
	r.Settings.Citations = true
	result, err := llm.Generate(t.Context(), p, r, nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got := result.Message.Text(); got != "The sky is blue. Risks rose." {
		t.Fatalf("text = %q", got)
	}
	char := result.Message.Content[0].Block.Text.Citations[0]
	if char.Title != "sky.txt" || char.Text != "The sky is blue." ||
		*char.Location != (content.CitationLocation{
			RequestDocumentIndex: 0,
			Unit:                 content.LocationChar,
			Start:                0,
			End:                  16,
		}) {
		t.Fatalf("char citation = %+v %+v", char, char.Location)
	}
	cites := result.Message.Content[1].Block.Text.Citations
	page := cites[0]
	if page.Title != "report.pdf" ||
		*page.Location != (content.CitationLocation{
			RequestDocumentIndex: 1,
			Unit:                 content.LocationPage,
			Start:                2,
			End:                  3,
		}) {
		t.Fatalf("page citation = %+v %+v", page, page.Location)
	}
	if block := cites[1]; block.Location != nil || len(block.Raw) == 0 {
		t.Fatalf("block citation = %+v", block)
	}
	if search := cites[2]; search.URL != "https://example.com/r" ||
		search.Title != "Result" || search.Location != nil {
		t.Fatalf("search result citation = %+v", search)
	}
}
