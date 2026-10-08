package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sync"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/patterns/fanout"
	"github.com/deepnoodle-ai/decide/patterns/rank"
)

// maxQuestions is how many tools one Decide request asks about: Clef's
// limit, which Jev also accepts. Larger catalogs go out as several requests.
const maxQuestions = 64

// Picker narrows the tools offered to the model. On the first model call
// of a turn it asks a decision model, once per tool, whether the tool could
// help with the user's request, and hides the tools that fall short. Every
// later call of the turn hides the same tools, because changing the offered
// tools resets the cached prompt.
type Picker struct {
	Client *decide.Client
	// Threshold is the P(yes) a tool needs to stay offered.
	Threshold float64
	// Keep is the fewest tools to offer, in rank order, whatever their P(yes).
	Keep int
	// Always lists tools that are always offered and never asked about.
	Always []string
	// Report, when set, is called with each turn's pick.
	Report func(Pick)

	mu sync.Mutex
	// picks holds each turn's pick by turn ID, so every model call of the
	// turn hides the same tools. A long-running host would drop a turn's
	// entry when the turn ends.
	picks map[string]Pick
}

// Pick is one turn's decision.
type Pick struct {
	Model  string   // the decision model's resolved version
	Kept   []Judged // in rank order, then the Always tools
	Hidden []string
	// HiddenBytes approximates the bytes of tool definitions left out of
	// each model call: the JSON of each hidden definition.
	HiddenBytes int
	// Err is why no tools were hidden, such as a failed Decide request.
	Err error
}

// Judged is one offered tool and its P(yes); Always tools have no P.
type Judged struct {
	Name   string
	P      float64
	Always bool
}

// Hook returns the BeforeModel hook.
func (p *Picker) Hook() dashi.ModelHook {
	return dashi.ModelHook{Name: "toolpick", Func: p.beforeModel}
}

func (p *Picker) beforeModel(
	ctx context.Context, plan dashi.ModelPlan) (dashi.ModelDecision, error) {
	if plan.Purpose == dashi.ModelPurposeCompaction {
		return dashi.ModelDecision{}, nil
	}
	p.mu.Lock()
	pick, ok := p.picks[plan.Turn.ID]
	p.mu.Unlock()
	if !ok {
		pick = p.pick(ctx, plan.Input)
		// Hiding is an optimization, not a safety control: when Decide
		// fails, offer every tool and say so, rather than stop the turn or
		// guess. The failure is kept too, so the offer stays stable for the
		// rest of the turn instead of changing when a retry succeeds.
		if pick.Err != nil {
			log.Printf("toolpick: offering every tool: %v", pick.Err)
		}
		p.mu.Lock()
		if p.picks == nil {
			p.picks = map[string]Pick{}
		}
		p.picks[plan.Turn.ID] = pick
		p.mu.Unlock()
		if p.Report != nil {
			p.Report(pick)
		}
	}
	return dashi.ModelDecision{Hide: pick.Hidden}, nil
}

// pick asks Decide about every tool in the input except the Always ones.
func (p *Picker) pick(ctx context.Context, in llm.Input) Pick {
	request := userRequest(in.Messages)
	var always []Judged
	var asked []tool.Definition
	for _, def := range in.Tools {
		if slices.Contains(p.Always, def.Name) || def.Name == dashi.SubmitOutputTool {
			always = append(always, Judged{Name: def.Name, Always: true})
		} else {
			asked = append(asked, def)
		}
	}
	if request == "" || len(asked) == 0 {
		return Pick{Err: fmt.Errorf("no user request or no tools to judge")}
	}

	// One request per chunk of tools, sent in parallel. The request is the
	// state; each question names one tool, keyed by its name (tool names
	// already fit Decide's key rules).
	chunks := slices.Collect(slices.Chunk(asked, maxQuestions))
	results, err := fanout.Map(ctx, p.Client, chunks, 4, func(
		_ context.Context, _ int, defs []tool.Definition) (*decide.Request, error) {
		req := decide.NewRequest(request)
		for _, def := range defs {
			req.Questions[def.Name] = decide.Noul(
				fmt.Sprintf("Could the tool %q help with any part of this request? "+
					"The tool's description: %s", def.Name, def.Description),
				decide.NoulTrue("The model would likely call this tool for the request."),
				decide.NoulFalse("The tool is unrelated to the request."),
			)
		}
		return req, nil
	})
	if err != nil {
		return Pick{Err: err}
	}
	observations := make([]rank.Observation, 0, len(asked))
	for _, r := range results {
		if r.Err != nil {
			return Pick{Err: r.Err}
		}
		for _, def := range chunks[r.Index] {
			o, err := rank.FromResponse(r.Response, def.Name)
			if err != nil {
				return Pick{Err: err}
			}
			observations = append(observations, o)
		}
	}
	order, err := rank.Rerank(asked, observations,
		func(_ int, def tool.Definition) string { return def.Name })
	if err != nil {
		return Pick{Err: err}
	}

	pick := Pick{Model: order.Observations[0].Model}
	for i, e := range order.Entries {
		if i < p.Keep || e.Value >= p.Threshold {
			pick.Kept = append(pick.Kept, Judged{Name: e.Item.Name, P: e.Value})
			continue
		}
		pick.Hidden = append(pick.Hidden, e.Item.Name)
		if b, err := json.Marshal(e.Item); err == nil {
			pick.HiddenBytes += len(b)
		}
	}
	pick.Kept = append(pick.Kept, always...)
	return pick
}

// userRequest returns the text of the latest user message, skipping
// context items such as reminders, which carry Context.
func userRequest(messages []llm.Message) string {
	for _, m := range slices.Backward(messages) {
		if m.Role == llm.RoleUser && m.Context == nil && m.Text() != "" {
			return m.Text()
		}
	}
	return ""
}
