package dashi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

var refBytes = []byte("%PDF-1.4 stored upload")

func refFor(id string, data []byte) content.Ref {
	sum := sha256.Sum256(data)
	return content.Ref{
		ID:     id,
		SHA256: hex.EncodeToString(sum[:]),
		Size:   int64(len(data)),
	}
}

func refBlock(ref content.Ref) content.Block {
	return content.DocumentBlock(
		content.Source{Ref: &ref, MIMEType: "application/pdf"},
		"upload.pdf",
	)
}

func refMessage(ref content.Ref) llm.Message {
	return llm.UserMessage(llm.Text("read this"), llm.Block(refBlock(ref)))
}

// bodyClient encodes the prepared input as the request body, so the request
// digest and BeforeSend see the exact bytes that would be sent.
type bodyClient struct {
	scriptedClient
}

func (c *bodyClient) Prepare(
	ctx context.Context,
	in llm.Input,
) (llm.Request, error) {
	c.inputs = append(c.inputs, in)
	body, err := json.Marshal(in)
	if err != nil {
		return llm.Request{}, err
	}
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      body,
	})
}

// countingResolver serves stored bytes and records each call.
type countingResolver struct {
	mu      sync.Mutex
	files   map[string][]byte
	err     error
	calls   []string
	turnIDs []string
}

func (r *countingResolver) Resolve(
	_ context.Context,
	turnID string,
	ref content.Ref,
) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, ref.ID)
	r.turnIDs = append(r.turnIDs, turnID)
	if r.err != nil {
		return nil, r.err
	}
	data, ok := r.files[ref.ID]
	if !ok {
		return nil, errors.New("not found")
	}
	return data, nil
}

func (r *countingResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func runWithRef(
	t *testing.T,
	runner Runner,
	messages ...llm.Message,
) Outcome {
	t.Helper()
	if runner.Agent.ID == "" {
		runner.Agent = testAgent()
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{CommandID: "start", Input: messages},
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out
}

func requireNoModelIntent(t *testing.T, out Outcome, client *scriptedClient) {
	t.Helper()
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelIntent {
			t.Fatal("a model intent was recorded")
		}
	}
	if len(client.inputs) != 0 || client.sends != 0 {
		t.Fatalf("prepared %d, sent %d", len(client.inputs), client.sends)
	}
}

func TestRefHydratesBeforeModelRequest(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{results: []llm.Result{final("done")}}

	out := runWithRef(
		t,
		Runner{Client: client, ContentResolver: resolver},
		refMessage(ref),
	)

	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
	got := client.inputs[0].Messages[0].Content[1].Block.Document.Source
	if got.Ref != nil || !bytes.Equal(got.Data, refBytes) {
		t.Fatalf("source was not hydrated: %+v", got)
	}
	if got.MIMEType != "application/pdf" {
		t.Fatalf("MIME type = %q", got.MIMEType)
	}
	if resolver.turnIDs[0] != "t1" {
		t.Fatalf("resolver saw turn %q", resolver.turnIDs[0])
	}
}

func TestRefRecordHoldsNoBytes(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &bodyClient{scriptedClient{results: []llm.Result{final("done")}}}

	out := runWithRef(
		t,
		Runner{Client: client, ContentResolver: resolver},
		refMessage(ref),
	)

	encoded, err := json.Marshal(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, refBytes) ||
		bytes.Contains(encoded, []byte(base64Of(refBytes))) {
		t.Fatal("the record holds attachment bytes")
	}
	if !bytes.Contains(encoded, []byte(`"upl_1"`)) {
		t.Fatal("the record lost the reference")
	}
}

func TestRefRecordSizeDoesNotGrowWithAttachment(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 5<<20)
	ref := refFor("upl_big", data)
	message := refMessage(ref)

	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= 1024 {
		t.Fatalf("message with a 5 MiB reference encodes to %d bytes", len(encoded))
	}
}

func TestRefRequestDigestCoversHydratedBytes(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &bodyClient{scriptedClient{results: []llm.Result{final("done")}}}

	out := runWithRef(
		t,
		Runner{Client: client, ContentResolver: resolver},
		refMessage(ref),
	)

	var digestOf string
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelIntent {
			digestOf = step.Model.RequestDigest
		}
	}
	body, err := json.Marshal(client.inputs[0])
	if err != nil {
		t.Fatal(err)
	}
	if digestOf != digest(body) {
		t.Fatalf("digest %q does not cover the hydrated request", digestOf)
	}
	if !bytes.Contains(body, []byte(base64Of(refBytes))) {
		t.Fatal("the hydrated request holds no bytes")
	}
}

