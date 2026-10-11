package mcp

import (
	"bytes"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConvertResultSupportsGuaranteedMediaAndAliases(t *testing.T) {
	for _, tc := range []struct {
		name string
		mime string
		want string
		data []byte
		kind content.Kind
	}{
		{
			name: "PNG",
			mime: "image/png",
			want: "image/png",
			data: resultPNG(t),
			kind: content.KindImage,
		},
		{
			name: "JPEG",
			mime: "image/jpeg",
			want: "image/jpeg",
			data: []byte{0xff, 0xd8, 0xff, 0xe0},
			kind: content.KindImage,
		},
		{
			name: "JPEG alias",
			mime: "image/jpg",
			want: "image/jpeg",
			data: []byte{0xff, 0xd8, 0xff, 0xe0},
			kind: content.KindImage,
		},
		{
			name: "GIF87a",
			mime: "image/gif",
			want: "image/gif",
			data: []byte("GIF87a"),
			kind: content.KindImage,
		},
		{
			name: "GIF89a",
			mime: "image/gif",
			want: "image/gif",
			data: []byte("GIF89a"),
			kind: content.KindImage,
		},
		{
			name: "WebP",
			mime: "image/webp",
			want: "image/webp",
			data: []byte("RIFF\x06\x00\x00\x00WEBPVP8 "),
			kind: content.KindImage,
		},
		{
			name: "WAV",
			mime: "audio/wav",
			want: "audio/wav",
			data: []byte("RIFF\x04\x00\x00\x00WAVE"),
			kind: content.KindAudio,
		},
		{
			name: "WAV wave alias",
			mime: "audio/wave",
			want: "audio/wav",
			data: []byte("RIFF\x04\x00\x00\x00WAVE"),
			kind: content.KindAudio,
		},
		{
			name: "WAV x-wav alias",
			mime: "audio/x-wav",
			want: "audio/wav",
			data: []byte("RIFF\x04\x00\x00\x00WAVE"),
			kind: content.KindAudio,
		},
		{
			name: "MP3 ID3",
			mime: "audio/mpeg",
			want: "audio/mpeg",
			data: []byte("ID3\x04\x00\x00\x00\x00\x00\x00"),
			kind: content.KindAudio,
		},
		{
			name: "MP3 frame",
			mime: "audio/mpeg",
			want: "audio/mpeg",
			data: []byte{0xff, 0xfb, 0x90, 0x00},
			kind: content.KindAudio,
		},
		{
			name: "MP3 alias",
			mime: "audio/mp3",
			want: "audio/mpeg",
			data: []byte{0xff, 0xfb, 0x90, 0x00},
			kind: content.KindAudio,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var block sdk.Content = &sdk.ImageContent{Data: tc.data, MIMEType: tc.mime}
			if tc.kind == content.KindAudio {
				block = &sdk.AudioContent{Data: tc.data, MIMEType: tc.mime}
			}
			out, warnings, err := ConvertResult(&sdk.CallToolResult{
				Content: []sdk.Content{block},
			})
			if err != nil || len(warnings) != 0 {
				t.Fatalf("guaranteed media was not preserved: warnings %v, error %v", warnings, err)
			}
			got := out.Output.Content[0]
			if got.Kind != tc.kind {
				t.Fatalf("kind = %s; want %s", got.Kind, tc.kind)
			}
			source := got.Image
			var data []byte
			var mime string
			if source != nil {
				data, mime = source.Source.Data, source.Source.MIMEType
			} else {
				data, mime = got.Audio.Source.Data, got.Audio.Source.MIMEType
			}
			if mime != tc.want {
				t.Fatalf("MIME = %s; want %s", mime, tc.want)
			}
			if !bytes.Equal(data, tc.data) {
				t.Fatal("returned media bytes changed")
			}
			if err := out.Validate(); err != nil || out.State != tool.StateSucceeded {
				t.Fatal("converted media is not a valid successful outcome")
			}
		})
	}
}
