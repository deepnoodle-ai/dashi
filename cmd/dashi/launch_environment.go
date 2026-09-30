package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/deepnoodle-ai/wonton/env"
)

func canonicalWorkspace(invocationDir, requested string) (string, error) {
	if requested == "" {
		requested = invocationDir
	}
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(invocationDir, requested)
	}
	path, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return "", fmt.Errorf("--workspace: cannot resolve directory")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("--workspace: cannot resolve directory")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("--workspace: needs an existing directory")
	}
	return path, nil
}

// loadNamedEnvironment loads only explicitly named files. Already-present
// variables win even when empty, and relative names use the invocation directory.
func loadNamedEnvironment(invocationDir string, files []string, lookup func(string) (string, bool), set func(string, string) error) (map[string]string, error) {
	origins := make(map[string]string)
	for _, name := range files {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(invocationDir, path)
		}
		values, err := env.ReadEnvFile(path)
		if err != nil {
			return nil, fmt.Errorf("--env-file: cannot read %s", path)
		}
		names := make([]string, 0, len(values))
		for key := range values {
			names = append(names, key)
		}
		slices.Sort(names)
		for _, key := range names {
			if !validConfigName(key) {
				continue
			}
			if _, exists := lookup(key); exists {
				continue
			}
			if err := set(key, values[key]); err != nil {
				return nil, fmt.Errorf("--env-file: cannot set variable %s", key)
			}
			origins[key] = path
		}
	}
	return origins, nil
}
