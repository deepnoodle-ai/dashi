package mcp

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConvertResultJSONDecodesWholeMultimodalEnvelope(t *testing.T) {
	res := &sdk.CallToolResult{
		Content: []sdk.Content{
			&sdk.TextContent{Text: "Screenshot"},
			&sdk.ImageContent{Data: resultPNG(t), MIMEType: "image/png"},
		},
		IsError: true,
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := ConvertResultJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Output.Content) != 2 || out.Output.Content[1].Kind != content.KindImage {
		t.Fatal("whole MCP result lost its image")
	}
	if out.State != tool.StateFailed {
		t.Fatal("JSON MCP failure became success")
	}
}

func TestConvertResultJSONPreservesExactStructuredNumbers(t *testing.T) {
	for _, number := range []string{
		"9007199254740993", "0.1234567890123456789", "1e400",
	} {
		for _, besideText := range []bool{false, true} {
			name := number
			if besideText {
				name += " beside equivalent text"
			}
			t.Run(name, func(t *testing.T) {
				value := `{"n":` + number + `}`
				text := ""
				if besideText {
					quoted, _ := json.Marshal(value)
					text = `{"type":"text","text":` + string(quoted) + `}`
				}
				raw := json.RawMessage(`{"content":[` + text + `],"structuredContent":` + value + `}`)
				out, _, err := ConvertResultJSON(raw)
				if err != nil {
					t.Fatal(err)
				}
				if len(out.Output.Content) != 1 {
					t.Fatal("structured value was duplicated or lost")
				}
				if got := out.Output.Content[0].Text.Text; got != value {
					t.Fatalf("structured number changed: got %s, want %s", got, value)
				}
			})
		}
	}
}

func TestConvertResultJSONDistinguishesRoundedAndEquivalentNumbers(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want int
	}{
		{
			name: "rounded nearby integer is different",
			raw:  `{"content":[{"type":"text","text":"9007199254740992"}],"structuredContent":9007199254740993}`,
			want: 2,
		},
		{
			name: "exact alternate decimal is equivalent",
			raw:  `{"content":[{"type":"text","text":"0.12345678901234567890"}],"structuredContent":0.1234567890123456789}`,
			want: 1,
		},
		{
			name: "explicit structured null is retained",
			raw:  `{"content":[],"structuredContent":null}`,
			want: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := ConvertResultJSON(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Output.Content) != tc.want {
				t.Fatalf("content count = %d; want %d", len(out.Output.Content), tc.want)
			}
		})
	}
}

func TestConvertResultJSONRejectsMalformedContentSafely(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"content":null}`, `{"content":{}}`,
		`{"content":[]} {"SECRET":true}`,
		`{"content":[],"isError":"SECRET"}`,
		`{"content":[],"isError":null}`,
		`{"content":[],"resultType":"SECRET"}`,
		`{"content":[null]}`,
		`{"content":[{"type":"text"}]}`,
		`{"content":[{"type":"text","text":null}]}`,
		`{"content":[{"type":"SECRET"}]}`,
		`{"content":[{"type":"tool_use","name":"SECRET"}]}`,
		`{"content":[{"type":"image","mimeType":"image/png","data":"SECRET"}]}`,
		`{"content":[{"type":"resource","resource":{"uri":"file:///SECRET"}}]}`,
		`{"content":[{"type":"resource","resource":{"uri":"file:///SECRET","text":"","blob":"AA=="}}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			out, _, err := ConvertResultJSON(json.RawMessage(raw))
			if err == nil || !reflect.DeepEqual(out, tool.Outcome{}) {
				t.Fatalf("malformed input returned %+v, error %v", out, err)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatal("error disclosed raw input or SDK exception text")
			}
		})
	}
}

func TestConvertResultJSONInputRequiredPrecedesContentValidation(t *testing.T) {
	for _, raw := range []string{
		`{"resultType":"input_required","requestState":"SECRET"}`,
		`{"resultType":"input_required","content":null}`,
		`{"resultType":"input_required","content":[{"type":"SECRET"}]}`,
	} {
		out, _, err := ConvertResultJSON(json.RawMessage(raw))
		if !errors.Is(err, ErrInputRequired) || !reflect.DeepEqual(out, tool.Outcome{}) {
			t.Fatalf("input request returned %+v, %v", out, err)
		}
	}
}

func TestConvertResultJSONIdentifiesSDKDecodeFailureBlock(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"first"},{"type":"image","data":"!SECRET!","mimeType":"image/png"}]}`)
	_, _, err := ConvertResultJSON(raw)
	if err == nil || !strings.Contains(err.Error(), "block 1") {
		t.Fatalf("SDK decoding error did not identify its block: %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatal("SDK decoding error disclosed its input")
	}
}
