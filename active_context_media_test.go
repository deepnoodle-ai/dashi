package dashi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func activeMediaWarnings(warnings []Warning) int {
	n := 0
	for _, w := range warnings {
		if w.Code == WarningActiveCompactionMediaEstimateUnavailable {
			n++
		}
	}
	return n
}
func activePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	seed := uint32(1)
	for i := range img.Pix {
		seed = 1664525*seed + 1013904223
		img.Pix[i] = byte(seed >> 24)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
func TestActiveCompactionLargeImageEstimateUnavailable(t *testing.T) {
	data := activePNG(t)
	for _, window := range []int{128000, 200000} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			c := &activeClient{window: window, scriptedClient: scriptedClient{results: []llm.Result{final("inspected image")}}}
			a := testAgent()
			a.ActiveCompaction = &ActiveCompactionPolicy{}
			a.Settings.MaxTokens = 256
			start := activeStart(a)
			start.Input[0].Content = append(start.Input[0].Content, llm.Image(content.Source{Data: data, MIMEType: "image/png"}))
			out, err := (Runner{Client: c}).Run(t.Context(), Turn{}, start)
			if err != nil || out.Stop.Kind != StopCompleted || c.sends != 1 || len(out.ContextCompactions) != 0 || out.ActiveCompactionWindow != window || activeMediaWarnings(out.Warnings) != 1 {
				t.Fatalf("stop=%+v sends=%d warnings=%+v err=%v", out.Stop, c.sends, out.Warnings, err)
			}
			req, err := c.Prepare(t.Context(), c.sent[0])
			if err != nil {
				t.Fatal(err)
			}
			estimate := llm.EstimatedInputTokens(req)
			if estimate <= window || len(data) < 1000000 {
				t.Fatalf("probe too small: PNG=%d estimate=%d window=%d", len(data), estimate, window)
			}
			if _, err := Replay(out.Turn); err != nil {
				t.Fatal(err)
			}
			t.Logf("valid PNG bytes=%d prepared estimate=%d window=%d sends=%d warning=1", len(data), estimate, window, c.sends)
		})
	}
}
func TestActiveCompactionMediaHistoryToolHookReload(t *testing.T) {
	data := activePNG(t)
	imagePart := llm.Image(content.Source{Data: data, MIMEType: "image/png"})
	for _, source := range []string{"input", "history", "tool", "hook"} {
		t.Run(source, func(t *testing.T) {
			c := &activeClient{window: 128000, scriptedClient: scriptedClient{results: []llm.Result{callResult("read-image", "read"), callResult("read-again", "read"), final("inspected")}}}
			a := testAgent()
			a.ActiveCompaction = &ActiveCompactionPolicy{}
			a.Settings.MaxTokens = 256
			a.Limits.ModelAttempts = 1
			start := activeStart(a)
			switch source {
			case "input":
				start.Input[0].Content = append(start.Input[0].Content, imagePart)
			case "history":
				start.History = []llm.Message{llm.UserMessage(imagePart)}
			}
			calls := 0
			tools := tool.List{tool.MustFunc("read", "Read evidence", func(context.Context, tool.Call, struct {
				Value int `json:"value"`
			}) (tool.Outcome, error) {
				calls++
				out := tool.Succeeded("read complete")
				if source == "tool" {
					out.Output.Content = append(out.Output.Content, *imagePart.Block)
				}
				return out, nil
			})}
			hooks := Hooks{}
			if source == "hook" {
				hooks.BeforeModel = []ModelHook{{Name: "image", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
					return ModelDecision{Deliver: []ContextItem{{ID: "image", Origin: ContextRuntime, Name: "image", Message: llm.UserMessage(imagePart)}}}, nil
				}}}
			}
			r := Runner{Client: c, Tools: tools, Hooks: hooks}
			first, err := r.Run(t.Context(), Turn{ID: "media"}, start)
			if err != nil || first.Stop.Kind != StopLimited {
				t.Fatalf("first=%+v err=%v", first.Stop, err)
			}
			encoded, _ := json.Marshal(first.Turn)
			var loaded Turn
			if err = json.Unmarshal(encoded, &loaded); err != nil {
				t.Fatal(err)
			}
			out, err := r.Run(t.Context(), loaded, Continue{CommandID: "resume", ModelAttempts: 4, History: start.History})
			if err != nil || out.Stop.Kind != StopCompleted || c.sends != 3 || calls != 2 || activeMediaWarnings(out.Warnings) != 1 || len(out.ContextCompactions) != 0 {
				t.Fatalf("stop=%+v sends=%d calls=%d warnings=%+v err=%v", out.Stop, c.sends, calls, out.Warnings, err)
			}
			view, err := Replay(out.Turn)
			if err != nil || activeMediaWarnings(view.Warnings) != 1 {
				t.Fatalf("reload warning=%+v err=%v", view.Warnings, err)
			}
		})
	}
}
func TestActiveCompactionSummaryHookMedia(t *testing.T) {
	c, a := activeFixture()
	c.results = append(c.results[:3], final("done"))
	calls := 0
	hooks := Hooks{BeforeModel: []ModelHook{{Name: "summary-image", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
		if p.Purpose != ModelPurposeCompaction {
			return ModelDecision{}, nil
		}
		return ModelDecision{Deliver: []ContextItem{{ID: "image", Name: "image", Origin: ContextRuntime, Message: llm.UserMessage(llm.Image(content.Source{Data: []byte{1, 2, 3}, MIMEType: "image/png"}))}}}, nil
	}}}}
	out, err := (Runner{Client: c, Tools: activeTools(&calls), Hooks: hooks}).Run(t.Context(), Turn{}, activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted || len(out.ContextCompactions) != 1 || c.sends != 4 || activeMediaWarnings(out.Warnings) != 1 {
		t.Fatalf("stop=%+v applications=%d sends=%d warnings=%+v err=%v", out.Stop, len(out.ContextCompactions), c.sends, out.Warnings, err)
	}
	if !activeInputHasMedia(c.sent[2]) || !activeInputHasMedia(c.sent[3]) {
		t.Fatal("summary hook media was dropped")
	}
}

