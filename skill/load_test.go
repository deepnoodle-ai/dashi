package skill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/skill"
)

// writeTree writes files under root; keys are slash paths.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func info(set *skill.Set, name string) (skill.Info, bool) {
	for _, i := range set.Skills() {
		if i.Name == name {
			return i, true
		}
	}
	return skill.Info{}, false
}

func TestLoadDirsReadsFolderSkillsAndCommandFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"review/SKILL.md": "---\nname: review\ndescription: >\n" +
			"  Review a change\n  for bugs.\nargument-hint: <path>\n---\n" +
			"Review $1 now.\n",
		"review/checklist.md": "supporting file",
		"plain/SKILL.md":      "No frontmatter here.",
		"deploy.md":           "Deploy it.",
		"README.md":           "ignored",
		".hidden/SKILL.md":    "ignored",
	})
	set, warns := skill.LoadDirs(root, filepath.Join(root, "missing"))
	if len(warns) != 0 {
		t.Fatalf("warnings = %+v", warns)
	}
	var names []string
	for _, i := range set.Skills() {
		names = append(names, i.Name)
	}
	if strings.Join(names, ",") != "deploy,plain,review" {
		t.Fatalf("names = %v", names)
	}
	review, _ := info(set, "review")
	if review.Description != "Review a change for bugs." ||
		review.ArgumentHint != "<path>" ||
		review.Dir != filepath.Join(root, "review") ||
		!review.ModelInvocable ||
		!review.UserInvocable {
		t.Fatalf("review = %+v", review)
	}
	deploy, _ := info(set, "deploy")
	if deploy.ModelInvocable || !deploy.UserInvocable || deploy.Dir != "" {
		t.Fatalf("command file = %+v", deploy)
	}
}

func TestLoadDirsWarnsForEachBadFileAndKeepsGoodOnes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"good-one/SKILL.md":  "Body one.",
		"good-two.md":        "Body two.",
		"unclosed/SKILL.md":  "---\nname: unclosed\nBody",
		"badyaml/SKILL.md":   "---\nname: [oops\n---\nBody",
		"badname/SKILL.md":   "---\nname: has space\n---\nBody",
		"empty/SKILL.md":     "---\nname: empty\n---\n  \n",
		"long/SKILL.md":      "---\ndescription: " + strings.Repeat("d", 1025) + "\n---\nBody",
		"huge/SKILL.md":      strings.Repeat("b", 64<<10+1),
		"git/commit/one.md":  "nested",
		"dup-a/SKILL.md":     "---\nname: same\n---\nBody A",
		"dup-b/SKILL.md":     "---\nname: SAME\n---\nBody B",
		"onlysupport/x.txt":  "no skill file",
		"binary.md":          "\xff\xfe\x00",
		"empty-cmd.md":       "",
		"onlyfrontmatter.md": "---\nname: ofm\n---\n",
	})
	set, warns := skill.LoadDirs(root)
	var names []string
	for _, i := range set.Skills() {
		names = append(names, i.Name)
	}
	if strings.Join(names, ",") != "good-one,good-two,same" {
		t.Fatalf("names = %v", names)
	}
	want := []string{
		"unclosed", "badyaml", "badname", "empty", "long", "huge",
		"git", "dup-b", "onlysupport", "binary.md", "empty-cmd.md",
		"onlyfrontmatter.md",
	}
	for _, name := range want {
		found := false
		for _, w := range warns {
			if strings.Contains(w.Path, string(filepath.Separator)+name) &&
				w.Message != "" {
				found = true
			}
		}
		if !found {
			t.Errorf("no warning names %q; warnings = %+v", name, warns)
		}
	}
	if len(warns) != len(want) {
		t.Errorf("got %d warnings, want %d: %+v", len(warns), len(want), warns)
	}
}

func TestLoadDirsFirstDirWinsWithoutWarning(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	writeTree(t, project, map[string]string{"review/SKILL.md": "project body"})
	writeTree(t, home, map[string]string{
		"review/SKILL.md": "home body",
		"other/SKILL.md":  "other body",
	})
	set, warns := skill.LoadDirs(project, home)
	if len(warns) != 0 {
		t.Fatalf("warnings = %+v", warns)
	}
	body, ok := set.Expand("Review", "")
	if !ok || !strings.HasSuffix(body, "project body") {
		t.Fatalf("expand = %q, %v", body, ok)
	}
	if len(set.Skills()) != 2 {
		t.Fatalf("skills = %+v", set.Skills())
	}
}

