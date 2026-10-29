package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type searchInput struct {
	Query string `json:"query" enum:"yes,no"`
	Count int    `json:"count,omitempty" minimum:"1"`
}

func TestFuncRejectsInvalidArgumentsBeforeExecuting(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		{name: "missing required field", args: `{}`},
		{name: "misspelled required field", args: `{"qurey":"yes"}`},
		{name: "null", args: `null`},
		{name: "wrong type", args: `{"query":3}`},
		{name: "outside enum", args: `{"query":"maybe"}`},
		{name: "below minimum", args: `{"query":"yes","count":0}`},
		{name: "trailing value", args: `{"query":"yes"} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			fn := newSearchTool(t, &calls)
			out, err := fn.Executor.Execute(
				t.Context(),
				searchCall(test.args),
			)
			if err != nil ||
				out.State != StateNotExecuted ||
				calls != 0 {
				t.Fatalf(
					"%s: out=%+v err=%v calls=%d",
					test.args,
					out,
					err,
					calls,
				)
			}
		})
	}
}

func TestFuncExecutesValidCallWithoutSharingArguments(t *testing.T) {
	calls := 0
	fn := newSearchTool(t, &calls)
	call := searchCall(`{"query":"yes"}`)
	out, err := fn.Executor.Execute(t.Context(), call)
	if err != nil ||
		out.State != StateSucceeded ||
		calls != 1 ||
		!json.Valid(call.Args) {
		t.Fatalf("out=%+v err=%v calls=%d", out, err, calls)
	}
}

func TestFuncDoesNotExecuteCanceledCall(t *testing.T) {
	calls := 0
	fn := newSearchTool(t, &calls)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, err := fn.Executor.Execute(ctx, searchCall(`{"query":"yes"}`))
	if err != nil || out.State != StateNotExecuted || calls != 0 {
		t.Fatal("canceled call executed")
	}
}

func TestFuncReturnsCallbackResults(t *testing.T) {
	failure := errors.New("quota exceeded")
	tests := []struct {
		name    string
		outcome Outcome
		err     error
		check   func(Outcome, error) bool
	}{
		{
			name: "known failure",
			outcome: Outcome{
				State:  StateFailed,
				Output: new(TextOutput("rejected")),
			},
			check: func(out Outcome, err error) bool {
				return err == nil && out.State == StateFailed
			},
		},
		{
			name: "plain error",
			outcome: Outcome{
				State:  StateSucceeded,
				Output: new(TextOutput("maybe")),
			},
			err: failure,
			check: func(out Outcome, err error) bool {
				return err == failure && out.State == ""
			},
		},
		{
			name: "fatal error",
			outcome: Outcome{
				State:  StateSucceeded,
				Output: new(TextOutput("maybe")),
			},
			err: Fatal(failure),
			check: func(out Outcome, err error) bool {
				return errors.Is(
					err,
					ErrFatal,
				) && errors.Is(
					err,
					failure,
				)
			},
		},
		{
			name:    "invalid outcome",
			outcome: Outcome{State: StateWaiting},
			check: func(out Outcome, err error) bool {
				return errors.Is(err, ErrInvalidOutcome) &&
					errors.Is(err, ErrFatal) &&
					out.State == ""
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fn, err := Func(
				"act",
				"",
				func(context.Context, Call, struct{}) (Outcome, error) {
					return test.outcome, test.err
				},
			)
			if err != nil {
				t.Fatalf(
					"construct outcome test tool: %v",
					err,
				)
			}
			call := Call{
				ID:   "id",
				Name: "act",
				Args: json.RawMessage(`{}`),
			}
			out, err := fn.Executor.Execute(
				t.Context(),
				call,
			)
			if !test.check(out, err) {
				t.Fatalf(
					"executor changed the callback result: outcome=%+v error=%v",
					out,
					err,
				)
			}
		})
	}
}

func TestFatalKeepsCause(t *testing.T) {
	cause := &json.SyntaxError{Offset: 3}
	err := Fatal(cause)
	if !errors.Is(err, ErrFatal) {
		t.Fatal("fatal error does not match ErrFatal")
	}
	syntax, ok := errors.AsType[*json.SyntaxError](err)
	if !ok || syntax != cause {
		t.Fatal("fatal error lost its cause")
	}
	if err.Error() != cause.Error() {
		t.Fatalf("fatal error changed the message: %q", err.Error())
	}
	if errors.Is(cause, ErrFatal) {
		t.Fatal("wrapping marked the original error fatal")
	}
}

func TestFatalOfNilIsStillFatal(t *testing.T) {
	if err := Fatal(nil); !errors.Is(err, ErrFatal) {
		t.Fatalf("Fatal(nil) lost the fatal marker: %v", err)
	}
}

func TestOutcomeValidateMatchesPayloadsToState(t *testing.T) {
	tests := []struct {
		name    string
		outcome Outcome
		valid   bool
	}{
		{
			name:    "not executed without output",
			outcome: Outcome{State: StateNotExecuted},
			valid:   true,
		},
		{
			name: "succeeded with empty output",
			outcome: Outcome{
				State:  StateSucceeded,
				Output: new(TextOutput("")),
			},
			valid: true,
		},
		{
			name: "waiting with reason",
			outcome: Outcome{
				State: StateWaiting,
				Wait: &Wait{
					Reason: "approval",
				},
			},
			valid: true,
		},
		{
			name: "detached with handle",
			outcome: Outcome{
				State: StateDetached,
				Handle: &Handle{
					ID: "job-1",
				},
			},
			valid: true,
		},
		{
			name:    "succeeded without output",
			outcome: Outcome{State: StateSucceeded},
		},
		{
			name:    "waiting without reason",
			outcome: Outcome{State: StateWaiting, Wait: &Wait{}},
		},
		{
			name: "detached without handle ID",
			outcome: Outcome{
				State:  StateDetached,
				Handle: &Handle{},
			},
		},
		{
			name: "failed with wait details",
			outcome: Outcome{
				State:  StateFailed,
				Output: new(TextOutput("no")),
				Wait:   &Wait{Reason: "approval"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.outcome.Validate()
			if test.valid && err != nil {
				t.Fatalf(
					"valid outcome was rejected: %v",
					err,
				)
			}
			if !test.valid && err == nil {
				t.Fatalf("accepted %+v", test.outcome)
			}
		})
	}
}

func TestSchemaCompilationDoesNotLoadExternalReferences(t *testing.T) {
	if _, err := compileSchema([]byte(`{"$ref":"file:///missing.json"}`)); err == nil {
		t.Fatal("accepted external reference")
	}
}

// newSearchTool returns a typed tool that counts its invocations in calls and
// fails the test if it receives unexpected input or call identity.
func newSearchTool(t *testing.T, calls *int) Tool {
	t.Helper()
	fn, err := Func(
		"search",
		"Search",
		func(ctx context.Context, call Call, in searchInput) (Outcome, error) {
			*calls++
			if call.ID != "call-1" || in.Query != "yes" {
				t.Fatalf(
					"lost input or identity: %+v %+v",
					call,
					in,
				)
			}
			call.Args[0] = '['
			return Outcome{
				State:  StateSucceeded,
				Output: new(TextOutput("found")),
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct typed search tool: %v", err)
	}
	return fn
}

func searchCall(args string) Call {
	return Call{ID: "call-1", Name: "search", Args: json.RawMessage(args)}
}

func TestListReturnsCopy(t *testing.T) {
	list := List{{Definition: Definition{Name: "a"}}}
	got, err := list.Tools(t.Context())
	if err != nil || len(got) != 1 || got[0].Definition.Name != "a" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	got[0].Definition.Name = "changed"
	if list[0].Definition.Name != "a" {
		t.Fatalf("list shares its backing array: %+v", list)
	}
}

func TestOutcomeConstructors(t *testing.T) {
	valid := map[string]Outcome{
		"succeeded":       Succeeded("done"),
		"succeeded empty": Succeeded(""),
		"failed":          Failed("broken"),
		"waiting":         Waiting("needs review"),
		"detached":        Detached("job-1"),
	}
	for name, out := range valid {
		if err := out.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if s := Succeeded("done"); s.State != StateSucceeded ||
		s.Output.Content[0].Text.Text != "done" {
		t.Errorf("succeeded = %+v", s)
	}
	if f := Failed("broken"); f.State != StateFailed ||
		f.Output.Content[0].Text.Text != "broken" {
		t.Errorf("failed = %+v", f)
	}
	for name, out := range map[string]Outcome{
		"waiting without reason":  Waiting(""),
		"detached without handle": Detached(""),
	} {
		if err := out.Validate(); err == nil {
			t.Errorf("%s: want invalid", name)
		}
	}
}

type quotedInput struct {
	Inner struct {
		Count int `json:"count,string"`
	} `json:"inner"`
}

func buildFunc[T any]() error {
	_, err := Func(
		"f",
		"",
		func(context.Context, Call, T) (Outcome, error) {
			return Succeeded(""), nil
		},
	)
	return err
}

func TestFuncRequiresStructArguments(t *testing.T) {
	refused := map[string]error{
		"string": buildFunc[string](),
		"map":    buildFunc[map[string]any](),
		"any":    buildFunc[any](),
		"time":   buildFunc[*time.Time](),
	}
	for name, err := range refused {
		if err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if err := buildFunc[*searchInput](); err != nil {
		t.Fatalf("pointer to struct: %v", err)
	}
}

// A ",string" field is a JSON string in both the schema and decoding, so a
// call that quotes the number validates and decodes.
func TestFuncAcceptsQuotedField(t *testing.T) {
	var got int
	fn, err := Func(
		"quoted",
		"",
		func(_ context.Context, _ Call, in quotedInput) (Outcome, error) {
			got = in.Inner.Count
			return Succeeded(""), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fn.Definition.Schema), `"count":{"type":"string"}`) {
		t.Fatalf("schema = %s", fn.Definition.Schema)
	}
	out, err := fn.Executor.Execute(
		t.Context(),
		Call{ID: "call-1", Name: "quoted", Args: json.RawMessage(`{"inner":{"count":"5"}}`)},
	)
	if err != nil || out.State != StateSucceeded || got != 5 {
		t.Fatalf("out=%+v err=%v count=%d", out, err, got)
	}
}

func TestMustFuncPanicsOnBuildError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic for a string argument type")
		}
	}()
	MustFunc("bad", "Bad.",
		func(context.Context, Call, string) (Outcome, error) {
			return Succeeded("ok"), nil
		})
}

type (
	recursiveSlice []recursiveSlice
	recursiveMap   map[string]recursiveMap
	treeNode       struct {
		Name     string     `json:"name"`
		Children []treeNode `json:"children,omitempty"`
	}
	listThroughStruct []wrapsList
	wrapsList         struct {
		Next listThroughStruct `json:"next,omitempty"`
	}
)

// buildFuncWithin fails the test instead of hanging if construction does not
// return promptly.
func buildFuncWithin[T any](t *testing.T) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- buildFunc[T]() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("tool.Func did not return")
		return nil
	}
}

func TestFuncRecursiveArgumentTypes(t *testing.T) {
	refused := map[string]error{
		"recursive slice": buildFuncWithin[struct {
			Items recursiveSlice `json:"items"`
		}](t),
		"recursive map": buildFuncWithin[struct {
			Items recursiveMap `json:"items"`
		}](t),
	}
	for name, err := range refused {
		if err == nil || !strings.Contains(err.Error(), "recursive type") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	allowed := map[string]error{
		"struct":               buildFuncWithin[treeNode](t),
		"slice through struct": buildFuncWithin[wrapsList](t),
	}
	for name, err := range allowed {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Clone gives the copy its own Data bytes. Validate leaves Data to the
// runner, which drops what is not valid JSON.
func TestOutcomeData(t *testing.T) {
	o := Succeeded("ok")
	o.Data = json.RawMessage(`{"turn":"a"}`)
	c := o.Clone()
	c.Data[2] = 'x'
	if string(o.Data) != `{"turn":"a"}` {
		t.Fatalf("Clone shares Data: %s", o.Data)
	}
	o.Data = json.RawMessage(`{"turn":`)
	if err := o.Validate(); err != nil {
		t.Fatalf("invalid Data failed the outcome: %v", err)
	}
}
