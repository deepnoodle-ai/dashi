package main

import (
	"context"
	"encoding/json"
	"strings"
)

func (c *Controller) destinationModel(ctx context.Context, name string, p *cliPreferences) {
	provider, model := "", name
	if name == "" {
		labels := p.modelLabels()
		if len(labels) == 0 {
			c.notice("No models are registered. Configure a destination in user settings.")
			return
		}
		index, ok := c.choose(ctx, "Destination and model", labels)
		if !ok {
			return
		}
		provider, model, _ = strings.Cut(labels[index], " ")
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
