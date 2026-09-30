// Package compactiontext supplies the recorded-summary interpretation shared
// by history projection and presentation.
package compactiontext

import "strings"

// Summary returns the last tagged summary, or the trimmed answer when tags
// are absent. Empty tags mean no replacement, even with a completed stop.
func Summary(answer string) string {
	answer = strings.TrimSpace(answer)
	end := strings.LastIndex(answer, "</summary>")
	if end < 0 {
		return answer
	}
	start := strings.LastIndex(answer[:end], "<summary>")
	if start < 0 {
		return answer
	}
	return strings.TrimSpace(answer[start+len("<summary>") : end])
}
