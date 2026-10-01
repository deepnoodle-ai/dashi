//go:build !darwin

package main

import "golang.org/x/term"

func composerTTYAttributes(fd int) (any, error) { return term.GetState(fd) }
