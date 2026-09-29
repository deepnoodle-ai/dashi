package tool_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi/tool"
)

type lookupArgs struct {
	Service string `json:"service" description:"Service name"`
}

// ReadOnly and Idempotent declare a tool safe to repeat. A runner reruns such
// a call when a crash lost its result, and a read-only subagent may use a
// ReadOnly tool. Without either option, the annotation stays nil: unknown.
func ExampleReadOnly() {
	owner, err := tool.Func(
		"lookup_owner",
		"Returns the team that owns a service.",
		func(_ context.Context, _ tool.Call, in lookupArgs) (tool.Outcome, error) {
			return tool.Succeeded(in.Service + ": payments team"), nil
		},
		tool.ReadOnly(),
		tool.Idempotent(),
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	a := owner.Definition.Annotations
	fmt.Println("read-only:", *a.ReadOnly)
	fmt.Println("idempotent:", *a.Idempotent)
	fmt.Println("destructive known:", a.Destructive != nil)
	// Output:
	// read-only: true
	// idempotent: true
	// destructive known: false
}

// MustFunc builds a tool whose argument type is fixed at compile time, so
// startup code needs no error check. It panics if the tool cannot be built,
// as Func would report with an error.
func ExampleMustFunc() {
	owner := tool.MustFunc(
		"lookup_owner",
		"Returns the team that owns a service.",
		func(_ context.Context, _ tool.Call, in lookupArgs) (tool.Outcome, error) {
			return tool.Succeeded(in.Service + ": payments team"), nil
		},
		tool.ReadOnly(),
	)
	out, err := owner.Executor.Execute(context.Background(), tool.Call{
		ID:   "call_1",
		Name: "lookup_owner",
		Args: []byte(`{"service":"billing"}`),
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(owner.Definition.Name)
	fmt.Println(out.State)
	// Output:
	// lookup_owner
	// succeeded
}