type activePricedClient struct{ *activeClient }

func (c activePricedClient) Prepare(ctx context.Context, in llm.Input) (llm.Request, error) {
	req, err := c.activeClient.Prepare(ctx, in)
	if err != nil {
		return llm.Request{}, err
	}
	snap := req.Snapshot()
	snap.ModelInfo.Pricing = &llm.Pricing{Currency: "USD", UpdatedAt: "2026-09-30", Base: llm.Rates{Input: amount("1"), Output: amount("1")}}
	return llm.NewRequest(snap)
}
func TestActiveCompactionMediaFinancialAdmissionUnchanged(t *testing.T) {
	data := activePNG(t)
	for _, affordable := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "admitted"}[affordable], func(t *testing.T) {
			c := activePricedClient{&activeClient{window: 128000, scriptedClient: scriptedClient{results: []llm.Result{final("inspected")}}}}
			a := testAgent()
			a.ActiveCompaction = &ActiveCompactionPolicy{}
			a.Settings.MaxTokens = 256
			start := activeStart(a)
			start.Input[0].Content = append(start.Input[0].Content, llm.Image(content.Source{Data: data, MIMEType: "image/png"}))
			cap := "0.01"
			if affordable {
				cap = "2"
			}
			start.Budget = &Budget{MaxCost: &Money{Amount: amount(cap), Currency: "USD"}}
			observed := false
			hooks := Hooks{BeforeSend: []SendHook{{Name: "risk", Func: func(_ context.Context, p SendPlan) (SendDecision, error) {
				observed = true
				expected, ok := llm.MaxCostAtRisk(p.Request, *p.Request.Snapshot().ModelInfo.Pricing)
				if !ok || p.AtRisk == nil || p.AtRisk.Total != expected.Total {
					t.Fatalf("risk changed: plan=%+v expected=%+v priced=%v", p.AtRisk, expected, ok)
				}
				return SendDecision{}, nil
			}}}}
			out, err := (Runner{Client: c, Hooks: hooks}).Run(t.Context(), Turn{}, start)
			if err != nil {
				t.Fatal(err)
			}
			if affordable {
				if out.Stop.Kind != StopCompleted || c.sends != 1 || !observed {
					t.Fatalf("admission=%+v sends=%d", out.Stop, c.sends)
				}
			} else {
				if out.Stop.Limit == nil || out.Stop.Limit.Kind != LimitCost || c.sends != 0 || observed {
					t.Fatalf("refusal=%+v sends=%d", out.Stop, c.sends)
				}
			}
			if activeMediaWarnings(out.Warnings) != 1 {
				t.Fatal("missing unavailable estimate warning")
			}
		})
	}
}