func TestLoadDirsFollowsSymlinkedFolder(t *testing.T) {
	root, elsewhere := t.TempDir(), t.TempDir()
	writeTree(t, elsewhere, map[string]string{"SKILL.md": "linked body"})
	if err := os.Symlink(elsewhere, filepath.Join(root, "linked")); err != nil {
		t.Skip("symlinks unavailable")
	}
	set, warns := skill.LoadDirs(root)
	if _, ok := info(set, "linked"); !ok || len(warns) != 0 {
		t.Fatalf("skills = %+v, warnings = %+v", set.Skills(), warns)
	}
}

func TestLoadDirsFlagsControlModelAndUserUse(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"user-only/SKILL.md": "---\ndisable-model-invocation: true\n---\nUser.",
		"model-only/SKILL.md": "---\nuser-invocable: false\n" +
			"allowed-tools: [bash]\n---\nModel.",
	})
	set, _ := skill.LoadDirs(root)
	if _, ok := set.Expand("model-only", ""); ok {
		t.Fatal("model-only skill expanded for the user")
	}
	if _, ok := set.Expand("user-only", ""); !ok {
		t.Fatal("user-only skill did not expand")
	}
	bundle, err := set.Bundle(skill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	catalog := bundle.Catalog()
	if !strings.Contains(catalog, "model-only") ||
		strings.Contains(catalog, "user-only") {
		t.Fatalf("catalog = %q", catalog)
	}
}

func TestLoadDirsDropsSkillsThatOverflowCatalog(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := range 70 {
		name := "skill-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		files[name+"/SKILL.md"] = "---\ndescription: " +
			strings.Repeat("x", 1000) + "\n---\nBody."
	}
	writeTree(t, root, files)
	set, warns := skill.LoadDirs(root)
	if len(warns) == 0 {
		t.Fatal("no warning for the overflowing catalog")
	}
	for _, w := range warns {
		if !strings.Contains(w.Message, "left out of the model catalog") {
			t.Fatalf("warning = %+v", w)
		}
	}
	bundle, err := set.Bundle(skill.Options{ToolName: strings.Repeat("t", 64)})
	if err != nil {
		t.Fatalf("bundle failed after the loader fit the catalog: %v", err)
	}
	if len(bundle.Catalog()) > 64<<10 {
		t.Fatalf("catalog is %d bytes", len(bundle.Catalog()))
	}
	dropped := 0
	for _, i := range set.Skills() {
		if !i.ModelInvocable {
			dropped++
			if _, ok := set.Expand(i.Name, ""); !ok {
				t.Fatalf("dropped skill %q is not user-invocable", i.Name)
			}
		}
	}
	if dropped != len(warns) || dropped == 0 {
		t.Fatalf("dropped %d skills, %d warnings", dropped, len(warns))
	}
}

func TestSetBundleFailsOnlyOnBadToolName(t *testing.T) {
	set, _ := skill.LoadDirs()
	if _, err := set.Bundle(skill.Options{ToolName: "bad name"}); err == nil {
		t.Fatal("bundle accepted a bad tool name")
	}
	bundle, err := set.Bundle(skill.Options{})
	if err != nil || bundle.Catalog() != "" {
		t.Fatalf("empty set bundle: %v, %q", err, bundle.Catalog())
	}
}

func TestDefaultDirsOrdersProjectBeforeHome(t *testing.T) {
	dirs := skill.DefaultDirs("/repo", "/home/u")
	if len(dirs) != 10 ||
		dirs[0] != filepath.Join("/repo", ".dashi", "skills") ||
		dirs[5] != filepath.Join("/home/u", ".dashi", "skills") {
		t.Fatalf("dirs = %v", dirs)
	}
	if got := skill.DefaultDirs("", "/home/u"); len(got) != 5 {
		t.Fatalf("home only = %v", got)
	}
}

func TestLoadedSkillsDoNotDependOnDirOrder(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, map[string]string{"one/SKILL.md": "1"})
	writeTree(t, b, map[string]string{"two/SKILL.md": "2"})
	first, _ := skill.LoadDirs(a, b)
	second, _ := skill.LoadDirs(b, a)
	x, _ := first.Bundle(skill.Options{})
	y, _ := second.Bundle(skill.Options{})
	if x.Catalog() != y.Catalog() {
		t.Fatalf("catalogs differ:\n%s\n%s", x.Catalog(), y.Catalog())
	}
}

