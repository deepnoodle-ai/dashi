package client

import (
	"testing"

	"github.com/deepnoodle-ai/decide"
)

func TestDefault(t *testing.T) {
	given, err := decide.NewClient(decide.WithAPIKey("given"), decide.WithoutEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Default(given); got != given || err != nil {
		t.Fatalf("Default(given) = %v, %v; want given", got, err)
	}
	t.Setenv("TYPESAFE_API_KEY", "from-environment")
	first, err := Default(nil)
	if err != nil || first == nil {
		t.Fatalf("Default(nil) = %v, %v", first, err)
	}
	if again, _ := Default(nil); again != first {
		t.Fatal("Default(nil) made a second client")
	}
}