func TestActiveCompactionMediaDoesNotDisableLaterTextChecks(t *testing.T) {
	c, a := activeFixture()
	calls := 0
	hooks := Hooks{BeforeModel: []ModelHook{{Name: "first-image", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
		if p.Turn.ModelAttempts != 0 {
			return ModelDecision{}, nil
		}
		return ModelDecision{Deliver: []ContextItem{{ID: "first-image", Name: "image", Origin: ContextRuntime, NextCallOnly: true, Message: llm.UserMessage(llm.Image(content.Source{Data: []byte{1, 2, 3}, MIMEType: "image/png"}))}}}, nil
	}}}}
	out, err := (Runner{Client: c, Tools: activeTools(&calls), Hooks: hooks}).Run(t.Context(), Turn{}, activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted || len(out.ContextCompactions) != 2 || activeMediaWarnings(out.Warnings) != 1 {
		t.Fatalf("stop=%+v applications=%d warnings=%+v err=%v", out.Stop, len(out.ContextCompactions), out.Warnings, err)
	}
	if !activeInputHasMedia(c.sent[0]) || activeInputHasMedia(c.sent[1]) {
		t.Fatal("next-call media did not expire on ordinary dispatch")
	}
}
func TestActiveCompactionMediaProviderRejectionDoesNotRetry(t *testing.T) {
	c := &activeClient{window: 128000, scriptedClient: scriptedClient{results: []llm.Result{{}}, errors: []error{&llm.Error{Provider: "test", Category: llm.ErrorInvalidRequest, Reason: llm.ReasonContextWindowExceeded, Disposition: llm.FailureRejected, Status: 400}}}}
	a := testAgent()
	a.ActiveCompaction = &ActiveCompactionPolicy{}
	a.Settings.MaxTokens = 256
	start := activeStart(a)
	start.Input[0].Content = append(start.Input[0].Content, llm.Image(content.Source{Data: activePNG(t), MIMEType: "image/png"}))
	out, err := (Runner{Client: c}).Run(t.Context(), Turn{}, start)
	if err != nil || c.sends != 1 || out.Stop.Kind == StopCompleted || len(out.ContextCompactions) != 0 || activeMediaWarnings(out.Warnings) != 1 {
		t.Fatalf("stop=%+v sends=%d warnings=%+v err=%v", out.Stop, c.sends, out.Warnings, err)
	}
}
func TestActiveInputMediaDetection(t *testing.T) {
	for _, kind := range []content.Kind{content.KindImage, content.KindDocument, content.KindAudio, content.KindVideo} {
		block := content.Block{Kind: kind}
		for _, part := range []llm.Content{llm.Block(block), {Kind: llm.ContentToolResult, ToolResult: &llm.ToolResult{Output: tool.Output{Content: []content.Block{block}}}}, {Kind: llm.ContentHostedUse, HostedUse: &llm.HostedUse{Output: []content.Block{block}}}, {Kind: llm.ContentHostedResult, HostedResult: &llm.HostedResult{Output: []content.Block{block}}}} {
			if !activeInputHasMedia(llm.Input{Messages: []llm.Message{llm.UserMessage(part)}}) {
				t.Fatalf("missed %s in %s", kind, part.Kind)
			}
		}
	}
	if activeInputHasMedia(llm.Input{Messages: []llm.Message{llm.UserMessage(llm.Text("text and ordinary path references"))}}) {
		t.Fatal("text treated as media")
	}
}