func TestRefHooksSeeRecordedAndHydratedInput(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &bodyClient{scriptedClient{results: []llm.Result{final("done")}}}
	var modelSaw content.Source
	var sendSawBytes bool
	hooks := Hooks{
		BeforeModel: []ModelHook{{
			Name: "watch-model",
			Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
				modelSaw = p.Input.Messages[0].Content[1].Block.Document.Source
				return ModelDecision{}, nil
			},
		}},
		BeforeSend: []SendHook{{
			Name: "watch-send",
			Func: func(_ context.Context, p SendPlan) (SendDecision, error) {
				sendSawBytes = bytes.Contains(
					p.Request.Body(),
					[]byte(base64Of(refBytes)),
				)
				return SendDecision{}, nil
			},
		}},
	}

	out := runWithRef(
		t,
		Runner{Client: client, ContentResolver: resolver, Hooks: hooks},
		refMessage(ref),
	)

	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
	if modelSaw.Ref == nil || modelSaw.Data != nil {
		t.Fatalf("BeforeModel saw %+v, want the reference", modelSaw)
	}
	if !sendSawBytes {
		t.Fatal("BeforeSend did not see the hydrated request")
	}
}

func TestRefFromHookIsHydrated(t *testing.T) {
	ref := refFor("upl_2", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_2": refBytes}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	hooks := Hooks{BeforeModel: []ModelHook{{
		Name: "add-upload",
		Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
			return ModelDecision{Deliver: []ContextItem{{
				ID:      "extra",
				Message: refMessage(ref),
			}}}, nil
		},
	}}}

	out := runWithRef(
		t,
		Runner{Client: client, ContentResolver: resolver, Hooks: hooks},
		llm.UserMessage(llm.Text("hello")),
	)

	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
	found := false
	for _, m := range client.inputs[0].Messages {
		for _, c := range m.Content {
			for _, s := range c.Sources() {
				found = found || bytes.Equal(s.Data, refBytes)
			}
		}
	}
	if !found || resolver.count() != 1 {
		t.Fatalf("found=%v resolves=%d", found, resolver.count())
	}
}

func TestRefFailuresStopBeforeModelIntent(t *testing.T) {
	good := refFor("upl_1", refBytes)
	wrongSize := good
	wrongSize.Size++
	wrongDigest := refFor("upl_1", []byte("other bytes at the same size!"))
	wrongDigest.Size = int64(len(refBytes))
	tests := []struct {
		name     string
		ref      content.Ref
		resolver ContentResolver
		limits   ContentLimits
		want     string
		resolved int
	}{
		{
			name: "no resolver",
			ref:  good,
			want: `content reference "upl_1": no_resolver`,
		},
		{
			name: "resolver error",
			ref:  good,
			resolver: &countingResolver{
				err: errors.New("tenant may not read upl_1"),
			},
			want:     `content reference "upl_1": unavailable`,
			resolved: 1,
		},
		{
			name: "revoked",
			ref:  good,
			resolver: &countingResolver{
				files: map[string][]byte{},
			},
			want:     `content reference "upl_1": unavailable`,
			resolved: 1,
		},
		{
			name: "size mismatch",
			ref:  wrongSize,
			resolver: &countingResolver{
				files: map[string][]byte{"upl_1": refBytes},
			},
			want:     `content reference "upl_1": size_mismatch`,
			resolved: 1,
		},
		{
			name: "digest mismatch",
			ref:  wrongDigest,
			resolver: &countingResolver{
				files: map[string][]byte{"upl_1": refBytes},
			},
			want:     `content reference "upl_1": digest_mismatch`,
			resolved: 1,
		},
		{
			name: "over the reference limit",
			ref:  good,
			resolver: &countingResolver{
				files: map[string][]byte{"upl_1": refBytes},
			},
			limits: ContentLimits{MaxRefBytes: 8},
			want:   `content reference "upl_1": too_large`,
		},
		{
			name: "over the request limit",
			ref:  good,
			resolver: &countingResolver{
				files: map[string][]byte{"upl_1": refBytes},
			},
			limits: ContentLimits{MaxTotalBytes: 8},
			want:   `content reference "upl_1": too_large`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptedClient{results: []llm.Result{final("done")}}

			out := runWithRef(
				t,
				Runner{
					Client:          client,
					ContentResolver: tt.resolver,
					ContentLimits:   tt.limits,
				},
				refMessage(tt.ref),
			)

			if out.Stop.Kind != StopFailed || out.Stop.Reason != tt.want {
				t.Fatalf("stop = %+v, want %q", out.Stop, tt.want)
			}
			requireNoModelIntent(t, out, client)
			if r, ok := tt.resolver.(*countingResolver); ok {
				if r.count() != tt.resolved {
					t.Fatalf("resolves = %d, want %d", r.count(), tt.resolved)
				}
			}
			encoded, _ := json.Marshal(out.Turn)
			if bytes.Contains(encoded, refBytes) {
				t.Fatal("the failure record holds bytes")
			}
			if strings.Contains(out.Stop.Reason, "tenant may not read") {
				t.Fatal("the stop reason exposes the resolver's error text")
			}
		})
	}
}

