package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
)

func (c *Controller) destinationModel(ctx context.Context, name string, p *cliPreferences) {
	provider, model := "", name
	if name == "" {
		choices := p.modelChoices()
		labels := make([]string, len(choices))
		for i, choice := range choices {
			labels[i] = choice.label
		}
		if len(labels) == 0 {
			c.notice("No models are registered. Configure a destination in user settings.")
			return
		}
		var index int
		var ok bool
		if chooser, detailed := c.ui.(interface {
			ChooseDetailed(context.Context, string, []string, []string) (int, bool)
		}); detailed {
			details := make([]string, len(choices))
			for i, choice := range choices {
				details[i] = choice.detail
			}
			index, ok = chooser.ChooseDetailed(ctx, "Destination and model", labels, details)
		} else {
			for i, choice := range choices {
				labels[i] += " — " + strings.ReplaceAll(choice.detail, "\n", "; ")
			}
			index, ok = c.choose(ctx, "Destination and model", labels)
		}
		if !ok {
			return
		}
		if index < 0 || index >= len(choices) {
			c.notice("Model choice is no longer available.")
			return
		}
		provider, model = choices[index].provider, choices[index].model
	} else if first, rest, pair := strings.Cut(name, " "); pair {
		provider, model = first, strings.TrimSpace(rest)
	}
	selection, request, err := p.choose(ctx, provider, model)
	if err != nil {
		c.emit(warnBlock("%v. Keeping the previous selection.", err))
		return
	}
	p.mu.Lock()
	p.launch.Selection = selection
	p.mu.Unlock()
	c.eng.SetModel(selection.Model)
	c.notice("Next turn: %s %s.", selection.Destination.ID, selection.Model)
	for _, warning := range request.Warnings() {
		c.notice("%s", warning.Message)
	}
	if err := c.eng.saveNextPreferences(); err != nil {
		c.emit(warnBlock("Selection is active, but session preferences were not saved: %v.", err))
	}
	if err := p.writer.update(map[string]json.RawMessage{"provider": preferenceJSON(selection.Destination.ID), "model": preferenceJSON(selection.Model)}); err != nil {
		c.emit(warnBlock("Selection is active, but user preferences were not saved: %v.", err))
	}
}

func (c *Controller) generationControl(ctx context.Context, command, value string) {
	p := c.eng.cfg.Preferences
	if p == nil {
		c.notice("Generation controls need configured destinations.")
		return
	}
	flag, field := "", ""
	switch command {
	case "/effort":
		flag, field = "--thinking-effort", "thinking_effort"
	case "/thinking":
		flag, field = "--show-thinking", "show_thinking"
		if value == "on" {
			value = "true"
		} else if value == "off" {
			value = "false"
		} else {
			c.notice("Usage: /thinking on|off")
			return
		}
	case "/temperature":
		flag, field = "--temperature", "temperature"
	case "/max-tokens":
		flag, field = "--max-tokens", "max_tokens"
	}
	if value == "" {
		c.notice("%s needs a value. Use /settings to inspect controls.", command)
		return
	}
	parsed, err := parseArgs([]string{flag + "=" + value})
	if err != nil {
		c.emit(warnBlock("%v. Keeping the previous preference.", err))
		return
	}
	p.mu.RLock()
	current := p.launch
	p.mu.RUnlock()
	base, err := reopenedPreferences(p.metadata(), "")
	if err != nil {
		c.emit(warnBlock("%v.", err))
		return
	}
	controls, err := resolveControls([]generationTier{{generationPreferences: parsed.Controls, Source: "session change"}, {generationPreferences: base.generationPreferences, Source: "current session"}})
	if err != nil {
		c.emit(warnBlock("%v.", err))
		return
	}
	for name, source := range current.Controls.Sources {
		if name != field {
			controls.Sources[name] = source
		}
	}
	source := controls.Sources[field]
	source.Overridden = nil
	if old := current.Controls.Sources[field]; old.Winning != "" {
		source.Overridden = append([]string{old.Winning}, old.Overridden...)
	}
	controls.Sources[field] = source
	agent, err := selectionAgent(current.Selection, controls.Settings)
	if err != nil {
		c.emit(warnBlock("%v.", err))
		return
	}
	client, err := p.clientFor(ctx, agent)
	if err != nil {
		c.emit(warnBlock("%v. Keeping the previous preference.", err))
		return
	}
	request, err := client.Prepare(ctx, llm.Input{Model: agent.Model, Stream: true, Settings: controls.Settings, Messages: []llm.Message{llm.UserMessage(llm.Text("Preparation preview."))}})
	if err != nil {
		c.emit(warnBlock("%v. Keeping the previous preference.", err))
		return
	}
	p.mu.Lock()
	p.launch.Controls = controls
	p.mu.Unlock()
	c.notice("%s set for the next turn.", field)
	for _, warning := range request.Warnings() {
		c.notice("%s", warning.Message)
	}
	if err := c.eng.saveNextPreferences(); err != nil {
		c.emit(warnBlock("Preference is active, but session preferences were not saved: %v.", err))
	}
	var saved any
	switch field {
	case "thinking_effort":
		saved = parsed.Controls.Effort.Value
	case "show_thinking":
		saved = parsed.Controls.ShowThinking.Value
	case "temperature":
		if !parsed.Controls.Temperature.Null {
			saved = parsed.Controls.Temperature.Value
		}
	case "max_tokens":
		saved = parsed.Controls.MaxTokens.Value
	}
	if err := p.writer.update(map[string]json.RawMessage{field: preferenceJSON(saved)}); err != nil {
		c.emit(warnBlock("Preference is active, but user preferences were not saved: %v.", err))
	}
}
