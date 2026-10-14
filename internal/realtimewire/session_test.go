package realtimewire

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi/realtime"
	"github.com/deepnoodle-ai/dashi/tool"
)

type fakeConn struct {
	mu     sync.Mutex
	reads  chan []byte
	writes [][]byte
	done   chan struct{}
	once   sync.Once
}

func fake(messages ...string) *fakeConn {
	f := &fakeConn{reads: make(chan []byte, 32), done: make(chan struct{})}
	for _, m := range messages {
		f.reads <- []byte(m)
	}
	return f
}
func (f *fakeConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-f.reads:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return nil, io.EOF
	}
}
func (f *fakeConn) Write(ctx context.Context, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte{}, b...))
	return ctx.Err()
}
func (f *fakeConn) Close() error { f.once.Do(func() { close(f.done) }); return nil }
func connect(t *testing.T, google bool, f *fakeConn) (realtime.Session, context.CancelFunc) {
	t.Helper()
	c, e := New(Config{APIKey: "fake", Dial: func(context.Context, string, http.Header, *http.Client) (realtime.Connection, error) { return f, nil }}, google)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, e := c.Connect(ctx, realtime.Config{Tools: []tool.Definition{{Name: "ask_agent", Schema: json.RawMessage(`{"type":"object"}`)}}})
	if e != nil {
		t.Fatal(e)
	}
	return s, cancel
}
func next(t *testing.T, s realtime.Session, kind realtime.EventKind) realtime.Event {
	t.Helper()
	e, err := s.Receive(context.Background())
	if err != nil || e.Kind != kind {
		t.Fatalf("event %v error %v wanted %v", e.Kind, err, kind)
	}
	return e
}
func TestGrokToolLifecycleAndTranscripts(t *testing.T) {
	f := fake(`{"type":"session.updated","session":{"id":"s"}}`, `{"type":"response.function_call_arguments.done","call_id":"c","name":"ask_agent","arguments":"{}"}`, `{"type":"conversation.item.input_audio_transcription.updated","item_id":"i","transcript":"hello"}`, `{"type":"conversation.item.input_audio_transcription.completed","item_id":"i","transcript":"hello there"}`)
	s, _ := connect(t, false, f)
	if !s.Info().NeedsResponseAfterTools {
		t.Fatal("missing explicit response")
	}
	next(t, s, realtime.EventToolCall)
	if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandRespond}); err == nil {
		t.Fatal("respond allowed with pending call")
	}
	result := realtime.Command{Kind: realtime.CommandToolResult, ToolResult: &realtime.ToolResult{CallID: "c", Name: "ask_agent", Output: json.RawMessage(`{"accepted":true}`)}}
	if err := s.Send(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), result); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandRespond}); err != nil {
		t.Fatal(err)
	}
	e := next(t, s, realtime.EventTranscript)
	if e.Transcript.Mode != realtime.TranscriptModeReplace {
		t.Fatal("cumulative transcript appended")
	}
	e = next(t, s, realtime.EventTranscript)
	if !e.Transcript.Final {
		t.Fatal("missing final")
	}
}
func TestGeminiInterruptionFencesUntilCompletion(t *testing.T) {
	f := fake(`{"setupComplete":{}}`, `{"serverContent":{"modelTurn":{"parts":[{"inlineData":{"data":"AAA=","mimeType":"audio/pcm;rate=24000","mimeType":"audio/pcm;rate=24000"}}]}}}`, `{"serverContent":{"interrupted":true}}`, `{"serverContent":{"modelTurn":{"parts":[{"inlineData":{"data":"AAA=","mimeType":"audio/pcm;rate=24000"}}]}}}`, `{"serverContent":{"turnComplete":true}}`, `{"serverContent":{"modelTurn":{"parts":[{"inlineData":{"data":"AAA=","mimeType":"audio/pcm;rate=24000"}}]}}}`)
	s, _ := connect(t, true, f)
	a := next(t, s, realtime.EventResponseStarted)
	next(t, s, realtime.EventAudio)
	next(t, s, realtime.EventInterrupted)
	next(t, s, realtime.EventResponseCompleted)
	b := next(t, s, realtime.EventResponseStarted)
	if a.ResponseID == b.ResponseID {
		t.Fatal("response identity reused")
	}
	next(t, s, realtime.EventAudio)
	if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandInterrupt}); err == nil {
		t.Fatal("unsupported cancellation succeeded")
	}
}
func TestCancellationUnblocksReceive(t *testing.T) {
	s, cancel := connect(t, false, fake(`{"type":"session.updated"}`))
	cancel()
	_, err := s.Receive(context.Background())
	if err == nil {
		t.Fatal("receive survived close")
	}
}
func TestMalformedPayloadSanitized(t *testing.T) {
	s, _ := connect(t, false, fake(`{"type":"session.updated"}`, `secret broken payload`))
	_, err := s.Receive(context.Background())
	var e *realtime.Error
	if !errors.As(err, &e) || e.Category != realtime.ErrorProtocol || strings.Contains(err.Error(), "secret") || e.Cause == nil {
		t.Fatalf("unsafe error %v", err)
	}
}
func TestCanceledToolCannotBeAnswered(t *testing.T) {
	s, _ := connect(t, true, fake(`{"setupComplete":{}}`, `{"toolCall":{"functionCalls":[{"id":"c","name":"ask_agent","args":{}}]}}`, `{"toolCallCancellation":{"ids":["c"]}}`))
	next(t, s, realtime.EventResponseStarted)
	next(t, s, realtime.EventToolCall)
	next(t, s, realtime.EventToolCanceled)
	if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandToolResult, ToolResult: &realtime.ToolResult{CallID: "c", Name: "ask_agent", Output: json.RawMessage(`{}`)}}); err == nil {
		t.Fatal("canceled call answered")
	}
}

