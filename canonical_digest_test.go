package kernel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

// jsonbRecorder stores steps like a Postgres jsonb column: Load returns each
// step with the same JSON meaning but different bytes. Keys come back in
// jsonb's order (shorter first), whitespace changes, escapes are decoded,
// duplicate keys keep the last value, and exponent numbers are expanded
// (1e2 becomes 100) while decimal scale is kept (1.50 stays 1.50).
type jsonbRecorder struct{ *MemoryRecorder }

var _ Loader = jsonbRecorder{}

func (r jsonbRecorder) Load(ctx context.Context, turnID string) (Turn, error) {
	t, err := r.MemoryRecorder.Load(ctx, turnID)
	if err != nil {
		return Turn{}, err
	}
	for i, step := range t.Steps {
		t.Steps[i] = jsonbRoundTrip(step)
	}
	return t, nil
}

// jsonbRoundTrip passes v through the simulated jsonb store.
func jsonbRoundTrip[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(jsonbNormalize(raw), &out); err != nil {
		panic(err)
	}
	return out
}

func jsonbNormalize(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	writeJSONB(&buf, value)
	return buf.Bytes()
}

func writeJSONB(buf *bytes.Buffer, value any) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		// jsonb orders object keys by length, then bytes.
		slices.SortFunc(keys, func(a, b string) int {
			if len(a) != len(b) {
				return len(a) - len(b)
			}
			return strings.Compare(a, b)
		})
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteString(", ")
			}
			writeJSONBString(buf, k)
			buf.WriteString(": ")
			writeJSONB(buf, v[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteString(", ")
			}
			writeJSONB(buf, item)
		}
		buf.WriteByte(']')
	case string:
		writeJSONBString(buf, v)
	case json.Number:
		buf.WriteString(jsonbNumber(string(v)))
	default:
		b, _ := json.Marshal(v)
		buf.Write(b)
	}
}

func writeJSONBString(buf *bytes.Buffer, s string) {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	buf.Truncate(buf.Len() - 1) // Encode appends a newline.
}

// jsonbNumber spells a number as Postgres numeric does: no exponent, and the
// scale of the input (digits after the point, less the exponent) kept.
func jsonbNumber(lit string) string {
	neg := strings.HasPrefix(lit, "-")
	lit = strings.TrimPrefix(lit, "-")
	mantissa, expPart, _ := strings.Cut(strings.ToLower(lit), "e")
	exp, _ := strconv.Atoi(expPart)
	intPart, frac, _ := strings.Cut(mantissa, ".")
	digits := intPart + frac
	point := len(intPart) + exp
	var whole, fraction string
	switch {
	case point <= 0:
		whole, fraction = "0", strings.Repeat("0", -point)+digits
	case point >= len(digits):
		whole = digits + strings.Repeat("0", point-len(digits))
	default:
		whole, fraction = digits[:point], digits[point:]
	}
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	out := whole
	if fraction != "" {
		out += "." + fraction
	}
	if neg && strings.Trim(out, "0.") != "" {
		out = "-" + out
	}
	return out
}

func TestJSONBSimulation(t *testing.T) {
	for in, want := range map[string]string{
		"1e2":     "100",
		"1.50":    "1.50",
		"1.5e1":   "15",
		"1.50e1":  "15.0",
		"1e-07":   "0.0000001",
		"-0":      "0",
		"0.5":     "0.5",
		"-2.5E+1": "-25",
	} {
		if got := jsonbNumber(in); got != want {
			t.Errorf("jsonbNumber(%s) = %s, want %s", in, got, want)
		}
	}
	got := string(jsonbNormalize([]byte(`{"bb":1e2,"a":"é<","bb":{"z":1,"y":[1.50]}}`)))
	if want := `{"a": "é<", "bb": {"y": [1.50], "z": 1}}`; got != want {
		t.Fatalf("normalize = %s, want %s", got, want)
	}
}

// byteHex hashes v's plain json.Marshal output, which a jsonb round trip
// changes even when the JSON meaning is the same.
func byteHex(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Messy but valid JSON, as a model, a schema author, or another encoder may
// write it. Each value changes bytes when it passes through jsonb.
var (
	messySchema = json.RawMessage(`{ "type" : "object",
		"properties": {"b": {"type": "number", "maximum": 1e3}, "n": {"type": "number", "multipleOf": 0.50}, "a": {"type": "string", "description": "café <id>"}},
		"additionalProperties": false, "type": "object" }`)
	messyArgs = json.RawMessage(`{"b": 1e2, "a": "café <x>", "n": 1.50E0}`)
)

func messyTool(annotations tool.Annotations, exec execFunc) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:        "work",
			Schema:      messySchema,
			Kind:        tool.KindLocal,
			Annotations: annotations,
		},
		Executor: exec,
	}
}

