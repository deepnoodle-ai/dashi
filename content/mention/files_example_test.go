package mention_test

import (
	"fmt"
	"github.com/deepnoodle-ai/dashi/content/mention"
)

func ExampleScan() {
	text := `Explain @"notes with spaces.txt":12 and @main.go.`
	for _, match := range mention.Scan(text) {
		fmt.Println(match.Path, text[match.Start:match.End])
	}
	// Output:
	// notes with spaces.txt:12 @"notes with spaces.txt":12
	// main.go @main.go
}
