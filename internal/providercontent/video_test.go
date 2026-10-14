package providercontent

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestAdaptChecksVideoFileReferenceScope(t *testing.T) {
	foreign := content.Origin{Provider: "google", API: "gemini", Model: "m", Scope: "a"}
	target := content.Origin{Provider: "openai", API: "openai.responses", Model: "m", Scope: "b"}
	video := func(origin content.Origin) llm.Content {
		return llm.Block(content.Block{
			Kind: content.KindVideo,
			Video: &content.Video{Source: content.Source{
				ID:       "files/abc",
				Origin:   origin,
				MIMEType: "video/mp4",
			}},
		})
	}
	input := func(origin content.Origin) llm.Input {
		return llm.Input{
			Model:    "m",
			Messages: []llm.Message{llm.UserMessage(llm.Text("Describe"), video(origin))},
		}
	}
	if _, _, err := Adapt(input(foreign), target); err == nil {
		t.Fatal("Adapt accepted a video file reference from another provider scope")
	}
	if _, _, err := Adapt(input(target), target); err != nil {
		t.Fatalf("Adapt rejected a same-scope video reference: %v", err)
	}
}
