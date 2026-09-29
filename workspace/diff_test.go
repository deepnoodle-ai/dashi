package workspace

import "testing"

func TestUnifiedDiffShowsChangeWithContext(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\n"
	updated := "a\nb\nc\nd\nE\nf\ng\nh\n"
	want := "--- x\n+++ x\n@@ -2,7 +2,7 @@\n b\n c\n d\n-e\n+E\n f\n g\n h"
	if got := unifiedDiff("x", old, updated); got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffSplitsDistantChanges(t *testing.T) {
	old := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	updated := "one\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\ntwelve\n"
	got := unifiedDiff("x", old, updated)
	want := "--- x\n+++ x\n@@ -1,4 +1,4 @@\n-1\n+one\n 2\n 3\n 4\n@@ -9,4 +9,4 @@\n 9\n 10\n 11\n-12\n+twelve"
	if got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffForNewFileAndNoChange(t *testing.T) {
	if got := unifiedDiff("x", "same", "same"); got != "" {
		t.Fatalf("diff = %q", got)
	}
	want := "--- x\n+++ x\n@@ -0,0 +1,2 @@\n+a\n+b"
	if got := unifiedDiff("x", "", "a\nb\n"); got != want {
		t.Fatalf("diff:\n%s", got)
	}
}
