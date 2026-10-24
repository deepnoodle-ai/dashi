package gemini

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

// A JSON null usage is usage the provider did not report, not a malformed
// response.
func TestMediaNullUsageIsUnreported(t *testing.T) {
	image := base64.StdEncoding.EncodeToString([]byte("png-data"))
	t.Run("image", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"responseId":"image-1","usageMetadata":null,"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"%s"}}]}}]}`, image)
		}))
		defer server.Close()
		p := newMediaFixture(t, server)
		request, err := p.PrepareImage(t.Context(), media.ImageInput{
			Model:     "gemini-image",
			Operation: media.ImageGenerate,
			Prompt:    []content.Block{content.TextBlock("a cat")},
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := p.SendImage(t.Context(), request)
		if err != nil || result.RawUsage != nil {
			t.Fatalf("result %+v, error %v", result, err)
		}
	})
	t.Run("interactions", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"id":"i-1","status":"completed","usage":null,"steps":[{"type":"model_output","content":[{"type":"image","mime_type":"image/png","data":"%s"}]}]}`, image)
		}))
		defer server.Close()
		p := newInteractionsFixture(t, server)
		request, err := p.PrepareImage(t.Context(), media.ImageInput{
			Model:     "gemini-3.1-flash-image",
			Operation: media.ImageGenerate,
			Prompt:    []content.Block{content.TextBlock("a cat")},
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := p.SendImage(t.Context(), request)
		if err != nil || result.RawUsage != nil {
			t.Fatalf("result %+v, error %v", result, err)
		}
	})
	t.Run("video job", func(t *testing.T) {
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"id":"interaction-1","status":"completed","usage":null,"steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":%q}]}]}`, server.URL+"/v1beta/files/clip-1:download?alt=media")
		}))
		defer server.Close()
		p := newInteractionsJobFixture(t, server)
		request, err := p.PrepareVideo(t.Context(), media.VideoInput{
			Model:  "gemini-omni-1.1-flash",
			Prompt: []content.Block{content.TextBlock("a sunrise")},
		})
		if err != nil {
			t.Fatal(err)
		}
		submission, err := p.SubmitVideo(t.Context(), request)
		if err != nil || submission.State != media.SubmissionAccepted ||
			submission.RawUsage != nil || submission.Validate() != nil {
			t.Fatalf("submission %+v, error %v", submission, err)
		}
	})
}
