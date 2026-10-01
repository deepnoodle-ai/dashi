package main

import (
	"context"
	"github.com/deepnoodle-ai/dashi/llm"
	"reflect"
	"testing"
)

type sentControlClient struct {
	llm.Client
	sent *[]llm.Settings
}

func (c *sentControlClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	*c.sent = append(*c.sent, call.Request.Snapshot().Input.Settings)
	return c.Client.Send(ctx, call)
}

func TestClearedControlsStayClearedInRecordedAndDispatchedNextTurn(t *testing.T) {
	for _, tc := range []struct {
		command, set, clear string
		check               func(llm.Settings) bool
	}{
		{"/temperature", "0.7", "omit", func(s llm.Settings) bool { return s.Temperature == nil }},
		{"/effort", "high", "omit", func(s llm.Settings) bool { return s.Reasoning == nil }},
		{"/max-tokens", "1234", "default", func(s llm.Settings) bool { return s.MaxTokens == 0 }},
	} {
		t.Run(tc.command, func(t *testing.T) {
			e, p := routedTestEngine(t, func(string) (int, string) { return 200, "answer" })
			var sent []llm.Settings
			factory := p.routes.factory
			p.routes.factory = func(d configuredDestination, lookup func(string) (string, bool), trusted bool) (llm.Client, error) {
				client, err := factory(d, lookup, trusted)
				if err != nil {
					return nil, err
				}
				return &sentControlClient{Client: client, sent: &sent}, nil
			}
			ctrl := NewController(t.Context(), e, newRecorderUI(), e.cfg.Store, e.cfg.Dir)
			ctrl.generationControl(t.Context(), tc.command, tc.set)
			if _, err := e.Prompt(t.Context(), "use explicit control"); err != nil {
				t.Fatal(err)
			}
			if len(sent) != 1 || tc.check(sent[0]) {
				t.Fatalf("explicit setting missing: %+v", sent)
			}
			ctrl.generationControl(t.Context(), tc.command, tc.clear)
			if _, err := e.Prompt(t.Context(), "use cleared control"); err != nil {
				t.Fatal(err)
			}
			last := e.Last(t.Context())
			if len(sent) != 2 || !tc.check(sent[1]) || last == nil || !tc.check(last.View.Agent.Settings) || !last.View.Agent.NoInheritedSettings {
				t.Fatalf("sent=%+v last=%+v", sent, last)
			}
			if !reflect.DeepEqual(last.View.Agent.Settings, sent[1]) {
				t.Fatalf("record and sent settings disagree: recorded=%+v sent=%+v", last.View.Agent.Settings, sent[1])
			}
		})
	}
}
