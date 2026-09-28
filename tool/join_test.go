package tool

import (
	"context"
	"errors"
	"testing"
)

type failingSet struct{}

func (failingSet) Tools(context.Context) ([]Tool, error) {
	return nil, errors.New("registry down")
}

func TestJoinCombinesToolsets(t *testing.T) {
	a := List{{Definition: Definition{Name: "a"}}}
	b := List{{Definition: Definition{Name: "b"}}}
	tools, err := Join(a, nil, b).Tools(context.Background())
	if err != nil || len(tools) != 2 || tools[0].Definition.Name != "a" ||
		tools[1].Definition.Name != "b" {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	if _, err := Join(a, a).Tools(context.Background()); err == nil {
		t.Fatal("a duplicate name was accepted")
	}
	if _, err := Join(a, failingSet{}).Tools(context.Background()); err == nil {
		t.Fatal("a failing set's error was dropped")
	}
}
