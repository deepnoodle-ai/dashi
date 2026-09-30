package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitEnvironmentFilesUseInvocationAndKeepEmptyProcessValues(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "fixture.env")
	if err := os.WriteFile(file, []byte("EMPTY=do-not-override\nNEW='$(touch nope)'\nOTHER=${NEW}\nmalformed line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := values[k]; return v, ok }
	set := func(k, v string) error { values[k] = v; return nil }
	origins, err := loadNamedEnvironment(dir, []string{"fixture.env"}, lookup, set)
	if err != nil || values["EMPTY"] != "" || values["NEW"] != "$(touch nope)" || values["OTHER"] != "${NEW}" || origins["NEW"] != file || origins["EMPTY"] != "" {
		t.Fatalf("values=%v origins=%v err=%v", values, origins, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nope")); !os.IsNotExist(err) {
		t.Fatal("shell text executed")
	}
	untouched := map[string]string{}
	_, err = loadNamedEnvironment(dir, nil, func(string) (string, bool) { return "", false }, func(k, v string) error { untouched[k] = v; return nil })
	if err != nil || len(untouched) != 0 {
		t.Fatal("unnamed env file loaded")
	}
}
func TestWorkspaceCanonicalizesBeforeUsingProjectFiles(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	got, err := canonicalWorkspace(dir, "link")
	want, _ := filepath.EvalSymlinks(real)
	if err != nil || got != want {
		t.Fatalf("got=%s want=%s err=%v", got, want, err)
	}
	if _, err := canonicalWorkspace(dir, "missing"); err == nil {
		t.Fatal("missing workspace accepted")
	}
}
