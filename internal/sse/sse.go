// Package sse reads framed JSON events without provider-specific interpretation.
// It does not accumulate model results or retry connections. The reader is
// adapted from Dive (65c393f), Apache-2.0.
package sse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxEventBytes bounds one line and one event's joined data. It leaves room
// for events that carry inline images while stopping a stream with no line
// breaks from exhausting memory.
const MaxEventBytes = 64 << 20

// ErrEventTooLarge reports a line or event over MaxEventBytes.
var ErrEventTooLarge = errors.New("sse: event exceeds size limit")

// ErrMalformedEvent reports event data that does not decode as JSON. Like
// ErrEventTooLarge, it means the provider broke the protocol, not that the
// connection failed.
var ErrMalformedEvent = errors.New("sse: malformed event")

// ServerSentEventsCallback observes raw lines before framing. An error stops
// the reader and is returned by Err.
type ServerSentEventsCallback func(line string) error

// ServerSentEventsReader decodes framed data into T, joining multiline data
// and recognizing [DONE]. It borrows a stream that the caller must close.
type ServerSentEventsReader[T any] struct {
	body        io.ReadCloser
	reader      *bufio.Reader
	err         error
	done        bool
	sseCallback ServerSentEventsCallback
	maxBytes    int
}

// NewServerSentEventsReader borrows stream and starts before the first event.
func NewServerSentEventsReader[T any](
	stream io.ReadCloser,
) *ServerSentEventsReader[T] {
	return &ServerSentEventsReader[T]{
		body:     stream,
		reader:   bufio.NewReader(stream),
		maxBytes: MaxEventBytes,
	}
}

// readLine returns the next line with its newline, failing with
// ErrEventTooLarge once the line passes the limit.
func (s *ServerSentEventsReader[T]) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := s.reader.ReadSlice('\n')
		if len(line)+len(chunk) > s.maxBytes {
			return nil, ErrEventTooLarge
		}
		line = append(line, chunk...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// WithSSECallback sets an optional callback that is called for each line of the
// server-sent events stream.
func (s *ServerSentEventsReader[T]) WithSSECallback(
	callback ServerSentEventsCallback,
) *ServerSentEventsReader[T] {
	s.sseCallback = callback
	return s
}

// Err reports a read, callback, or JSON failure; clean end-of-stream is nil.
func (s *ServerSentEventsReader[T]) Err() error {
	return s.err
}

// Next returns one decoded data event. False means termination; inspect Err to
// distinguish failure from [DONE] or EOF.
func (s *ServerSentEventsReader[T]) Next() (T, bool) {
	var zero T
	if s.done || s.err != nil {
		return zero, false
	}
	var data []byte
	for {
		line, err := s.readLine()
		if err != nil && err != io.EOF {
			s.err = err
			return zero, false
		}
		if len(line) > 0 && s.sseCallback != nil {
			if e := s.sseCallback(string(line)); e != nil {
				s.err = e
				return zero, false
			}
		}
		line = bytes.TrimSuffix(
			bytes.TrimSuffix(
				line,
				[]byte("\n"),
			),
			[]byte("\r"),
		)
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line[5:], []byte(" "))
			if len(data)+len(value) > s.maxBytes {
				s.err = ErrEventTooLarge
				return zero, false
			}
			data = append(data, value...)
			data = append(data, '\n')
		}
		if err == io.EOF {
			s.done = true
		}
		if len(line) != 0 && !s.done {
			continue
		}
		if len(data) == 0 {
			if s.done {
				return zero, false
			}
			continue
		}
		data = bytes.TrimSuffix(data, []byte("\n"))
		if bytes.Equal(data, []byte("[DONE]")) {
			s.done = true
			return zero, false
		}
		var event T
		if err := json.Unmarshal(data, &event); err != nil {
			s.err = fmt.Errorf("%w: %w", ErrMalformedEvent, err)
			return zero, false
		}
		return event, true
	}
}
