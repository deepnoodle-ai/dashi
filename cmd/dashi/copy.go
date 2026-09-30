package main

import (
	"fmt"
	"strconv"
	"strings"
)

// codeBlock is one fenced block of a markdown reply.
type codeBlock struct {
	lang string
	text string
}

// lines is how many lines the block holds.
func (b codeBlock) lines() int { return strings.Count(b.text, "\n") + 1 }

// summary is the block's first line with anything on it, for the list.
func (b codeBlock) summary() string {
	for _, line := range strings.Split(b.text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// codeBlocks pulls the fenced blocks out of markdown, in order.
//
// The scan is deliberately literal: a fence is a line whose first non-space run
// is three or more backticks or tildes, and it closes on a line whose fence is
// at least as long and of the same character. That is what CommonMark says, and
// it is what makes a block containing ``` inside a ```` fence come out whole.
func codeBlocks(markdown string) []codeBlock {
	if markdown == "" {
		return nil
	}
	var blocks []codeBlock
	var open bool
	var fenceChar byte
	var fenceLen int
	var lang string
	var body []string

	for _, line := range strings.Split(markdown, "\n") {
		char, length, rest, isFence := parseFence(line)
		switch {
		case !open && isFence:
			open, fenceChar, fenceLen = true, char, length
			lang = ""
			if fields := strings.Fields(rest); len(fields) > 0 {
				lang = fields[0]
			}
			body = body[:0]
		case open && isFence && char == fenceChar && length >= fenceLen && strings.TrimSpace(rest) == "":
			blocks = append(blocks, codeBlock{lang: lang, text: strings.Join(body, "\n")})
			open = false
		case open:
			body = append(body, line)
		}
	}
	// An unclosed fence is still a block: a reply cut off mid-stream is exactly
	// when someone wants the code out of it.
	if open && len(body) > 0 {
		blocks = append(blocks, codeBlock{lang: lang, text: strings.Join(body, "\n")})
	}
	return blocks
}

// parseFence reports whether line opens or closes a fence, along with the fence
// character, its length, and whatever follows it on the line.
func parseFence(line string) (char byte, length int, rest string, ok bool) {
	trimmed := strings.TrimLeft(line, " ")
	// More than three leading spaces is an indented code block, not a fence.
	if len(line)-len(trimmed) > 3 || trimmed == "" {
		return 0, 0, "", false
	}
	char = trimmed[0]
	if char != '`' && char != '~' {
		return 0, 0, "", false
	}
	for length < len(trimmed) && trimmed[length] == char {
		length++
	}
	if length < 3 {
		return 0, 0, "", false
	}
	rest = trimmed[length:]
	// An info string may not contain a backtick, which is what keeps inline
	// code spans from being read as fences.
	if char == '`' && strings.ContainsRune(rest, '`') {
		return 0, 0, "", false
	}
	return char, length, rest, true
}

func lastAssistantText(blocks []Block) string {
	for i := len(blocks) - 1; i >= 0; i-- {
		switch blocks[i].Kind {
		case KindAssistant:
			return blocks[i].Text
		case KindUser:
			return ""
		}
	}
	return ""
}

func copyRequest(blocks []Block, selection, arg string) (text, notice string) {
	arg = strings.TrimSpace(arg)
	if arg == "" && selection != "" {
		return selection, ""
	}
	code := codeBlocks(lastAssistantText(blocks))
	if len(code) == 0 {
		return "", "Nothing selected, and the latest answer has no code blocks."
	}
	if arg == "" {
		var report strings.Builder
		report.WriteString("Code blocks in the latest answer:\n")
		for i, b := range code {
			fmt.Fprintf(&report, "%d. %s (%d lines): %s\n", i+1, orDefault(b.lang, "text"), b.lines(), truncate(b.summary(), 48))
		}
		report.WriteString("Use /copy N or /copy all.")
		return "", report.String()
	}
	if strings.EqualFold(arg, "all") {
		var texts []string
		for _, b := range code {
			texts = append(texts, b.text)
		}
		return strings.Join(texts, "\n"), ""
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > len(code) {
		return "", fmt.Sprintf("Choose a code block from 1 to %d. Use /copy to list them.", len(code))
	}
	return code[n-1].text, ""
}
