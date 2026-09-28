package dashi

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestEmptyContextOriginIsUser(t *testing.T) {
	user := ContextItem{Name: "n", Message: llm.UserMessage(llm.Text("hi"))}
	if err := validateContext(user); err != nil {
		t.Fatalf("empty origin rejected: %v", err)
	}
	system := user
	system.Message.Role = llm.RoleSystem
	if validateContext(system) == nil {
		t.Fatal("empty origin allowed system role")
	}
	system.Origin = ContextOperator
	if err := validateContext(system); err != nil {
		t.Fatalf("operator system context rejected: %v", err)
	}
	explicit := user
	explicit.Origin = ContextUser
	if contextKey(user) != contextKey(explicit) {
		t.Fatal("empty and user origins name different snapshot series")
	}
	if validateContext(ContextItem{
		Origin:  "library",
		Message: user.Message,
	}) == nil {
		t.Fatal("unknown origin accepted")
	}
}
