package selective

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/decide/patterns/compact"
)

func TestFocusCountsEncodedBytes(t *testing.T) {
	text := strings.Repeat("<>&\"\\\n界", 2000)
	msgs := history()
	msgs[1] = user("task " + text)
	msgs[7] = user("request " + text)
	msgs = append(msgs, assistant(text+" recent end"))
	raw := focusState(msgs, 7, nil)
	var got focus
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for name, field := range map[string]struct {
		text string
		max  int
	}{
		"task": {got.Task, taskBytes}, "request": {got.LatestRequest, requestBytes},
		"recent": {got.RecentMessages, recentBytes},
	} {
		encoded, _ := json.Marshal(field.text)
		if len(encoded) > field.max || !utf8.ValidString(field.text) {
			t.Fatalf("%s: %d encoded bytes, UTF-8 valid %v", name, len(encoded), utf8.ValidString(field.text))
		}
	}
	if !strings.HasPrefix(got.Task, "task ") || !strings.HasPrefix(got.LatestRequest, "request ") ||
		!strings.HasSuffix(got.RecentMessages, " recent end") {
		t.Fatal("lost task/request prefixes or the recent end")
	}
	q := questions{state: raw, limits: compact.Jev113}
	answers := make([]answer, 1)
	batches, err := q.pack([]segment{{whole: []llm.Message{assistant("needed old detail")}}}, answers, "jev-1.13.0")
	if err != nil || len(batches) != 1 || answers[0].scores.Err != nil {
		t.Fatalf("escaped focus does not fit: batches %d, error %v, segment %v", len(batches), err, answers[0].scores.Err)
	}
}

func TestCutEncodedKeepsWholeCharacters(t *testing.T) {
	for _, text := range []string{"plain text", "<>&\"\\\n", "界é🙂"} {
		for maxBytes := 2; maxBytes < 32; maxBytes++ {
			for _, end := range []bool{false, true} {
				got := cutEncoded(text, maxBytes, end)
				encoded, _ := json.Marshal(got)
				if len(encoded) > maxBytes || !utf8.ValidString(got) ||
					!end && !strings.HasPrefix(text, got) || end && !strings.HasSuffix(text, got) {
					t.Fatalf("cut %q to %d bytes (end %v): %q", text, maxBytes, end, got)
				}
			}
		}
	}
}
