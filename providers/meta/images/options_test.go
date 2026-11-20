package images

import "testing"

func TestOptionsValidate(t *testing.T) {
	high := 2.5
	for name, options := range map[string]Options{
		"strength":    {ReasoningStrength: "medium"},
		"temperature": {Temperature: &high},
	} {
		if _, err := options.fields(); err == nil {
			t.Errorf("%s: invalid option accepted", name)
		}
	}
}

func TestOptionsCloneDetaches(t *testing.T) {
	temperature, on := 1.0, true
	options := Options{Temperature: &temperature, ToolEnablement: &ToolEnablement{Shell: &on}}
	clone := options.clone()
	temperature, on = 2, false
	if *clone.Temperature != 1 || !*clone.ToolEnablement.Shell {
		t.Fatalf("clone shares state: %+v", clone)
	}
}
