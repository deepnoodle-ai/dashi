package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// defaultModel is the model when no flag, variable, or settings file names
// one.
const defaultModel = "gpt-6-luna"

// Settings holds the values a settings file can set.
type Settings struct {
	Model string `json:"model,omitempty"`
}

// loadSettings reads the user file, then the repository file. A value in the
// repository file wins. A missing file is not an error; an unreadable one
// gives a warning and is skipped.
func loadSettings(home, dir string) (Settings, []string) {
	var s Settings
	var warnings []string
	for _, path := range []string{
		filepath.Join(home, ".dashi", "settings.json"),
		filepath.Join(dir, ".dashi", "settings.json"),
	} {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			warnings = append(warnings, path+": "+err.Error())
			continue
		}
		var next Settings
		if err := json.Unmarshal(data, &next); err != nil {
			warnings = append(warnings, path+": "+err.Error())
			continue
		}
		if next.Model != "" {
			s.Model = next.Model
		}
	}
	return s, warnings
}

// chooseModel applies the order: flag, DASHI_MODEL, settings file, default.
func chooseModel(flagValue string, getenv func(string) string, s Settings) string {
	for _, v := range []string{flagValue, getenv("DASHI_MODEL"), s.Model} {
		if v != "" {
			return v
		}
	}
	return defaultModel
}
