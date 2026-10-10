// Package realtimewire keeps socket lifecycle and bounds common while retaining
// each provider's event grammar. Pending tool IDs fence duplicate effects.
package realtimewire

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/realtime"
)

type socket struct{ conn *websocket.Conn }

func (s *socket) Read(ctx context.Context) ([]byte, error) { _, b, e := s.conn.Read(ctx); return b, e }
func (s *socket) Write(ctx context.Context, b []byte) error {
	return s.conn.Write(ctx, websocket.MessageText, b)
}
func (s *socket) Close() error { return s.conn.CloseNow() }
func Dial(ctx context.Context, endpoint string, headers http.Header, client *http.Client) (realtime.Connection, error) {
	c, r, e := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: headers, HTTPClient: client})
	if e != nil {
		category := realtime.ErrorTransport
		if r != nil {
			switch r.StatusCode {
			case 401, 403:
				category = realtime.ErrorAuthentication
			case 429:
				category = realtime.ErrorRateLimited
			}
		}
		return nil, realtime.Fail(category)
	}
	c.SetReadLimit(realtime.MaxMessageBytes)
	return &socket{c}, nil
}

type Session struct {
	conn            realtime.Connection
	info            realtime.SessionInfo
	mu              sync.Mutex
	readMu          sync.Mutex
	writeMu         sync.Mutex
	pending         map[string]string
	seen            map[string]bool
	allowed         map[string]bool
	events          []realtime.Event
	response        string
	sequence        int
	inputSequence   int
	inputID         string
	inputText       string
	active, discard bool
	stop            func() bool
	closed          bool
}