func messyCallResult() llm.Result {
	call := tool.Call{ID: "call-1", Name: "work", Args: messyArgs}
	return llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		}),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
}

func messyHistory() []llm.Message {
	call := tool.Call{ID: "earlier-call", Name: "work", Args: messyArgs}
	return []llm.Message{
		llm.UserMessage(llm.Text("earlier <question>")),
		llm.AssistantMessage(
			llm.Content{Kind: llm.ContentToolCall, ToolCall: &call},
			llm.Content{
				Kind: llm.ContentOpaque,
				Opaque: &llm.Opaque{
					Version: 1,
					Origin: content.Origin{
						Provider: "test",
						API:      "test.chat",
						Model:    "fixture",
						Scope:    "fixture",
					},
					Type: "signature",
					Unit: llm.OpaqueUnitItem,
					Raw:  json.RawMessage(`{"sig": "abc", "n": 2.0e0, "sig": "last"}`),
				},
			},
		),
	}
}

// reload returns the turn as a host using jsonb storage would read it, and
// checks that the store really changed its bytes.
func reload(t *testing.T, rec jsonbRecorder, id string) Turn {
	t.Helper()
	stored, err := rec.MemoryRecorder.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := rec.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(stored)
	b, _ := json.Marshal(loaded)
	if bytes.Equal(a, b) {
		t.Fatal("jsonb simulation did not change the stored bytes")
	}
	return loaded
}

