package mcp_test

import (
	"encoding/json"
	"fmt"

	dashimcp "github.com/deepnoodle-ai/dashi/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ExampleConvertResult turns a tool result that an application saved with
// json.Marshal back into a Dashi outcome, so the image reaches the model as
// an image instead of JSON text.
func ExampleConvertResult() {
	saved := []byte(`{"content":[` +
		`{"type":"text","text":"page loaded"},` +
		`{"type":"image","data":"iVBORw0KGgo=","mimeType":"image/png"}]}`)

	var res sdk.CallToolResult
	if err := json.Unmarshal(saved, &res); err != nil {
		panic(err)
	}
	outcome, warnings := dashimcp.ConvertResult(&res)

	fmt.Println(outcome.State, len(warnings))
	for _, b := range outcome.Output.Content {
		fmt.Println(b.Kind)
	}
	// Output:
	// succeeded 0
	// text
	// image
}
