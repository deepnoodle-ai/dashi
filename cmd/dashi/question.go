package main

import (
	"fmt"
	"strconv"
	"strings"
)

// taskQuestion is a question as a UI shows it.
type taskQuestion struct {
	// Question is the printable form of the question.
	Question
	// Task is the short ID of the delegated task that asks. It is empty for
	// a question of the foreground conversation.
	Task string
}

// taskOrigin names the delegated task that asks, and how to inspect it.
func taskOrigin(task string) string {
	return fmt.Sprintf("From task %s · /task %s shows it", task, task)
}

// Choices are indexes, so displaying a printable label cannot change the
// original answer or merge two labels that look alike after sanitization.
type questionAnswer struct {
	text    string
	choices []int
}

func parseQuestionAnswer(q Question, line string) (questionAnswer, error) {
	text := strings.TrimSpace(line)
	if !q.Multiple {
		if i, err := strconv.Atoi(text); err == nil && i >= 1 && i <= len(q.Choices) {
			return questionAnswer{choices: []int{i - 1}}, nil
		}
		return questionAnswer{text: line}, nil
	}
	if own, ok := strings.CutPrefix(text, "other:"); ok {
		if strings.TrimSpace(own) != "" {
			return questionAnswer{text: strings.TrimSpace(own)}, nil
		}
		return questionAnswer{}, fmt.Errorf("Enter your own words after other:")
	}
	if text == "" {
		return questionAnswer{}, fmt.Errorf("Choose at least one option, or enter other: YOUR ANSWER")
	}
	var chosen []int
	seen := map[int]bool{}
	for _, value := range strings.Split(text, ",") {
		i, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || i < 1 || i > len(q.Choices) || seen[i] {
			return questionAnswer{}, fmt.Errorf("Choose distinct offered option numbers, or enter other: YOUR ANSWER")
		}
		seen[i] = true
		chosen = append(chosen, i-1)
	}
	return questionAnswer{choices: chosen}, nil
}
