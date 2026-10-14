// Package realtime describes live model connections. It does not capture devices,
// execute tools, own durable conversations, or establish what a listener heard.
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type Client interface {
	Connect(context.Context, Config) (Session, error)
}
type Session interface {
	Info() SessionInfo
	Send(context.Context, Command) error
	Receive(context.Context) (Event, error)
	Close() error
}
type Config struct {
	Model, Instructions, Voice string
	Tools                      []tool.Definition
	InputFormat, OutputFormat  content.AudioFormat
}
type SessionInfo struct {
	Provider, Model, SessionID                  string
	InputFormat, OutputFormat                   content.AudioFormat
	NeedsResponseAfterTools, PlaybackTruncation bool
}
type AudioFrame struct {
	Data   []byte
	Format content.AudioFormat
}
type ToolCall struct {
	ID, Name  string
	Arguments json.RawMessage
}
type ToolResult struct {
	CallID, Name string
	Output       json.RawMessage
}
type PlaybackPosition struct {
	ResponseID, ItemID string
	ContentIndex       int
	PlayedSamples      int64
}
type CommandKind string

const (
	CommandAudio      CommandKind = "audio"
	CommandText       CommandKind = "text"
	CommandToolResult CommandKind = "tool_result"
	CommandRespond    CommandKind = "respond"
	CommandAudioEnd   CommandKind = "audio_end"
	CommandInterrupt  CommandKind = "interrupt"
)

type Command struct {
	Kind       CommandKind
	Audio      *AudioFrame
	Text       string
	ToolResult *ToolResult
	Playback   *PlaybackPosition
}
type Speaker string

const (
	SpeakerUser      Speaker = "user"
	SpeakerAssistant Speaker = "assistant"
)

type TranscriptMode string

const (
	TranscriptModeDelta   TranscriptMode = "delta"
	TranscriptModeReplace TranscriptMode = "replace"
)

type Transcript struct {
	Speaker Speaker
	Text    string
	Mode    TranscriptMode
	Final   bool
}
type EventKind string

const (
	EventAudio             EventKind = "audio"
	EventTranscript        EventKind = "transcript"
	EventSpeechStarted     EventKind = "speech_started"
	EventSpeechStopped     EventKind = "speech_stopped"
	EventInterrupted       EventKind = "interrupted"
	EventToolCall          EventKind = "tool_call"
	EventToolCanceled      EventKind = "tool_canceled"
	EventResponseStarted   EventKind = "response_started"
	EventResponseCompleted EventKind = "response_completed"
	EventUsage             EventKind = "usage"
	EventError             EventKind = "error"
)

type Event struct {
	Kind                   EventKind
	ID, ResponseID, ItemID string
	Audio                  *AudioFrame
	Transcript             *Transcript
	ToolCall               *ToolCall
	CanceledCallIDs        []string
	Usage                  *llm.Usage
	Error                  *Error
}
type ErrorCategory string

const (
	ErrorInvalidRequest ErrorCategory = "invalid_request"
	ErrorUnsupported    ErrorCategory = "unsupported"
	ErrorAuthentication ErrorCategory = "authentication"
	ErrorRateLimited    ErrorCategory = "rate_limited"
	ErrorProvider       ErrorCategory = "provider"
	ErrorCanceled       ErrorCategory = "canceled"
	ErrorTransport      ErrorCategory = "transport"
	ErrorProtocol       ErrorCategory = "protocol"
)

type Error struct {
	Category ErrorCategory
	Code     string
	Cause    error
}

func (e *Error) Error() string {
	msg := "realtime: " + string(e.Category)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}
func (e *Error) Unwrap() error          { return e.Cause }
func Fail(category ErrorCategory) error { return &Error{Category: category} }

// Connection is the bounded JSON transport seam. Implementations must support
// one reader and serialized writes, and Close must unblock an outstanding read.
type Connection interface {
	Read(context.Context) ([]byte, error)
	Write(context.Context, []byte) error
	Close() error
}
type DialFunc func(context.Context, string, http.Header, *http.Client) (Connection, error)

const MaxAudioBytes = 64 * 1024
const MaxMessageBytes = 2 * 1024 * 1024
const MaxToolBytes = 64 * 1024
const MaxPendingTools = 32

func (c Config) Validate() error {
	if len(c.Instructions) > MaxToolBytes || len(c.Voice) > 256 || len(c.Tools) > MaxPendingTools {
		return Fail(ErrorInvalidRequest)
	}
	names := map[string]bool{}
	for _, t := range c.Tools {
		if t.ProviderTool != nil {
			return Fail(ErrorUnsupported)
		}
		var schema map[string]any
		if t.Name == "" || len(t.Name) > 256 || len(t.Description) > MaxToolBytes || names[t.Name] || len(t.Schema) > MaxToolBytes || json.Unmarshal(t.Schema, &schema) != nil || schema["type"] != "object" {
			return Fail(ErrorInvalidRequest)
		}
		names[t.Name] = true
	}
	return nil
}
func (c Command) Validate(format content.AudioFormat) error {
	if c.Playback != nil {
		return Fail(ErrorUnsupported)
	}
	count := 0
	if c.Audio != nil {
		count++
	}
	if c.Text != "" {
		count++
	}
	if c.ToolResult != nil {
		count++
	}
	switch c.Kind {
	case CommandAudio:
		if count != 1 || c.Audio == nil || len(c.Audio.Data) == 0 || len(c.Audio.Data) > MaxAudioBytes || len(c.Audio.Data)%2 != 0 || c.Audio.Format != format {
			return Fail(ErrorInvalidRequest)
		}
	case CommandText:
		if count != 1 || c.Text == "" || len(c.Text) > MaxToolBytes {
			return Fail(ErrorInvalidRequest)
		}
	case CommandToolResult:
		if count != 1 || c.ToolResult == nil || c.ToolResult.CallID == "" || len(c.ToolResult.Output) > MaxToolBytes {
			return Fail(ErrorInvalidRequest)
		}
		var obj map[string]any
		if json.Unmarshal(c.ToolResult.Output, &obj) != nil || obj == nil {
			return Fail(ErrorInvalidRequest)
		}
	case CommandRespond, CommandAudioEnd, CommandInterrupt:
		if count != 0 {
			return Fail(ErrorInvalidRequest)
		}
	default:
		return Fail(ErrorUnsupported)
	}
	return nil
}

var ErrClosed = errors.New("realtime: closed")
