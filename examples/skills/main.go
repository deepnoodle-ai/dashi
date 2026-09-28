// Command skills shows a Mobius-style host selecting assigned skills before
// giving Dashi an in-memory catalog. Set ANTHROPIC_API_KEY before running;
// ANTHROPIC_MODEL optionally overrides the catalog's default model.
//
// This example covers catalog and loading only. A real Mobius migration must
// keep its action-focus checkpoint and dispatch policy in the host. Dashi can
// authorize a later call in the same model batch before a skill load settles.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/skill"
	"github.com/deepnoodle-ai/dashi/tool"
)

// These fields resemble an app-owned assignment, not a Dashi skill schema.
type assignedSkill struct {
	Name            string
	Description     string
	Instructions    string
	RequiredActions []string
	AllowedActions  []string
}

type lookupInput struct {
	Incident string `json:"incident" description:"Incident ID, such as INC-42"`
}

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	lookup, err := tool.Func(
		"incident_lookup",
		"Look up a fictional incident.",
		func(
			_ context.Context,
			_ tool.Call,
			in lookupInput,
		) (tool.Outcome, error) {
			if in.Incident != "INC-42" {
				return tool.Failed("incident not found"), nil
			}
			return tool.Succeeded(
				"INC-42: API requests failed for 12 minutes; service recovered.",
			), nil
		},
	)
	if err != nil {
		return err
	}
	assigned := []assignedSkill{
		{
			Name:            "Incident Review",
			Description:     "Summarize the evidence for an incident.",
			Instructions:    "Look up the incident, then give a concise timeline and impact.",
			RequiredActions: []string{"incident_lookup"},
			AllowedActions:  []string{"incident_lookup"},
		},
		{
			Name:            "Billing Audit",
			Instructions:    "Inspect billing records.",
			RequiredActions: []string{"billing_lookup"},
		},
	}
	definitions := selectAvailable(
		assigned,
		map[string]bool{"incident_lookup": true},
	)
	bundle, err := skill.New(
		definitions,
		skill.Options{ToolName: "invoke_skill"},
	)
	if err != nil {
		return err
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	// The host filters assignments and versions its focus policy separately
	// from bundle.Digest. Increment focus-v1 when selectors or their meaning
	// change. A real host also persists/restores activated focus and enforces
	// it at dispatch; this example does not do those jobs.
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model:    os.Getenv("ANTHROPIC_MODEL"),
			Revision: "incident-agent-v1:" + bundle.Digest() + ":focus-v1",
			Instructions: "Use an available skill when it matches the task.\n\n" +
				bundle.Catalog(),
		},
		Client: client,
		Tools:  tool.Join(tool.List{lookup}, bundle),
	}
	outcome, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Use Incident Review to summarize INC-42.")),
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Answer)
	return nil
}

// selectAvailable is host code: tenancy and assignment checks would precede
// this step. A required action that is not present makes the skill unavailable.
func selectAvailable(
	assigned []assignedSkill,
	actions map[string]bool,
) []skill.Definition {
	var definitions []skill.Definition
	for _, item := range assigned {
		available := true
		for _, name := range item.RequiredActions {
			if !actions[name] {
				available = false
				break
			}
		}
		if !available {
			continue
		}
		definitions = append(definitions, skill.Definition{
			Name:         item.Name,
			Description:  item.Description,
			Instructions: item.Instructions,
		})
		// AllowedActions stays with the host's checkpoint and dispatch policy.
	}
	return definitions
}
