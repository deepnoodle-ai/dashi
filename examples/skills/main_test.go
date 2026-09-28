package main

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/skill"
)

func TestAssignedSkillsStayHostSelected(t *testing.T) {
	assigned := []assignedSkill{
		{
			Name:            "Incident Review",
			Instructions:    "Inspect the incident.",
			RequiredActions: []string{"incident_lookup"},
			AllowedActions:  []string{"incident_lookup"},
		},
		{
			Name:            "Billing Audit",
			Instructions:    "Inspect billing.",
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
		t.Fatal(err)
	}
	if len(definitions) != 1 ||
		!strings.Contains(bundle.Catalog(), "Incident Review") ||
		strings.Contains(bundle.Catalog(), "Billing Audit") ||
		strings.Contains(bundle.Catalog(), "Inspect the incident.") {
		t.Fatalf("selected definitions = %+v; catalog = %q", definitions, bundle.Catalog())
	}
	if len(assigned[0].AllowedActions) != 1 {
		t.Fatal("host focus policy was changed by catalog construction")
	}
}
