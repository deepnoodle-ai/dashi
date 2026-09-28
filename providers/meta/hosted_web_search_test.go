package meta

import (
	"encoding/json"
	"testing"
)

func TestWebSearchToolOmitsUnsetUserLocation(t *testing.T) {
	definition, err := NewWebSearchTool(WebSearchToolOptions{
		SearchContextSize: SearchContextLow,
	})
	if err != nil {
		t.Fatal(err)
	}
	var options map[string]json.RawMessage
	if err := json.Unmarshal(definition.Hosted.Options, &options); err != nil {
		t.Fatal(err)
	}
	if location, ok := options["user_location"]; ok {
		t.Fatalf("unset user location was sent as %s", location)
	}
}
