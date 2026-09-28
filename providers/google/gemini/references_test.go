package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
)

func TestPrepareEncodesMultipleInlineImages(t *testing.T) {
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := llm.Input{
		Model: "gemini-test",
		Messages: []llm.Message{
			llm.UserMessage(
				llm.Text("Describe both images."),
				llm.Block(content.ImageBlock(content.Source{
					Data:     []byte("first"),
					MIMEType: "image/png",
				})),
				llm.Block(content.ImageBlock(content.Source{
					Data:     []byte("second"),
					MIMEType: "image/jpeg",
				})),
			),
		},
	}
	prepared, err := client.Prepare(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Contents []struct {
			Parts []struct {
				Text       string `json:"text"`
				InlineData struct {
					MIMEType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(prepared.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Contents) != 1 || len(body.Contents[0].Parts) != 3 {
		t.Fatalf("unexpected image input parts: %s", prepared.Body())
	}
	parts := body.Contents[0].Parts
	if parts[0].Text != "Describe both images." ||
		parts[1].InlineData.MIMEType != "image/png" ||
		parts[1].InlineData.Data != base64.StdEncoding.EncodeToString([]byte("first")) ||
		parts[2].InlineData.MIMEType != "image/jpeg" ||
		parts[2].InlineData.Data != base64.StdEncoding.EncodeToString([]byte("second")) {
		t.Fatalf("inline images changed or reordered: %s", prepared.Body())
	}
}

func TestPrepareEncodesGeminiFileReferences(t *testing.T) {
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpClient, err := client.httpClient("gemini-test")
	if err != nil {
		t.Fatal(err)
	}
	input := llm.Input{
		Model: "gemini-test",
		Messages: []llm.Message{
			llm.UserMessage(
				llm.Block(content.DocumentBlock(content.Source{
					URI:      "https://files.example.test/pdf-1",
					MIMEType: "application/pdf",
					Origin:   httpClient.Origin("gemini-test"),
				})),
				llm.Block(content.ImageBlock(content.Source{
					URL:      "https://example.test/image.png",
					MIMEType: "image/png",
				})),
			),
		},
	}
	prepared, err := client.Prepare(context.Background(), input)
	if err != nil {
		t.Fatalf("Gemini file references did not prepare: %v", err)
	}
	var body struct {
		Contents []struct {
			Parts []struct {
				FileData struct {
					MIMEType string `json:"mimeType"`
					FileURI  string `json:"fileUri"`
				} `json:"fileData"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(prepared.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Contents) != 1 || len(body.Contents[0].Parts) != 2 {
		t.Fatalf("Gemini file parts were lost: %s", prepared.Body())
	}
	first := body.Contents[0].Parts[0].FileData
	if first.FileURI != "https://files.example.test/pdf-1" ||
		first.MIMEType != "application/pdf" {
		t.Fatalf("provider URI was changed: %+v", first)
	}
	second := body.Contents[0].Parts[1].FileData
	if second.FileURI != "https://example.test/image.png" ||
		second.MIMEType != "image/png" {
		t.Fatalf("public URL was changed: %+v", second)
	}
}

func TestPrepareRejectsForeignGeminiFileURI(t *testing.T) {
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := llm.Input{
		Model: "gemini-test",
		Messages: []llm.Message{
			llm.UserMessage(llm.Block(
				content.DocumentBlock(content.Source{
					URI:      "https://files.example.test/pdf-1",
					MIMEType: "application/pdf",
					Origin: content.Origin{
						Provider: "other",
						API:      API,
						Scope:    "other",
					},
				}),
			)),
		},
	}
	if _, err := client.Prepare(context.Background(), input); err == nil {
		t.Fatal("foreign provider URI prepared for Gemini")
	}
}
