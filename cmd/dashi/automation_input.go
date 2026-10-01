package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const automationStdinLimit = 16 << 20

// readAutomationStdin reads literal data only. The adapter supplies cancelRead
// only for an owned pipe whose Close unblocks Read, never for terminal stdin.
// A nil cancelRead is suitable for finite readers and regular files.
func readAutomationStdin(ctx context.Context, in io.Reader, cancelRead io.Closer) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if in == nil {
		return "", nil
	}
	if cancelRead != nil {
		done := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			_ = cancelRead.Close()
			close(done)
		})
		defer func() {
			if !stop() {
				<-done
			}
		}()
	}
	var data bytes.Buffer
	buf := make([]byte, 32<<10)
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		remaining := automationStdinLimit + 1 - data.Len()
		n, err := in.Read(buf[:min(len(buf), remaining)])
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if n > 0 {
			emptyReads = 0
			data.Write(buf[:n])
			if data.Len() > automationStdinLimit {
				return "", errors.New("stdin exceeds 16 MiB")
			}
		} else {
			emptyReads++
			if emptyReads >= 100 && err == nil {
				return "", io.ErrNoProgress
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// The input error can contain credentials. Keep it out of output.
				return "", errors.New("cannot read stdin")
			}
			break
		}
	}
	if bytes.IndexByte(data.Bytes(), 0) >= 0 {
		return "", errors.New("stdin contains NUL")
	}
	if !utf8.Valid(data.Bytes()) {
		return "", errors.New("stdin is not valid UTF-8")
	}
	return data.String(), nil
}

// combineAutomationInput has no command, attachment, approval, or admission
// side effects. Callers parse mentions in prompt only, before calling it.
func combineAutomationInput(prompt, stdin string, resume bool) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(prompt), "/") && stdin != "" {
		return "", errors.New("slash-prefixed prompt cannot include stdin")
	}
	combined := prompt + stdin
	if prompt != "" && stdin != "" {
		combined = prompt + "\n\n" + stdin
	}
	if strings.TrimSpace(combined) == "" && !(resume && combined == "") {
		return "", errors.New("request is empty")
	}
	return combined, nil
}