func TestRefSizeLimitNeedsNoResolverCall(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{results: []llm.Result{final("done")}}

	out := runWithRef(
		t,
		Runner{
			Client:          client,
			ContentResolver: resolver,
			ContentLimits:   ContentLimits{MaxRefBytes: 8},
		},
		refMessage(ref),
	)

	if out.Stop.Kind != StopFailed || resolver.count() != 0 {
		t.Fatalf("stop=%+v resolves=%d", out.Stop, resolver.count())
	}
}

func TestRefNegativeLimitTurnsLimitOff(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{results: []llm.Result{final("done")}}

	out := runWithRef(
		t,
		Runner{
			Client:          client,
			ContentResolver: resolver,
			ContentLimits:   ContentLimits{MaxRefBytes: -1, MaxTotalBytes: -1},
		},
		refMessage(ref),
	)

	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
}

func TestRefSameRefTwiceResolvesTwice(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{results: []llm.Result{final("done")}}

	out := runWithRef(
		t,
		Runner{Client: client, ContentResolver: resolver},
		llm.UserMessage(llm.Block(refBlock(ref)), llm.Block(refBlock(ref))),
	)

	if out.Stop.Kind != StopCompleted || resolver.count() != 2 {
		t.Fatalf("stop=%+v resolves=%d", out.Stop, resolver.count())
	}
}

func TestRefResolvesAgainOnEachModelCall(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "echo"), final("done")},
	}
	echo := tool.Tool{
		Definition: tool.Definition{
			Name:   "echo",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Succeeded("ok"), nil
		}),
	}

	out := runWithRef(
		t,
		Runner{
			Client:          client,
			Tools:           tool.List{echo},
			ContentResolver: resolver,
		},
		refMessage(ref),
	)

	if out.Stop.Kind != StopCompleted || resolver.count() != 2 {
		t.Fatalf("stop=%+v resolves=%d", out.Stop, resolver.count())
	}
}

func TestRefRevokedBetweenModelCallsStopsSecondCall(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "revoke"), final("done")},
	}
	revoke := tool.Tool{
		Definition: tool.Definition{
			Name:   "revoke",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			resolver.mu.Lock()
			delete(resolver.files, "upl_1")
			resolver.mu.Unlock()
			return tool.Succeeded("revoked"), nil
		}),
	}

	out := runWithRef(
		t,
		Runner{
			Client:          client,
			Tools:           tool.List{revoke},
			ContentResolver: resolver,
		},
		refMessage(ref),
	)

	if out.Stop.Kind != StopFailed ||
		out.Stop.Reason != `content reference "upl_1": unavailable` {
		t.Fatalf("stop = %+v", out.Stop)
	}
	if client.sends != 1 {
		t.Fatalf("sends = %d, want 1", client.sends)
	}
}

func TestRefPausedTurnResolvesAgainOnResume(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	runner := Runner{
		Agent:           testAgent(),
		Client:          client,
		Recorder:        rec,
		ContentResolver: resolver,
	}
	pause := make(chan struct{})
	var once sync.Once
	// The pause arrives after the request is prepared, so the first run has
	// already resolved the reference when it stops.
	runner.Hooks = Hooks{BeforeSend: []SendHook{{
		Name: "pause",
		Func: func(context.Context, SendPlan) (SendDecision, error) {
			once.Do(func() { close(pause) })
			return SendDecision{}, nil
		},
	}}}

	first, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{CommandID: "start", Input: []llm.Message{refMessage(ref)}},
		WithPause(pause),
	)
	if err != nil || first.Stop.Kind != StopInterrupted {
		t.Fatalf("first: stop=%+v err=%v", first.Stop, err)
	}
	before := resolver.count()
	out, err := runner.Run(t.Context(), first.Turn, Continue{CommandID: "resume"})

	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("resume: stop=%+v err=%v", out.Stop, err)
	}
	if before != 1 || resolver.count() != 2 {
		t.Fatalf("resolves before=%d after=%d", before, resolver.count())
	}
}