type blockedWrite struct {
	*fakeConn
	started chan struct{}
}

func (f *blockedWrite) Write(ctx context.Context, b []byte) error {
	close(f.started)
	select {
	case <-f.done:
		return io.EOF
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestCloseUnblocksWriter(t *testing.T) {
	f := &blockedWrite{fakeConn: fake(), started: make(chan struct{})}
	s := NewSession(context.Background(), f, realtime.SessionInfo{Provider: "grok"})
	done := make(chan error, 1)
	go func() { done <- s.Send(context.Background(), realtime.Command{Kind: realtime.CommandRespond}) }()
	<-f.started
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("blocked writer succeeded")
	}
}
func TestCustomDestinationDoesNotAcquireEnvironmentKey(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "secret")
	t.Setenv("GOOGLE_API_KEY", "another")
	t.Setenv("XAI_API_KEY", "secret")
	for _, google := range []bool{true, false} {
		f := fake(`{"setupComplete":{},"type":"session.updated"}`)
		var auth string
		c, err := New(Config{Endpoint: "wss://example.test/voice", Dial: func(_ context.Context, _ string, h http.Header, _ *http.Client) (realtime.Connection, error) {
			auth = h.Get("Authorization") + h.Get("x-goog-api-key")
			return f, nil
		}}, google)
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.Connect(context.Background(), realtime.Config{})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		if auth != "" && auth != "Bearer " {
			t.Fatal("environment credential acquired")
		}
	}
}
func TestSetupAcknowledgmentAndAudioWire(t *testing.T) {
	f := fake(`{"type":"conversation.created"}`, `{"type":"session.updated"}`)
	s, _ := connect(t, false, f)
	frame := &realtime.AudioFrame{Data: []byte{1, 2}, Format: s.Info().InputFormat}
	if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandAudio, Audio: frame}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(f.writes[1], &m); err != nil {
		t.Fatal(err)
	}
	if str(m, "type") != "input_audio_buffer.append" || str(m, "audio") != "AQI=" {
		t.Fatal("wrong audio wire")
	}
	frame.Data = make([]byte, realtime.MaxAudioBytes+2)
	if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandAudio, Audio: frame}); err == nil {
		t.Fatal("unbounded input")
	}
	if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandInterrupt, Playback: &realtime.PlaybackPosition{ItemID: "x"}}); err == nil {
		t.Fatal("unqualified truncation accepted")
	}
}
func TestGeminiActivityTranscriptAndWrongMedia(t *testing.T) {
	f := fake(`{"setupComplete":{}}`, `{"voiceActivity":{"type":"ACTIVITY_START"}}`, `{"serverContent":{"interimInputTranscription":{"text":"hell"}}}`, `{"serverContent":{"inputTranscription":{"text":"hello","finished":true}}}`, `{"voiceActivity":{"type":"ACTIVITY_START"}}`, `{"serverContent":{"modelTurn":{"parts":[{"inlineData":{"data":"AAA=","mimeType":"image/png"}}]}}}`)
	s, _ := connect(t, true, f)
	a := next(t, s, realtime.EventSpeechStarted)
	next(t, s, realtime.EventTranscript)
	e := next(t, s, realtime.EventTranscript)
	if !e.Transcript.Final || e.Transcript.Text != "hello" || e.ItemID != a.ItemID {
		t.Fatal("transcription correlation")
	}
	b := next(t, s, realtime.EventSpeechStarted)
	if a.ItemID == b.ItemID {
		t.Fatal("utterance ID reused")
	}
	_, err := s.Receive(context.Background())
	if err == nil {
		t.Fatal("image treated as audio")
	}
}
func TestNegotiatedFormatMismatchFailsConnect(t *testing.T) {
	f := fake(`{"type":"session.updated","session":{"audio":{"output":{"format":{"type":"audio/pcm","rate":48000}}}}}`)
	c, _ := New(Config{APIKey: "fake", Dial: func(context.Context, string, http.Header, *http.Client) (realtime.Connection, error) { return f, nil }}, false)
	if _, err := c.Connect(context.Background(), realtime.Config{}); err == nil {
		t.Fatal("mismatched negotiated output accepted")
	}
}
func TestOtherProviderDefaultDoesNotAcquireCredential(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "secret")
	t.Setenv("XAI_API_KEY", "secret")
	for _, google := range []bool{true, false} {
		c, err := New(Config{Endpoint: defaultEndpoint(!google)}, google)
		if err != nil {
			t.Fatal(err)
		}
		if c.config.APIKey != "" {
			t.Fatal("credential crossed provider destination")
		}
	}
}
func TestSetupCanceledClosesConnection(t *testing.T) {
	f := fake()
	c, _ := New(Config{APIKey: "fake", Dial: func(ctx context.Context, _ string, _ http.Header, _ *http.Client) (realtime.Connection, error) {
		return f, nil
	}}, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Connect(ctx, realtime.Config{}); err == nil {
		t.Fatal("canceled connect succeeded")
	}
}
func TestConcurrentAudioWritesAndUsage(t *testing.T) {
	f := fake(`{"type":"session.updated"}`, `{"type":"response.created","response":{"id":"r"}}`, `{"type":"response.output_audio.delta","response_id":"r","item_id":"i","delta":"AQI="}`, `{"type":"response.done","response":{"id":"r","usage":{"input_tokens":12,"output_tokens":8}}}`)
	s, _ := connect(t, false, f)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandAudio, Audio: &realtime.AudioFrame{Data: []byte{1, 2}, Format: s.Info().InputFormat}})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(f.writes) != 9 {
		t.Fatal("lost write")
	}
	next(t, s, realtime.EventResponseStarted)
	e := next(t, s, realtime.EventAudio)
	if e.ResponseID != "r" || e.ItemID != "i" || len(e.Audio.Data) != 2 {
		t.Fatal("audio identity lost")
	}
	u := next(t, s, realtime.EventUsage)
	if u.Usage.InputTokens != 12 || !u.Usage.InputModalityTokenDetailsIncomplete {
		t.Fatal("usage facts fabricated")
	}
	next(t, s, realtime.EventResponseCompleted)
}
func TestGeminiTranscriptBeforeAudioHasStableIdentityAndFinality(t *testing.T) {
	s, _ := connect(t, true, fake(`{"setupComplete":{}}`, `{"serverContent":{"outputTranscription":{"text":"one"}}}`, `{"serverContent":{"modelTurn":{"parts":[{"inlineData":{"data":"AAA=","mimeType":"audio/pcm;rate=24000"}}]}}}`, `{"serverContent":{"generationComplete":true}}`, `{"serverContent":{"turnComplete":true}}`, `{"serverContent":{"outputTranscription":{"text":"two"}}}`, `{"serverContent":{"turnComplete":true}}`))
	a := next(t, s, realtime.EventResponseStarted)
	first := next(t, s, realtime.EventTranscript)
	audio := next(t, s, realtime.EventAudio)
	final := next(t, s, realtime.EventTranscript)
	if a.ResponseID == "" || first.ResponseID != a.ResponseID || audio.ResponseID != a.ResponseID || final.ResponseID != a.ResponseID || !final.Transcript.Final || final.Transcript.Mode != realtime.TranscriptModeReplace || final.Transcript.Text != "one" {
		t.Fatal("first response ownership/finality")
	}
	next(t, s, realtime.EventResponseCompleted)
	b := next(t, s, realtime.EventResponseStarted)
	second := next(t, s, realtime.EventTranscript)
	final = next(t, s, realtime.EventTranscript)
	if a.ResponseID == b.ResponseID || second.ResponseID != b.ResponseID || final.Transcript.Text != "two" || !final.Transcript.Final {
		t.Fatal("second transcript-only response ownership/finality")
	}
	next(t, s, realtime.EventResponseCompleted)
}
func TestGrokCancellationOwnsOnlyCanceledResponseTools(t *testing.T) {
	s, _ := connect(t, false, fake(`{"type":"session.updated"}`, `{"type":"response.created","response":{"id":"old"}}`, `{"type":"response.function_call_arguments.done","response_id":"old","call_id":"keep","name":"ask_agent","arguments":"{}"}`, `{"type":"response.done","response":{"id":"old","status":"completed"}}`, `{"type":"input_audio_buffer.speech_started"}`, `{"type":"response.created","response":{"id":"new"}}`, `{"type":"response.function_call_arguments.done","response_id":"new","call_id":"cancel1","name":"ask_agent","arguments":"{}"}`, `{"type":"response.function_call_arguments.done","response_id":"new","call_id":"cancel2","name":"ask_agent","arguments":"{}"}`, `{"type":"response.done","response":{"id":"new","status":"cancelled"}}`))
	next(t, s, realtime.EventResponseStarted)
	next(t, s, realtime.EventToolCall)
	next(t, s, realtime.EventResponseCompleted)
	next(t, s, realtime.EventSpeechStarted)
	next(t, s, realtime.EventResponseStarted)
	next(t, s, realtime.EventToolCall)
	next(t, s, realtime.EventToolCall)
	e := next(t, s, realtime.EventToolCanceled)
	if len(e.CanceledCallIDs) != 2 || e.ResponseID != "new" {
		t.Fatal("wrong tools canceled")
	}
	next(t, s, realtime.EventResponseCompleted)
	for _, id := range []string{"cancel1", "cancel2"} {
		if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandToolResult, ToolResult: &realtime.ToolResult{CallID: id, Name: "ask_agent", Output: json.RawMessage(`{}`)}}); err == nil {
			t.Fatal("canceled tool accepted")
		}
	}
	if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandToolResult, ToolResult: &realtime.ToolResult{CallID: "keep", Name: "ask_agent", Output: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal("completed response tool canceled", err)
	}
	if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandRespond}); err != nil {
		t.Fatal("response blocked after cancellation", err)
	}
}