func TestJSONBRoundTripApprovalAndToolBinding(t *testing.T) {
	rec := jsonbRecorder{NewMemoryRecorder()}
	client := &scriptedClient{
		results: []llm.Result{messyCallResult(), final("done")},
	}
	runs := 0
	binding := messyTool(
		tool.Annotations{},
		func(_ context.Context, call tool.Call) (tool.Outcome, error) {
			runs++
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("ok")),
			}, nil
		},
	)
	runner := Runner{
		Agent:    testAgent(),
		Client:   client,
		Tools:    fixedTools{binding},
		Recorder: rec,
		Policy:   deferredPolicy{},
	}
	history := messyHistory()

	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			History:   history,
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil || out.Stop.Kind != StopWaiting {
		t.Fatalf("start=%+v err=%v", out.Stop, err)
	}
	turn := reload(t, rec, "t1")
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	// The bug: byte digests of the reloaded definition differ from the live one.
	if byteHex(view.Tools["call-1"].Definition) == byteHex(binding.Definition) {
		t.Fatal("simulation left the tool definition byte-identical")
	}

	// A retried Start on the reloaded turn is the same command.
	again, err := runner.Run(
		t.Context(),
		turn,
		Start{
			CommandID: "start",
			History:   history,
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil || again.Stop.Kind != StopWaiting {
		t.Fatalf("retried start=%+v err=%v", again.Stop, err)
	}

	// The application may digest the arguments it saw live or reloaded.
	for _, args := range []json.RawMessage{
		messyArgs,
		view.Tools["call-1"].Call.Args,
	} {
		if digest(args) != ArgsDigest(view.Tools["call-1"].Call.Args) {
			t.Fatalf("approval digest of %s does not match", args)
		}
	}
	approval := ApprovalFact{
		CallID:         "call-1",
		Approve:        true,
		ArgsDigest:     digest(messyArgs),
		PolicyRevision: "policy-v1",
	}
	settle := Settle{CommandID: "approve", CallID: "call-1", Approval: &approval}
	out, err = runner.Run(t.Context(), turn, settle)
	if err != nil || !out.ReadyToContinue {
		t.Fatalf("approve=%+v err=%v", out, err)
	}
	turn = reload(t, rec, "t1")
	if _, err := runner.Run(
		t.Context(),
		turn,
		settle,
	); err != nil {
		t.Fatalf("retried approval: %v", err)
	}

	out, err = runner.Run(
		t.Context(),
		turn,
		Continue{CommandID: "continue", History: jsonbRoundTrip(history)},
	)
	if err != nil || out.Stop.Kind != StopCompleted || runs != 1 {
		t.Fatalf("continue=%+v err=%v runs=%d", out.Stop, err, runs)
	}
	turn = reload(t, rec, "t1")
	if _, err := Replay(turn); err != nil {
		t.Fatalf("replay of reloaded turn: %v", err)
	}
	for _, step := range turn.Steps {
		if len(step.Warnings) != 0 {
			t.Fatalf("unexpected warnings: %+v", step.Warnings)
		}
	}
	// Re-committing a reloaded step is an idempotent retry.
	last := turn.Steps[len(turn.Steps)-1]
	if err := rec.Commit(
		t.Context(),
		CommitRequest{Step: last, ExpectedSeq: last.Seq - 1},
	); err != nil {
		t.Fatalf("recommit of reloaded step: %v", err)
	}
}

func TestJSONBRoundTripInterruptedToolAndHistory(t *testing.T) {
	rec := jsonbRecorder{NewMemoryRecorder()}
	client := &scriptedClient{
		results: []llm.Result{messyCallResult(), final("done")},
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	runs := 0
	binding := messyTool(
		tool.Annotations{Idempotent: new(true)},
		func(context.Context, tool.Call) (tool.Outcome, error) {
			runs++
			if runs == 1 {
				cancel(errors.New("deploying"))
			}
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("ok")),
			}, nil
		},
	)
	runner := Runner{
		Agent:    testAgent(),
		Client:   client,
		Tools:    fixedTools{binding},
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
	}
	history := messyHistory()

	out, err := runner.Run(
		ctx,
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			History:   history,
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil || out.Stop.Kind != StopInterrupted {
		t.Fatalf("start=%+v err=%v", out.Stop, err)
	}
	stored := jsonbRoundTrip(history)
	startBytes, _ := json.Marshal(fingerprintMessages(history))
	storedBytes, _ := json.Marshal(fingerprintMessages(stored))
	if bytes.Equal(startBytes, storedBytes) {
		t.Fatal("simulation left the history byte-identical")
	}

	turn := reload(t, rec, "t1")
	out, err = runner.Run(
		t.Context(),
		turn,
		Continue{CommandID: "resume", History: stored},
	)
	if err != nil || out.Stop.Kind != StopCompleted || runs != 2 {
		t.Fatalf("resume=%+v err=%v runs=%d", out.Stop, err, runs)
	}
	for _, step := range out.Turn.Steps {
		if len(step.Warnings) != 0 {
			t.Fatalf("jsonb history produced warnings: %+v", step.Warnings)
		}
	}
	if len(client.inputs) != 2 || len(client.inputs[1].Messages) < 2 {
		t.Fatalf("model inputs=%d", len(client.inputs))
	}
}

func mustLoad(t *testing.T, rec jsonbRecorder) Turn {
	t.Helper()
	turn, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

// Strict history checking must not reject history that only went through a
// jsonb round trip: the fingerprint compares JSON meaning.
func TestStrictHistoryAcceptsJSONBRoundTrip(t *testing.T) {
	rec := jsonbRecorder{NewMemoryRecorder()}
	// The first model call is cut off by a deploy, so the turn waits to be
	// continued; the second completes.
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	client := &interruptionClient{
		scriptedClient: &scriptedClient{},
		onSend: func(ctx context.Context, _ llm.Sink, index int) (llm.Result, error) {
			if index == 0 {
				cancel(errors.New("deploying"))
				return llm.Result{}, ctx.Err()
			}
			return final("done"), nil
		},
	}
	history := messyHistory()
	runner := Runner{
		Agent:    testAgent(),
		Client:   client,
		Recorder: rec,
	}
	out, err := runner.Run(
		ctx,
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			History:   history,
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil || out.Stop.Kind != StopInterrupted {
		t.Fatalf("start=%+v err=%v", out.Stop, err)
	}
	stored := jsonbRoundTrip(history)
	a, _ := json.Marshal(fingerprintMessages(history))
	b, _ := json.Marshal(fingerprintMessages(stored))
	if bytes.Equal(a, b) {
		t.Fatal("simulation left the history byte-identical")
	}
	out, err = runner.Run(
		t.Context(),
		mustLoad(t, rec),
		Continue{
			CommandID:         "continue",
			History:           stored,
			OnHistoryMismatch: HistoryMismatchReject,
		},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf(
			"strict continue with jsonb history: stop=%+v err=%v",
			out.Stop,
			err,
		)
	}
	for _, step := range out.Turn.Steps {
		if len(step.Warnings) != 0 {
			t.Fatalf("jsonb history produced warnings: %+v", step.Warnings)
		}
	}
	// A genuinely different history is still rejected.
	other := append(stored, llm.UserMessage(llm.Text("extra")))
	_, err = runner.Run(
		t.Context(),
		mustLoad(t, rec),
		Continue{
			CommandID:         "continue-2",
			History:           other,
			OnHistoryMismatch: HistoryMismatchReject,
		},
	)
	if !errors.Is(err, ErrHistoryMismatch) {
		t.Fatalf("different history err=%v", err)
	}
}