func TestExpandSubstitutesArgumentsAndAddsBaseDir(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"folder/SKILL.md": "Check $1 then $2 (all: $ARGUMENTS) $3.",
		"plain/SKILL.md":  "Do the thing.",
		"cmd.md":          "Command body.",
	})
	set, _ := skill.LoadDirs(root)
	dir := filepath.Join(root, "folder")
	tests := []struct {
		name, skill, args, want string
	}{
		{
			name:  "placeholders",
			skill: "folder",
			args:  "a b",
			want:  "Base directory: " + dir + "\n\nCheck a then b (all: a b) .",
		},
		{
			name:  "appends arguments without placeholder",
			skill: "plain",
			args:  "src/api",
			want: "Base directory: " + filepath.Join(root, "plain") +
				"\n\nDo the thing.\n\nARGUMENTS: src/api",
		},
		{
			name:  "no arguments",
			skill: "cmd",
			want:  "Command body.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := set.Expand(tt.skill, tt.args)
			if !ok || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
	if _, ok := set.Expand("unknown", ""); ok {
		t.Fatal("unknown name expanded")
	}
}

func TestLoadSkillToolAddsBaseDirectoryForFolderSkills(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"review/SKILL.md": "Review body."})
	set, _ := skill.LoadDirs(root)
	bundle, err := set.Bundle(skill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := outputText(load(t, bundle, "review"))
	want := "Base directory: " + filepath.Join(root, "review") +
		"\n\n# review\n\nReview body."
	if out != want {
		t.Fatalf("load output = %q, want %q", out, want)
	}
}

func TestLoadDirsAcceptsClaudeCodeCommandFiles(t *testing.T) {
	set, warns := skill.LoadDirs("testdata/claude-commands")
	if len(warns) != 0 {
		t.Fatalf("warnings = %+v", warns)
	}
	tests := []struct {
		name, description, hint string
	}{
		{"commit", "Create a git commit", "[message]"},
		{
			"review-pr",
			"Review pull request: check style and tests",
			"[pr-number] [priority] [assignee]",
		},
		{
			"fix-issue",
			"Fix issue #$ARGUMENTS following our coding standards",
			"[issue-number]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := info(set, tt.name)
			if !ok {
				t.Fatalf("%s did not load", tt.name)
			}
			if got.Description != tt.description ||
				got.ArgumentHint != tt.hint ||
				!got.UserInvocable {
				t.Fatalf("info = %+v", got)
			}
		})
	}
	body, ok := set.Expand("review-pr", "12 high alice")
	if !ok || body != "Review PR #12 with priority high and assign it to alice." {
		t.Fatalf("expand = %q, %v", body, ok)
	}
}

func TestLoadDirsIgnoresWrongTypeFieldAndNamesIt(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"odd/SKILL.md": "---\ndescription: {a: b}\n" +
			"disable-model-invocation: [x]\n---\nBody.",
	})
	set, warns := skill.LoadDirs(root)
	if _, ok := info(set, "odd"); !ok {
		t.Fatal("skill with wrong-type fields did not load")
	}
	if len(warns) != 2 {
		t.Fatalf("warnings = %+v", warns)
	}
	for i, field := range []string{`"description"`, `"disable-model-invocation"`} {
		if !strings.Contains(warns[i].Path, "odd") ||
			!strings.Contains(warns[i].Message, field) {
			t.Errorf("warning %d = %+v", i, warns[i])
		}
	}
}

func TestLoadDirsListDescriptionWarnsAndArgumentHintList(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"listed/SKILL.md": "---\ndescription: [a, b]\nargument-hint: [x, y]\n---\nBody",
	})
	set, warns := skill.LoadDirs(root)
	got, ok := info(set, "listed")
	if !ok {
		t.Fatalf("skill not loaded; warnings = %+v", warns)
	}
	if got.Description != "" || got.ArgumentHint != "[x, y]" {
		t.Fatalf("info = %+v", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0].Path, "listed") ||
		!strings.Contains(warns[0].Message, `"description"`) {
		t.Fatalf("warnings = %+v", warns)
	}
}