func NewSession(ctx context.Context, conn realtime.Connection, info realtime.SessionInfo) *Session {
	s := &Session{conn: conn, info: info, pending: map[string]string{}, seen: map[string]bool{}, allowed: map[string]bool{}}
	s.stop = context.AfterFunc(ctx, func() { conn.Close() })
	return s
}
func (s *Session) Info() realtime.SessionInfo { s.mu.Lock(); defer s.mu.Unlock(); return s.info }
func (s *Session) Close() error {
	s.conn.Close() // Unblock transport operations before acquiring state.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if s.stop != nil {
		s.stop()
	}
	return s.conn.Close()
}
func (s *Session) Write(ctx context.Context, value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return realtime.Fail(realtime.ErrorInvalidRequest)
	}
	if len(b) > realtime.MaxMessageBytes {
		return realtime.Fail(realtime.ErrorInvalidRequest)
	}
	if e = s.conn.Write(ctx, b); e != nil {
		return classify(ctx, e)
	}
	return nil
}
func classify(ctx context.Context, e error) error {
	if ctx.Err() != nil {
		return realtime.Fail(realtime.ErrorCanceled)
	}
	if e == io.EOF || websocket.CloseStatus(e) == websocket.StatusNormalClosure {
		return io.EOF
	}
	return realtime.Fail(realtime.ErrorTransport)
}
func (s *Session) Setup(ctx context.Context, setup any, ack string) error {
	if e := s.Write(ctx, setup); e != nil {
		s.Close()
		return e
	}
	// Initial informational frames are discarded, but only a bounded number may
	// precede acknowledgment. Connect never exposes a half-configured session.
	for i := 0; i < 32; i++ {
		m, e := s.read(ctx)
		if e != nil {
			s.Close()
			return e
		}
		if s.info.Provider == "google" {
			if _, ok := m["setupComplete"]; ok {
				return nil
			}
		} else if str(m, "type") == ack {
			if session, ok := m["session"].(map[string]any); ok {
				s.info.SessionID = str(session, "id")
				if len(s.info.SessionID) > 256 {
					s.Close()
					return realtime.Fail(realtime.ErrorProtocol)
				}
				for _, direction := range []string{"input", "output"} {
					format := object(object(object(session, "audio"), direction), "format")
					if format != nil {
						rate, ok := format["rate"].(float64)
						if str(format, "type") != "audio/pcm" || !ok || rate != 24000 {
							s.Close()
							return realtime.Fail(realtime.ErrorUnsupported)
						}
					}
				}
			}
			return nil
		}
		if _, ok := m["error"]; ok {
			s.Close()
			return realtime.Fail(realtime.ErrorProvider)
		}
	}
	s.Close()
	return realtime.Fail(realtime.ErrorProtocol)
}
func (s *Session) read(ctx context.Context) (map[string]any, error) {
	b, e := s.conn.Read(ctx)
	if e != nil {
		return nil, classify(ctx, e)
	}
	if len(b) > realtime.MaxMessageBytes {
		return nil, realtime.Fail(realtime.ErrorProtocol)
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m == nil {
		return nil, realtime.Fail(realtime.ErrorProtocol)
	}
	return m, nil
}
func (s *Session) Send(ctx context.Context, c realtime.Command) error {
	if ctx.Err() != nil {
		s.Close()
		return realtime.Fail(realtime.ErrorCanceled)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return realtime.ErrClosed
	}
	if e := c.Validate(s.info.InputFormat); e != nil {
		return e
	}
	var wire any
	if s.info.Provider == "google" {
		switch c.Kind {
		case realtime.CommandAudio:
			wire = map[string]any{"realtimeInput": map[string]any{"audio": map[string]any{"data": base64.StdEncoding.EncodeToString(c.Audio.Data), "mimeType": fmt.Sprintf("audio/pcm;rate=%d", c.Audio.Format.SampleRateHz)}}}
		case realtime.CommandAudioEnd:
			wire = map[string]any{"realtimeInput": map[string]any{"audioStreamEnd": true}}
		case realtime.CommandText:
			wire = map[string]any{"clientContent": map[string]any{"turns": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": c.Text}}}}, "turnComplete": true}}
		case realtime.CommandToolResult:
			name, ok := s.pending[c.ToolResult.CallID]
			if !ok || name != c.ToolResult.Name {
				return realtime.Fail(realtime.ErrorInvalidRequest)
			}
			wire = map[string]any{"toolResponse": map[string]any{"functionResponses": []any{map[string]any{"id": c.ToolResult.CallID, "name": name, "response": json.RawMessage(c.ToolResult.Output), "scheduling": "WHEN_IDLE"}}}}
		default:
			return realtime.Fail(realtime.ErrorUnsupported)
		}
	} else {
		switch c.Kind {
		case realtime.CommandAudio:
			wire = map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(c.Audio.Data)}
		case realtime.CommandText:
			wire = map[string]any{"type": "conversation.item.create", "item": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": c.Text}}}}
		case realtime.CommandToolResult:
			name, ok := s.pending[c.ToolResult.CallID]
			if !ok || name != c.ToolResult.Name {
				return realtime.Fail(realtime.ErrorInvalidRequest)
			}
			wire = map[string]any{"type": "conversation.item.create", "item": map[string]any{"type": "function_call_output", "call_id": c.ToolResult.CallID, "output": string(c.ToolResult.Output)}}
		case realtime.CommandRespond:
			if len(s.pending) != 0 || s.active {
				return realtime.Fail(realtime.ErrorInvalidRequest)
			}
			wire = map[string]any{"type": "response.create"}
		case realtime.CommandInterrupt:
			if !s.active {
				return nil
			}
			wire = map[string]any{"type": "response.cancel"}
		default:
			return realtime.Fail(realtime.ErrorUnsupported)
		}
	}
	s.mu.Unlock()
	err := s.Write(ctx, wire)
	s.mu.Lock()
	if err != nil {
		s.closed = true
		s.conn.Close()
		return err
	}
	if c.Kind == realtime.CommandToolResult {
		delete(s.pending, c.ToolResult.CallID)
	}
	if c.Kind == realtime.CommandRespond {
		s.active = true
	}
	return nil
}
func str(m map[string]any, k string) string            { v, _ := m[k].(string); return v }
func object(m map[string]any, k string) map[string]any { v, _ := m[k].(map[string]any); return v }
func (s *Session) enqueue(e realtime.Event) error {
	if len(s.events) >= 128 {
		return realtime.Fail(realtime.ErrorProtocol)
	}
	s.events = append(s.events, e)
	return nil
}
func (s *Session) audio(data string, response, item string) error {
	b, e := base64.StdEncoding.DecodeString(data)
	if e != nil || len(b) == 0 || len(b) > realtime.MaxAudioBytes || len(b)%2 != 0 {
		return realtime.Fail(realtime.ErrorProtocol)
	}
	return s.enqueue(realtime.Event{Kind: realtime.EventAudio, ResponseID: response, ItemID: item, Audio: &realtime.AudioFrame{Data: b, Format: s.info.OutputFormat}})
}
func (s *Session) call(id, name string, args any, response, item string) error {
	if id == "" || len(id) > 256 || name == "" || !s.allowed[name] {
		return realtime.Fail(realtime.ErrorProtocol)
	}
	if s.seen[id] {
		return nil
	}
	if len(s.pending) >= realtime.MaxPendingTools || len(s.seen) >= 4096 {
		return realtime.Fail(realtime.ErrorProtocol)
	}
	b, e := json.Marshal(args)
	if raw, ok := args.(string); ok {
		b = []byte(raw)
	}
	var obj map[string]any
	if e != nil || len(b) > realtime.MaxToolBytes || json.Unmarshal(b, &obj) != nil || obj == nil {
		return realtime.Fail(realtime.ErrorProtocol)
	}
	s.pending[id] = name
	s.seen[id] = true
	return s.enqueue(realtime.Event{Kind: realtime.EventToolCall, ResponseID: response, ItemID: item, ToolCall: &realtime.ToolCall{ID: id, Name: name, Arguments: b}})
}
func (s *Session) Receive(ctx context.Context) (realtime.Event, error) {
	if ctx.Err() != nil {
		s.Close()
		return realtime.Event{}, realtime.Fail(realtime.ErrorCanceled)
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	for {
		s.mu.Lock()
		if len(s.events) > 0 {
			e := s.events[0]
			s.events[0] = realtime.Event{}
			s.events = s.events[1:]
			s.mu.Unlock()
			return e, nil
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return realtime.Event{}, realtime.ErrClosed
		}
		m, e := s.read(ctx)
		if e != nil {
			s.Close()
			return realtime.Event{}, e
		}
		s.mu.Lock()
		if s.info.Provider == "google" {
			e = s.google(m)
		} else {
			e = s.grok(m)
		}
		s.mu.Unlock()
		if e != nil {
			s.Close()
			return realtime.Event{}, e
		}
	}
}
func (s *Session) transcript(text string, speaker realtime.Speaker, mode realtime.TranscriptMode, final bool, response, item string) error {
	if len(text) > realtime.MaxToolBytes {
		return realtime.Fail(realtime.ErrorProtocol)
	}
	return s.enqueue(realtime.Event{Kind: realtime.EventTranscript, ResponseID: response, ItemID: item, Transcript: &realtime.Transcript{Text: text, Speaker: speaker, Mode: mode, Final: final}})
}
func (s *Session) google(m map[string]any) error {
	if _, ok := m["error"]; ok {
		return s.enqueue(realtime.Event{Kind: realtime.EventError, Error: &realtime.Error{Category: realtime.ErrorProvider}})
	}
	if activity := object(m, "voiceActivity"); activity != nil {
		kind := str(activity, "type")
		if kind == "ACTIVITY_START" {
			s.inputSequence++
			s.inputID = fmt.Sprintf("local-input-%d", s.inputSequence)
			s.inputText = ""
			if err := s.enqueue(realtime.Event{Kind: realtime.EventSpeechStarted, ItemID: s.inputID}); err != nil {
				return err
			}
		} else if kind == "ACTIVITY_END" {
			if err := s.enqueue(realtime.Event{Kind: realtime.EventSpeechStopped, ItemID: s.inputID}); err != nil {
				return err
			}
		}
	}
	if usage := object(m, "usageMetadata"); usage != nil {
		if e := s.usage(usage, true); e != nil {
			return e
		}
	}
	if _, ok := m["goAway"]; ok {
		return realtime.Fail(realtime.ErrorProvider)
	}
	if calls := object(m, "toolCall"); calls != nil {
		for _, v := range array(calls, "functionCalls") {
			c, _ := v.(map[string]any)
			if e := s.call(str(c, "id"), str(c, "name"), c["args"], s.response, ""); e != nil {
				return e
			}
		}
	}
	if cancel := object(m, "toolCallCancellation"); cancel != nil {
		var ids []string
		for _, v := range array(cancel, "ids") {
			id, _ := v.(string)
			if _, ok := s.pending[id]; ok {
				delete(s.pending, id)
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			if e := s.enqueue(realtime.Event{Kind: realtime.EventToolCanceled, CanceledCallIDs: ids}); e != nil {
				return e
			}
		}
	}
	c := object(m, "serverContent")
	if c == nil {
		return nil
	}
	if interrupted, _ := c["interrupted"].(bool); interrupted {
		s.discard = true
		s.active = false
		if e := s.enqueue(realtime.Event{Kind: realtime.EventInterrupted, ResponseID: s.response}); e != nil {
			return e
		}
	}
	if !s.discard {
		if turn := object(c, "modelTurn"); turn != nil {
			if !s.active {
				s.sequence++
				s.response = fmt.Sprintf("local-%d", s.sequence)
				s.active = true
				if e := s.enqueue(realtime.Event{Kind: realtime.EventResponseStarted, ResponseID: s.response}); e != nil {
					return e
				}
			}
			for _, v := range array(turn, "parts") {
				part, _ := v.(map[string]any)
				if audio := object(part, "inlineData"); audio != nil {
					if mime := str(audio, "mimeType"); mime != "audio/pcm;rate=24000" && mime != "audio/pcm" {
						return realtime.Fail(realtime.ErrorProtocol)
					}
					if e := s.audio(str(audio, "data"), s.response, s.response); e != nil {
						return e
					}
				}
			}
		}
		if t := object(c, "outputTranscription"); t != nil {
			if e := s.transcript(str(t, "text"), realtime.SpeakerAssistant, realtime.TranscriptModeDelta, boolean(t, "finished"), s.response, s.response); e != nil {
				return e
			}
		}
	}
	if t := object(c, "interimInputTranscription"); t != nil {
		if s.inputID == "" {
			s.inputSequence++
			s.inputID = fmt.Sprintf("local-input-%d", s.inputSequence)
			s.inputText = ""
		}
		if err := s.transcript(str(t, "text"), realtime.SpeakerUser, realtime.TranscriptModeReplace, false, "", s.inputID); err != nil {
			return err
		}
	}
	if t := object(c, "inputTranscription"); t != nil {
		if s.inputID == "" {
			s.inputSequence++
			s.inputID = fmt.Sprintf("local-input-%d", s.inputSequence)
			s.inputText = ""
		}
		s.inputText += str(t, "text")
		if e := s.transcript(s.inputText, realtime.SpeakerUser, realtime.TranscriptModeReplace, boolean(t, "finished"), "", s.inputID); e != nil {
			return e
		}
	}
	if t := object(c, "inputTranscription"); t != nil && boolean(t, "finished") {
		s.inputID = ""
		s.inputText = ""
	}
	if done, _ := c["turnComplete"].(bool); done {
		if e := s.enqueue(realtime.Event{Kind: realtime.EventResponseCompleted, ResponseID: s.response}); e != nil {
			return e
		}
		s.active = false
		s.discard = false
	}
	return nil
}
func array(m map[string]any, k string) []any { v, _ := m[k].([]any); return v }
func (s *Session) grok(m map[string]any) error {
	kind := str(m, "type")
	r, i := str(m, "response_id"), str(m, "item_id")
	e := realtime.Event{ID: str(m, "event_id"), ResponseID: r, ItemID: i}
	switch kind {
	case "response.created":
		response := object(m, "response")
		s.response = str(response, "id")
		s.active = true
		e.Kind = realtime.EventResponseStarted
		e.ResponseID = s.response
	case "response.done":
		s.active = false
		if usage := object(object(m, "response"), "usage"); usage != nil {
			if err := s.usage(usage, false); err != nil {
				return err
			}
		}
		e.Kind = realtime.EventResponseCompleted
		e.ResponseID = str(object(m, "response"), "id")
	case "input_audio_buffer.speech_started":
		e.Kind = realtime.EventSpeechStarted
		if s.response != "" {
			if err := s.enqueue(realtime.Event{Kind: realtime.EventInterrupted, ResponseID: s.response}); err != nil {
				return err
			}
		}
	case "input_audio_buffer.speech_stopped":
		e.Kind = realtime.EventSpeechStopped
	case "response.output_audio.delta", "response.audio.delta":
		return s.audio(str(m, "delta"), r, i)
	case "response.output_audio_transcript.delta", "response.audio_transcript.delta":
		return s.transcript(str(m, "delta"), realtime.SpeakerAssistant, realtime.TranscriptModeDelta, false, r, i)
	case "response.output_audio_transcript.done", "response.audio_transcript.done":
		return s.transcript(str(m, "transcript"), realtime.SpeakerAssistant, realtime.TranscriptModeReplace, true, r, i)
	case "conversation.item.input_audio_transcription.completed":
		return s.transcript(str(m, "transcript"), realtime.SpeakerUser, realtime.TranscriptModeReplace, true, r, i)
	case "conversation.item.input_audio_transcription.updated":
		return s.transcript(str(m, "transcript"), realtime.SpeakerUser, realtime.TranscriptModeReplace, false, r, i)
	case "response.function_call_arguments.done":
		return s.call(str(m, "call_id"), str(m, "name"), m["arguments"], r, i)
	case "error":
		e.Kind = realtime.EventError
		e.Error = &realtime.Error{Category: realtime.ErrorProvider}
	default:
		return nil
	}
	return s.enqueue(e)
}

func (s *Session) usage(m map[string]any, google bool) error {
	input, output := "input_tokens", "output_tokens"
	if google {
		input, output = "promptTokenCount", "responseTokenCount"
	}
	a, aok := m[input].(float64)
	b, bok := m[output].(float64)
	if !aok || !bok {
		return nil
	}
	if a < 0 || b < 0 || a > 1e9 || b > 1e9 {
		return realtime.Fail(realtime.ErrorProtocol)
	}
	u := &llm.Usage{InputTokens: int(a), OutputTokens: int(b), InputModalityTokenDetailsIncomplete: true, OutputModalityTokenDetailsIncomplete: true}
	return s.enqueue(realtime.Event{Kind: realtime.EventUsage, ResponseID: s.response, Usage: u})
}

func boolean(m map[string]any, k string) bool { v, _ := m[k].(bool); return v }
