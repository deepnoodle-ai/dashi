package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
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
		{name: "null", args: `null`},
		{name: "wrong type", args: `{"query":3}`},
		{name: "outside enum", args: `{"query":"maybe"}`},
		{name: "below minimum", args: `{"query":"yes","count":0}`},
		{name: "unknown field", args: `{"query":"yes","extra":1}`},
		{name: "trailing value", args: `{"query":"yes"} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			fn := newSearchTool(t, &calls)
			out, err := fn.Executor.Execute(context.Background(), searchCall(test.args))
			if err != nil || out.State != StateNotExecuted || calls != 0 {
				t.Fatalf("%s: out=%+v err=%v calls=%d", test.args, out, err, calls)
			}
		})
	}
}

func TestFuncExecutesValidCallWithoutSharingArguments(t *testing.T) {
	calls := 0
	fn := newSearchTool(t, &calls)
	call := searchCall(`{"query":"yes"}`)
	out, err := fn.Executor.Execute(context.Background(), call)
	if err != nil || out.State != StateSucceeded || calls != 1 || !json.Valid(call.Args) {
		t.Fatalf("out=%+v err=%v calls=%d", out, err, calls)
	}
}

func TestFuncDoesNotExecuteCanceledCall(t *testing.T) {
	calls := 0
	fn := newSearchTool(t, &calls)
	ctx, cancel := context.WithCancel(context.Background())
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
			name:    "known failure",
			outcome: Outcome{State: StateFailed, Output: ptr(TextOutput("rejected"))},
			check: func(out Outcome, err error) bool {
				return err == nil && out.State == StateFailed
			},
		},
		{
			name:    "plain error",
			outcome: Outcome{State: StateSucceeded, Output: ptr(TextOutput("maybe"))},
			err:     failure,
			check: func(out Outcome, err error) bool {
				return err == failure && out.State == ""
			},
		},
		{
			name:    "fatal error",
			outcome: Outcome{State: StateSucceeded, Output: ptr(TextOutput("maybe"))},
			err:     Fatal(failure),
			check: func(out Outcome, err error) bool {
				return errors.Is(err, ErrFatal) && errors.Is(err, failure)
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
			fn, err := Func("act", "", func(context.Context, Call, struct{}) (Outcome, error) {
				return test.outcome, test.err
			})
			if err != nil {
				t.Fatalf("construct outcome test tool: %v", err)
			}
			call := Call{ID: "id", Name: "act", Args: json.RawMessage(`{}`)}
			out, err := fn.Executor.Execute(context.Background(), call)
			if !test.check(out, err) {
				t.Fatalf("executor changed the callback result: outcome=%+v error=%v", out, err)
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
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) || syntax != cause {
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
			name:    "succeeded with empty output",
			outcome: Outcome{State: StateSucceeded, Output: ptr(TextOutput(""))},
			valid:   true,
		},
		{
			name:    "waiting with reason",
			outcome: Outcome{State: StateWaiting, Wait: &Wait{Reason: "approval"}},
			valid:   true,
		},
		{
			name:    "detached with handle",
			outcome: Outcome{State: StateDetached, Handle: &Handle{ID: "job-1"}},
			valid:   true,
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
			name:    "detached without handle ID",
			outcome: Outcome{State: StateDetached, Handle: &Handle{}},
		},
		{
			name: "failed with wait details",
			outcome: Outcome{
				State:  StateFailed,
				Output: ptr(TextOutput("no")),
				Wait:   &Wait{Reason: "approval"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.outcome.Validate()
			if test.valid && err != nil {
				t.Fatalf("valid outcome was rejected: %v", err)
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
	fn, err := Func("search", "Search", func(ctx context.Context, call Call, in searchInput) (Outcome, error) {
		*calls++
		if call.ID != "call-1" || in.Query != "yes" {
			t.Fatalf("lost input or identity: %+v %+v", call, in)
		}
		call.Args[0] = '['
		output := TextOutput("found")
		return Outcome{State: StateSucceeded, Output: &output}, nil
	})
	if err != nil {
		t.Fatalf("construct typed search tool: %v", err)
	}
	return fn
}

func searchCall(args string) Call {
	return Call{ID: "call-1", Name: "search", Args: json.RawMessage(args)}
}

func ptr[T any](v T) *T {
	return &v
}
