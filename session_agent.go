package dashi

import (
	"context"
	"reflect"
	"strings"
	"time"
)

// agentFor returns the agent for a new turn, merged field by field:
// Start.Agent, then Session.Agent, then the agent recorded by the last
// user-origin turn up to turn last. The engine fills what is still zero.
// The caller holds mu.
func (s *Session) agentFor(ctx context.Context, start Agent, last int) Agent {
	agent := mergeAgent(start, s.Agent)
	for n := last; n >= 1; n-- {
		t, err := s.load(ctx, n)
		if err != nil || t.view.Origin != StartUser {
			continue
		}
		return mergeAgent(agent, s.carried(t.view.Agent))
	}
	return agent
}

// carried is the part of a recorded agent that a later turn may inherit.
// A deadline is an absolute time that would stop the next turn before its
// first call. Provider, API, and model carry only while they match the
// runner's client, so a host that changes clients does not fail every
// turn. A derived revision is dropped and derived again, since the merged
// agent may differ.
func (s *Session) carried(a Agent) Agent {
	a.Limits.Deadline = time.Time{}
	if c := s.Runner.Client; c == nil ||
		a.Provider != c.Provider() || a.API != c.API() {
		a.Provider, a.API, a.Model = "", "", ""
	}
	if strings.HasPrefix(a.Revision, revisionPrefix) {
		a.Revision = ""
	}
	return a
}

// mergeAgent fills each zero field of a from b, recursing into Settings and
// Limits: a nil pointer, empty string, slice, or map, zero number, or false
// falls through, so a setting is turned off with its negative or No… form.
// Tools and NoTools are one selection and move together: a layer that sets
// either replaces both.
//
// It walks the fields by reflection so that a field added to Agent, Limits,
// or llm.Settings carries forward without a change here.
func mergeAgent(a, b Agent) Agent {
	if len(a.Tools) > 0 || a.NoTools {
		b.Tools, b.NoTools = a.Tools, a.NoTools
	}
	fillZero(reflect.ValueOf(&a).Elem(), reflect.ValueOf(b))
	return a
}

var timeType = reflect.TypeFor[time.Time]()

func fillZero(dst, src reflect.Value) {
	for i := range dst.NumField() {
		f := dst.Field(i)
		if !f.CanSet() {
			continue
		}
		switch {
		case f.Kind() == reflect.Struct && f.Type() != timeType:
			fillZero(f, src.Field(i))
		case f.IsZero(),
			(f.Kind() == reflect.Slice || f.Kind() == reflect.Map) && f.Len() == 0:
			f.Set(src.Field(i))
		}
	}
}