func TestGeminiOutputTranscriptBoundsAccumulatedText(t *testing.T) {
	text := strings.Repeat("a", realtime.MaxToolBytes/2+1)
	message, err := json.Marshal(map[string]any{"serverContent": map[string]any{"outputTranscription": map[string]any{"text": text}}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := connect(t, true, fake(`{"setupComplete":{}}`, string(message), string(message)))
	next(t, s, realtime.EventResponseStarted)
	next(t, s, realtime.EventTranscript)
	if _, err := s.Receive(context.Background()); err == nil {
		t.Fatal("accepted accumulated transcript above limit")
	}
}

func TestGrokSpeechInterruptsOnlyActiveResponse(t *testing.T) {
	s, _ := connect(t, false, fake(`{"type":"session.updated"}`, `{"type":"response.created","response":{"id":"r1"}}`, `{"type":"response.done","response":{"id":"r1","status":"completed"}}`, `{"type":"input_audio_buffer.speech_started"}`, `{"type":"response.created","response":{"id":"r2"}}`, `{"type":"input_audio_buffer.speech_started"}`))
	next(t, s, realtime.EventResponseStarted)
	next(t, s, realtime.EventResponseCompleted)
	next(t, s, realtime.EventSpeechStarted) // r1 finished: no interruption.
	next(t, s, realtime.EventResponseStarted)
	if e := next(t, s, realtime.EventInterrupted); e.ResponseID != "r2" {
		t.Fatalf("interrupted %q, want r2", e.ResponseID)
	}
	next(t, s, realtime.EventSpeechStarted)
}

func TestExpiredCallContextKeepsSession(t *testing.T) {
	s, _ := connect(t, false, fake(`{"type":"session.updated"}`, `{"type":"response.created","response":{"id":"r1"}}`))
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	var re *realtime.Error
	if err := s.Send(expired, realtime.Command{Kind: realtime.CommandText, Text: "hi"}); !errors.As(err, &re) || re.Category != realtime.ErrorCanceled {
		t.Fatalf("send error %v, want canceled", err)
	}
	if _, err := s.Receive(expired); !errors.As(err, &re) || re.Category != realtime.ErrorCanceled {
		t.Fatalf("receive error %v, want canceled", err)
	}
	if err := s.Send(context.Background(), realtime.Command{Kind: realtime.CommandText, Text: "hi"}); err != nil {
		t.Fatalf("session closed by expired call context: %v", err)
	}
	next(t, s, realtime.EventResponseStarted)
}
func TestGrokErrorsSayWhatFailed(t *testing.T) {
	s, _ := connect(t, false, fake(`{"type":"session.updated"}`, `{"type":"error","error":{"type":"invalid_request_error","code":"bad_audio","message":"Audio format not supported"}}`))
	event, err := s.Receive(context.Background())
	if err != nil || event.Kind != realtime.EventError || event.Error.Code != "bad_audio" || event.Error.Cause == nil || event.Error.Cause.Error() != "Audio format not supported" {
		t.Fatalf("%+v %v", event.Error, err)
	}
	s, _ = connect(t, false, fake(`{"type":"session.updated"}`, `{"type":"response.output_audio.delta","delta":""}`))
	if _, err := s.Receive(context.Background()); err == nil || !strings.Contains(err.Error(), "audio chunk of 0 bytes") {
		t.Fatal(err)
	}
}
func TestLargeAudioChunkSplitsIntoFrames(t *testing.T) {
	chunk := base64.StdEncoding.EncodeToString(make([]byte, realtime.MaxAudioBytes+4000))
	s, _ := connect(t, false, fake(`{"type":"session.updated"}`, `{"type":"response.output_audio.delta","response_id":"r","item_id":"i","delta":"`+chunk+`"}`))
	a := next(t, s, realtime.EventAudio)
	b := next(t, s, realtime.EventAudio)
	if len(a.Audio.Data) != realtime.MaxAudioBytes || len(b.Audio.Data) != 4000 || b.ResponseID != "r" {
		t.Fatal(len(a.Audio.Data), len(b.Audio.Data), b.ResponseID)
	}
}
