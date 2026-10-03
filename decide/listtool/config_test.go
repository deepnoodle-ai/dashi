package listtool

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/decide"
)

func TestConfigDefaults(t *testing.T) {
	client, err := decide.NewClient(decide.WithAPIKey("key-for-tests"), decide.WithoutEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	notes := List[string]{
		Name:  "notes",
		Items: func(context.Context, string) ([]string, error) { return nil, nil },
		ID:    func(s string) string { return s },
		Text:  func(s string) string { return s },
	}
	for _, tc := range []struct {
		cfg  Config
		want settings
	}{
		{Config{}, settings{maxQuestions: 8, maxRequests: 500, maxTurnRequests: 2000,
			maxItemBytes: 64 << 10, concurrency: 8}},
		{Config{MaxQuestions: 100, MaxRequests: -1, MaxTurnRequests: -1, MaxItemBytes: -1, Concurrency: -3},
			settings{maxQuestions: 64, maxRequests: math.MaxInt, maxTurnRequests: math.MaxInt, concurrency: 8}},
		{Config{MaxQuestions: 3, MaxRequests: 7, MaxTurnRequests: 9, MaxItemBytes: 100, Concurrency: 2},
			settings{maxQuestions: 3, maxRequests: 7, maxTurnRequests: 9, maxItemBytes: 100, concurrency: 2}},
	} {
		tc.cfg.Client, tc.cfg.Lists = client, []AnyList{notes}
		s, err := tc.cfg.resolve("judge_items")
		if err != nil {
			t.Fatal(err)
		}
		got := settings{maxQuestions: s.maxQuestions, maxRequests: s.maxRequests,
			maxTurnRequests: s.maxTurnRequests, maxItemBytes: s.maxItemBytes, concurrency: s.concurrency}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v resolved to %+v, want %+v", tc.cfg, got, tc.want)
		}
	}
}

func TestConstructorsRefuseBadConfig(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	client, err := decide.NewClient(decide.WithAPIKey("key-for-tests"), decide.WithoutEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	list := func(name string) List[string] {
		return List[string]{
			Name:  name,
			Items: func(context.Context, string) ([]string, error) { return nil, nil },
			ID:    func(s string) string { return s },
			Text:  func(s string) string { return s },
		}
	}
	noText := list("notes")
	noText.Text = nil
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no client and no key", Config{Lists: []AnyList{list("notes")}}, "no API key"},
		{"no lists", Config{Client: client}, "Config.Lists is empty"},
		{"no name", Config{Client: client, Lists: []AnyList{list("")}}, "a list has no Name"},
		{"bad name", Config{Client: client, Lists: []AnyList{list("my notes")}}, `list name "my notes" must be`},
		{"repeated name", Config{Client: client, Lists: []AnyList{list("notes"), list("notes")}},
			`list "notes" is listed twice`},
		{"no Text", Config{Client: client, Lists: []AnyList{noText}}, `list "notes" has no Text function`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, build := range []func(Config) error{
				func(c Config) error { _, err := JudgeItems(c); return err },
			} {
				err := build(tc.cfg)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error %v, want %q", err, tc.want)
				}
				if tc.cfg.Client == nil && !errors.Is(err, decide.ErrNoAPIKey) {
					t.Errorf("error %v, want decide.ErrNoAPIKey", err)
				}
			}
		})
	}
}
