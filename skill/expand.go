package skill

import (
	"regexp"
	"strconv"
	"strings"
)

var placeholder = regexp.MustCompile(`\$ARGUMENTS|\$[1-9]`)

// Expand returns the text that runs when a user invokes a skill by name, for
// example when a CLI user types "/review src/api". The result is the skill
// body, with the base-directory line for a folder skill, for use as the
// user's prompt. It replaces $ARGUMENTS with args and $1 to $9 with the
// space-separated words of args; a missing word becomes empty text. If the
// body has no placeholder and args is not empty, Expand appends "ARGUMENTS:"
// and args. Name matching ignores case.
//
// Expand returns false for an unknown name and for a skill with
// user-invocable set to false. Call Skills to tell the two cases apart.
func (s *Set) Expand(name, args string) (string, bool) {
	if s == nil {
		return "", false
	}
	name = strings.TrimSpace(name)
	args = strings.TrimSpace(args)
	for _, e := range s.entries {
		if !strings.EqualFold(e.info.Name, name) {
			continue
		}
		if !e.info.UserInvocable {
			return "", false
		}
		return withBaseDir(e.info.Dir, substitute(e.body, args)), true
	}
	return "", false
}

func substitute(body, args string) string {
	if !placeholder.MatchString(body) {
		if args == "" {
			return body
		}
		return body + "\n\nARGUMENTS: " + args
	}
	words := strings.Fields(args)
	return placeholder.ReplaceAllStringFunc(body, func(match string) string {
		if match == "$ARGUMENTS" {
			return args
		}
		i, _ := strconv.Atoi(match[1:])
		if i > len(words) {
			return ""
		}
		return words[i-1]
	})
}
