package tool

import (
	"context"
	"encoding/json"
	"testing"
)

type bytesInput struct {
	Data []byte `json:"data"`
}

// ValidateArguments rejects what Execute cannot decode into T, so a hook or
// an approval never sees a call that is sure to fail.
func TestValidateArgumentsDecodesIntoType(t *testing.T) {
	fn, err := Func(
		"bytes",
		"",
		func(context.Context, Call, bytesInput) (Outcome, error) {
			return Succeeded(""), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	validator := fn.Executor.(interface{ ValidateArguments(json.RawMessage) error })
	if err := validator.ValidateArguments(json.RawMessage(`{"data":"aGk="}`)); err != nil {
		t.Fatalf("valid base64: %v", err)
	}
	if err := validator.ValidateArguments(json.RawMessage(`{"data":"!!"}`)); err == nil {
		t.Fatal("invalid base64 passed validation")
	}
}