func TestActiveInputTextDocumentDetection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source content.Source
		media  bool
	}{
		{"plain", content.Source{Data: []byte("notes"), MIMEType: "text/plain"}, false},
		{"markdown", content.Source{Data: []byte("# Notes"), MIMEType: "text/markdown"}, false},
		{"csv", content.Source{Data: []byte("a,b\n1,2"), MIMEType: "text/csv"}, false},
		{"json", content.Source{Data: []byte(`{"note":"text"}`), MIMEType: "application/json"}, false},
		{"xml", content.Source{Data: []byte("<note>text</note>"), MIMEType: "application/xml"}, false},
		{"yaml", content.Source{Data: []byte("note: text"), MIMEType: "application/yaml"}, false},
		{"x-yaml", content.Source{Data: []byte("note: text"), MIMEType: "application/x-yaml"}, false},
		{"detected", content.Source{Data: []byte("notes")}, false},
		{"UTF-8", content.Source{Data: []byte("日本語のメモ"), MIMEType: "text/plain"}, false},
		{"invalid UTF-8", content.Source{Data: []byte{0xff}, MIMEType: "text/plain"}, true},
		{"NUL", content.Source{Data: []byte{'a', 0, 'b'}, MIMEType: "text/plain"}, true},
		{"PDF ASCII", content.Source{Data: []byte("%PDF-1.4"), MIMEType: "application/pdf"}, true},
		{"unknown", content.Source{Data: []byte("notes"), MIMEType: "application/octet-stream"}, true},
		{"remote", content.Source{URL: "https://example.test/notes.txt", MIMEType: "text/plain"}, true},
		{"provider ID", content.Source{ID: "file-1", MIMEType: "text/plain"}, true},
		{"provider URI", content.Source{URI: "provider://notes", MIMEType: "text/plain"}, true},
		{"unresolved", content.Source{Ref: &content.Ref{ID: "notes"}, MIMEType: "text/plain"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := content.DocumentBlock(tc.source, "")
			before, _ := json.Marshal(block)
			for _, part := range []llm.Content{llm.Block(block), {Kind: llm.ContentToolResult, ToolResult: &llm.ToolResult{Output: tool.Output{Content: []content.Block{block}}}}, {Kind: llm.ContentHostedUse, HostedUse: &llm.HostedUse{Output: []content.Block{block}}}, {Kind: llm.ContentHostedResult, HostedResult: &llm.HostedResult{Output: []content.Block{block}}}} {
				if got := activeInputHasMedia(llm.Input{Messages: []llm.Message{llm.UserMessage(part)}}); got != tc.media {
					t.Fatalf("%s media=%v, want %v", part.Kind, got, tc.media)
				}
			}
			after, _ := json.Marshal(block)
			if !bytes.Equal(before, after) {
				t.Fatal("classification changed document source")
			}
		})
	}
}

func TestActiveCompactionHydratedTextDocumentHardLimit(t *testing.T) {
	data := []byte(strings.Repeat("x", 10000))
	ref := refFor("notes", data)
	resolver := &countingResolver{files: map[string][]byte{"notes": data}}
	c := &activeClient{window: 8000}
	a := testAgent()
	a.Settings.MaxTokens = 256
	a.ActiveCompaction = &ActiveCompactionPolicy{}
	start := activeStart(a)
	start.Input[0].Content = append(start.Input[0].Content, llm.Document(content.Source{Ref: &ref, MIMEType: "text/plain"}, "notes.txt"))
	out, err := (Runner{Client: c, ContentResolver: resolver}).Run(t.Context(), Turn{}, start)
	if err != nil || out.Stop.Limit == nil || out.Stop.Limit.Kind != LimitContextWindow || resolver.count() != 1 || c.sends != 0 || activeMediaWarnings(out.Warnings) != 0 {
		t.Fatalf("stop=%+v resolutions=%d sends=%d warnings=%+v err=%v", out.Stop, resolver.count(), c.sends, out.Warnings, err)
	}
	source := out.Turn.Steps[0].Started.Input[0].Content[1].Block.Document.Source
	if source.Ref == nil || *source.Ref != ref || source.Data != nil {
		t.Fatal("classification rewrote recorded reference")
	}
}