func TestRefInToolResultIsHydrated(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "fetch"), final("done")},
	}
	fetch := tool.Tool{
		Definition: tool.Definition{
			Name:   "fetch",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{
				State: tool.StateSucceeded,
				Output: &tool.Output{
					Content: []content.Block{refBlock(ref)},
				},
			}, nil
		}),
	}

	out := runWithRef(
		t,
		Runner{
			Client:          client,
			Tools:           tool.List{fetch},
			ContentResolver: resolver,
		},
		llm.UserMessage(llm.Text("fetch it")),
	)

	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
	second := client.inputs[1]
	found := false
	for _, m := range second.Messages {
		for _, c := range m.Content {
			for _, s := range c.Sources() {
				found = found || bytes.Equal(s.Data, refBytes)
			}
		}
	}
	if !found {
		t.Fatal("the tool result reference was not hydrated")
	}
	encoded, _ := json.Marshal(out.Turn)
	if bytes.Contains(encoded, []byte(base64Of(refBytes))) {
		t.Fatal("the record holds the tool result bytes")
	}
}

func TestRefProjectAndReplayNeverResolve(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out := runWithRef(
		t,
		Runner{Client: client, ContentResolver: resolver},
		refMessage(ref),
	)
	before := resolver.count()

	projected, err := Project(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replay(out.Turn); err != nil {
		t.Fatal(err)
	}

	if resolver.count() != before {
		t.Fatal("Project or replay called the resolver")
	}
	source := projected[0].Content[1].Block.Document.Source
	if source.Ref == nil || source.Data != nil {
		t.Fatalf("projection holds %+v, want the reference only", source)
	}
}

func TestRefStreamHydrates(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	runner := Runner{
		Agent:           testAgent(),
		Client:          client,
		ContentResolver: resolver,
	}

	var last Outcome
	for event, err := range runner.Stream(
		t.Context(),
		Turn{ID: "t1"},
		Start{CommandID: "start", Input: []llm.Message{refMessage(ref)}},
	) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Outcome != nil {
			last = *event.Outcome
		}
	}

	if last.Stop.Kind != StopCompleted {
		t.Fatalf("stop = %+v", last.Stop)
	}
	if !client.inputs[0].Stream {
		t.Fatal("the request was not streamed")
	}
	source := client.inputs[0].Messages[0].Content[1].Block.Document.Source
	if !bytes.Equal(source.Data, refBytes) {
		t.Fatal("streamed request was not hydrated")
	}
}

func TestRefFreeTurnNeverCallsResolver(t *testing.T) {
	resolver := &countingResolver{}
	client := &scriptedClient{results: []llm.Result{final("done")}}

	out := runWithRef(
		t,
		Runner{Client: client, ContentResolver: resolver},
		llm.UserMessage(
			llm.Text("plain"),
			llm.Document(content.Source{Data: refBytes}, "a.pdf"),
		),
	)

	if out.Stop.Kind != StopCompleted || resolver.count() != 0 {
		t.Fatalf("stop=%+v resolves=%d", out.Stop, resolver.count())
	}
}

func TestRefHydrationLeavesRecordedInputUnchanged(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	message := refMessage(ref)

	runWithRef(
		t,
		Runner{Client: client, ContentResolver: resolver},
		message,
	)

	source := message.Content[1].Block.Document.Source
	if source.Ref == nil || source.Data != nil {
		t.Fatalf("the caller's input changed: %+v", source)
	}
}

func base64Of(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// rejectingClient fails Prepare the way a provider does for a media type it
// does not accept.
type rejectingClient struct {
	scriptedClient
}

func (c *rejectingClient) Prepare(
	_ context.Context,
	in llm.Input,
) (llm.Request, error) {
	c.inputs = append(c.inputs, in)
	return llm.Request{}, errors.New("test: application/pdf is not supported")
}

func TestRefProviderRejectionReturnsPrepareError(t *testing.T) {
	ref := refFor("upl_1", refBytes)
	resolver := &countingResolver{files: map[string][]byte{"upl_1": refBytes}}
	client := &rejectingClient{}

	out := runWithRef(
		t,
		Runner{Client: client, ContentResolver: resolver},
		refMessage(ref),
	)

	if out.Stop.Kind != StopFailed ||
		!strings.Contains(out.Stop.Reason, "application/pdf is not supported") {
		t.Fatalf("stop = %+v", out.Stop)
	}
	if strings.Contains(out.Stop.Reason, "content reference") {
		t.Fatalf("stop blames the reference: %q", out.Stop.Reason)
	}
}
