package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func resultPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestConvertResultPreservesOrderAndCopiesMedia(t *testing.T) {
	imageData := resultPNG(t)
	audioData := []byte("RIFF\x04\x00\x00\x00WAVE")
	res := &sdk.CallToolResult{
		Content: []sdk.Content{
			&sdk.TextContent{Text: "Screenshot"},
			&sdk.ImageContent{Data: imageData, MIMEType: "image/png"},
			&sdk.AudioContent{Data: audioData, MIMEType: "audio/x-wav"},
		},
		StructuredContent: map[string]any{"ok": true},
		IsError:           true,
	}
	out, warnings, err := ConvertResult(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected projection warnings: %v", warnings)
	}
	var kinds []content.Kind
	for _, block := range out.Output.Content {
		kinds = append(kinds, block.Kind)
	}
	if !slices.Equal(kinds, []content.Kind{
		content.KindText, content.KindImage, content.KindAudio, content.KindText,
	}) {
		t.Fatalf("content order = %v", kinds)
	}
	if out.State != tool.StateFailed {
		t.Fatal("MCP failure became success")
	}
	if out.Output.Content[2].Audio.Source.MIMEType != "audio/wav" {
		t.Fatal("WAV alias was not normalized")
	}
	imageData[0], audioData[0] = 0, 0
	if out.Output.Content[1].Image.Source.Data[0] == 0 {
		t.Fatal("returned image shares SDK bytes")
	}
	if out.Output.Content[2].Audio.Source.Data[0] == 0 {
		t.Fatal("returned audio shares SDK bytes")
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConvertResultMakesProjectionLossExplicit(t *testing.T) {
	res := &sdk.CallToolResult{
		Meta: sdk.Meta{"private": "SECRET"},
		Content: []sdk.Content{
			&sdk.ResourceLink{
				Name:        "notes",
				URI:         "file:///notes.txt",
				Description: "Summary notes",
				MIMEType:    "text/plain",
			},
			&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{
				URI:  "file:///inline.txt",
				Text: "inline text",
			}},
			&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{
				URI:      "file:///SECRET.bin",
				MIMEType: "application/octet-stream",
				Blob:     []byte("SECRET"),
			}},
			&sdk.ImageContent{
				Data:        []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`),
				MIMEType:    "image/svg+xml",
				Annotations: &sdk.Annotations{},
			},
		},
	}
	out, warnings, err := ConvertResult(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Output.Content) != 4 || len(warnings) != 4 {
		t.Fatalf("blocks = %d, warnings = %v", len(out.Output.Content), warnings)
	}
	for i, want := range []string{
		"Summary notes", "inline text", "6 bytes", "image/svg+xml",
	} {
		if !strings.Contains(out.Output.Content[i].Text.Text, want) {
			t.Fatalf("block %d lost its description", i)
		}
	}
	for _, warning := range warnings {
		if strings.Contains(warning, "SECRET") {
			t.Fatal("conversion diagnostic included an input value")
		}
	}
}

func TestConvertResultRejectsInvalidContentWithoutPartialOutcome(t *testing.T) {
	var nilText *sdk.TextContent
	var nilImage *sdk.ImageContent
	var nilAudio *sdk.AudioContent
	var nilLink *sdk.ResourceLink
	var nilResource *sdk.EmbeddedResource
	for name, bad := range map[string]sdk.Content{
		"nil":              nil,
		"nil text":         nilText,
		"nil image":        nilImage,
		"nil audio":        nilAudio,
		"nil link":         nilLink,
		"nil resource":     nilResource,
		"empty image":      &sdk.ImageContent{MIMEType: "image/png"},
		"mismatched image": &sdk.ImageContent{Data: []byte("SECRET"), MIMEType: "image/png"},
		"wrong family":     &sdk.ImageContent{Data: resultPNG(t), MIMEType: "audio/wav"},
		"malformed MIME":   &sdk.AudioContent{Data: []byte("SECRET"), MIMEType: "SECRET;broken"},
		"relative URI":     &sdk.ResourceLink{Name: "SECRET", URI: "SECRET/file"},
		"missing resource": &sdk.EmbeddedResource{},
		"ambiguous resource": &sdk.EmbeddedResource{Resource: &sdk.ResourceContents{
			URI: "file:///SECRET", Text: "SECRET", Blob: []byte("SECRET"),
		}},
		"sampling call":   &sdk.ToolUseContent{Name: "SECRET"},
		"sampling result": &sdk.ToolResultContent{ToolUseID: "SECRET"},
	} {
		t.Run(name, func(t *testing.T) {
			out, _, err := ConvertResult(&sdk.CallToolResult{
				Content: []sdk.Content{&sdk.TextContent{Text: "first"}, bad},
			})
			if err == nil {
				t.Fatal("invalid content converted successfully")
			}
			if !reflect.DeepEqual(out, tool.Outcome{}) {
				t.Fatal("conversion error returned a partial outcome")
			}
			if !strings.Contains(err.Error(), "block 1") {
				t.Fatalf("diagnostic does not identify the block: %v", err)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatal("error included an input value")
			}
		})
	}
}

func TestConvertResultRejectsUnusableStructuredAndNilResults(t *testing.T) {
	for _, res := range []*sdk.CallToolResult{
		nil,
		{InputRequests: sdk.InputRequestMap{}},
		{RequestState: "SECRET"},
		{StructuredContent: func() {}},
		{StructuredContent: json.RawMessage(`{"SECRET":`)},
		{StructuredContent: failingJSON{}},
	} {
		out, _, err := ConvertResult(res)
		if err == nil || !reflect.DeepEqual(out, tool.Outcome{}) {
			t.Fatalf("invalid result returned outcome %+v, error %v", out, err)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Fatal("structured marshal error disclosed input")
		}
	}
	out, warnings, err := ConvertResult(&sdk.CallToolResult{})
	if err != nil || out.State != tool.StateSucceeded || len(warnings) != 0 {
		t.Fatalf("empty result = %+v, warnings %v, error %v", out, warnings, err)
	}
}

type failingJSON struct{}

func (failingJSON) MarshalJSON() ([]byte, error) {
	return nil, errors.New("SECRET")
}

func TestConvertResultSeparatesInputRequired(t *testing.T) {
	var res sdk.CallToolResult
	if err := json.Unmarshal([]byte(`{"resultType":"input_required","requestState":"SECRET"}`), &res); err != nil {
		t.Fatal(err)
	}
	out, _, err := ConvertResult(&res)
	if !errors.Is(err, ErrInputRequired) || !reflect.DeepEqual(out, tool.Outcome{}) {
		t.Fatalf("input interaction became a completed outcome: %+v, %v", out, err)
	}
	if res.RequestState != "SECRET" || !res.NeedsInput() {
		t.Fatal("conversion changed the host's interaction state")
	}
}
