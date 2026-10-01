package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestAttachmentOversizeDiagnosticBoundaryAndNoDispatch(t *testing.T) {
	for _, size := range []int{262144, 262145} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			cfg := testConfig(t, say("boundary accepted"))
			fixtureSelectedText(t, cfg.Dir, "requirements.txt", strings.Repeat("x", size))
			var stdout, stderr bytes.Buffer
			code := runPrint(t.Context(), cfg, Options{Print: "inspect", PrintSet: true, AttachFiles: []string{"requirements.txt"}}, &stdout, &stderr)
			if size == 262144 {
				if code != 0 || cfg.Client.(*fakeClient).sends != 1 {
					t.Fatalf("boundary rejected: code=%d %s", code, stderr.String())
				}
				return
			}
			if code != 1 || cfg.Client.(*fakeClient).sends != 0 || stdout.Len() != 0 {
				t.Fatal("oversized selection dispatched or wrote stdout")
			}
			wants := []string{"requirements.txt", "text/plain", "262144-byte limit", "select a smaller file"}
			for _, want := range wants {
				mustHave(t, stderr.String(), want)
			}
			u := newLineUI(&bytes.Buffer{})
			a, err := buildApp(t.Context(), cfg, u)
			if err != nil {
				t.Fatal(err)
			}
			defer a.eng.Close()
			u.ctrl = a.ctrl
			u.AttachmentCommand("/attach", "requirements.txt")
			rows := strings.Join(attachmentRows(&u.attachments, cfg.Dir), "\n")
			for _, want := range wants {
				mustHave(t, rows, want)
			}
		})
	}
	err := attachmentSelectionError(strings.Repeat("x", 1000)+"\x1b[31m.txt", fmt.Errorf("fixture"))
	if strings.Contains(err.Error(), "\x1b") || len([]rune(err.Error())) > 140 {
		t.Fatalf("unsafe or unbounded filename: %q", err)
	}
}
