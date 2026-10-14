// Ported from Dive (65c393f), Apache-2.0; adapted for Dashi.

package sse

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestServerSentEventsReaderPreservesRecordedEvents(t *testing.T) {
	data, err := os.Open("../../providers/openai/responses/testdata/events-hello.txt")
	if err != nil {
		t.Fatalf("open recorded SSE fixture: %v", err)
	}
	defer data.Close()

	reader := NewServerSentEventsReader[map[string]any](data)

	var events []map[string]any

	for {
		event, ok := reader.Next()
		if !ok {
			break
		}
		events = append(events, event)
	}

	if got := len(events); got != 17 {
		t.Fatalf("len(events) = %v, want %v", got, 17)
	}
	ev0 := events[0]
	if got := ev0["type"]; got != "response.created" {
		t.Fatalf(
			"ev0[\"type\"] = %v, want %v",
			got,
			"response.created",
		)
	}
	if got := ev0["sequence_number"]; got != float64(0) {
		t.Fatalf(
			"ev0[\"sequence_number\"] = %v, want %v",
			got,
			float64(0),
		)
	}

	ev1 := events[1]
	if got := ev1["type"]; got != "response.in_progress" {
		t.Fatalf(
			"ev1[\"type\"] = %v, want %v",
			got,
			"response.in_progress",
		)
	}
	if got := ev1["sequence_number"]; got != float64(1) {
		t.Fatalf(
			"ev1[\"sequence_number\"] = %v, want %v",
			got,
			float64(1),
		)
	}

	ev2 := events[2]
	if got := ev2["type"]; got != "response.output_item.added" {
		t.Fatalf(
			"ev2[\"type\"] = %v, want %v",
			got,
			"response.output_item.added",
		)
	}
	if got := ev2["sequence_number"]; got != float64(2) {
		t.Fatalf(
			"ev2[\"sequence_number\"] = %v, want %v",
			got,
			float64(2),
		)
	}
}

func TestServerSentEventsReaderFramesData(t *testing.T) {
	tests := []struct{ name, stream string }{
		{
			name:   "multiline CRLF",
			stream: ": keepalive\r\nevent: update\r\ndata:{\r\ndata: \"n\": 1}\r\n\r\n",
		},
		{name: "final event at EOF", stream: "data: {\"n\":1}"},
		{
			name:   "done marker",
			stream: "data: {\"n\":1}\n\ndata: [DONE]\n\ndata: {\"n\":2}\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewServerSentEventsReader[map[string]int](io.NopCloser(strings.NewReader(tt.stream)))
			v, ok := r.Next()
			if !ok || v["n"] != 1 {
				t.Fatalf(
					"framed event = %v, ok = %t, error = %v",
					v,
					ok,
					r.Err(),
				)
			}
			if _, ok := r.Next(); ok || r.Err() != nil {
				t.Fatalf(
					"unexpected extra event or error: %v",
					r.Err(),
				)
			}
		})
	}
}

func TestServerSentEventsReaderBoundsEventSize(t *testing.T) {
	small := func(stream string) *ServerSentEventsReader[map[string]any] {
		r := NewServerSentEventsReader[map[string]any](io.NopCloser(strings.NewReader(stream)))
		r.maxBytes = 64
		return r
	}
	long := `{"s":"` + strings.Repeat("x", 8000) + `"}`
	for name, stream := range map[string]string{
		"one line":       "data: " + long + "\n\n",
		"no line breaks": "data: " + long,
		"joined lines":   strings.Repeat(`data: "`+strings.Repeat("y", 20)+"\"\n", 10) + "\n",
	} {
		r := small(stream)
		if _, ok := r.Next(); ok || !errors.Is(r.Err(), ErrEventTooLarge) {
			t.Fatalf("%s: err = %v, want ErrEventTooLarge", name, r.Err())
		}
	}
	r := small("data: {\"n\":1}\n\n")
	if v, ok := r.Next(); !ok || v["n"] != 1.0 {
		t.Fatalf("small event = %v, ok = %t, err = %v", v, ok, r.Err())
	}
	big := NewServerSentEventsReader[map[string]any](io.NopCloser(strings.NewReader("data: " + long + "\n\n")))
	if v, ok := big.Next(); !ok || len(v["s"].(string)) != 8000 {
		t.Fatalf("event longer than the read buffer failed: %v", big.Err())
	}
}

func TestServerSentEventsReaderReportsMalformedJSON(t *testing.T) {
	r := NewServerSentEventsReader[map[string]int](io.NopCloser(strings.NewReader("data: {invalid}\n\n")))
	if _, ok := r.Next(); ok || r.Err() == nil {
		t.Fatal("malformed JSON disappeared")
	}
}
