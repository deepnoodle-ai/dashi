package main

import "testing"

func TestProjects(t *testing.T) {
	const home = "/home/me"
	remote := func(r ...string) map[string]bool {
		m := map[string]bool{}
		for _, x := range r {
			m[x] = true
		}
		return m
	}
	dirs := []projectDir{
		{Dir: home + "/git/app"},
		{Dir: home + "/git/app/web"},
		{Dir: home + "/git/app/.claude/worktrees/fix-login"},
		{Dir: home + "/git/app/.claude/worktrees/fix-login/web"},
		{Dir: home + "/git/tool", Remotes: remote("github.com/me/tool")},
		{Dir: home + "/lib/dive", Remotes: remote("github.com/a/dive")},
		{Dir: home + "/git/dive", Remotes: remote("github.com/b/dive")},
		{Dir: home + "/old/dive"},
		{Dir: home},
		{Dir: ""},
		{Dir: "/srv/other"},
		{Dir: home + "/.codex/worktrees/1a2b/tool"},                                           // one match
		{Dir: home + "/.codex/worktrees/1a2b/tool/sub"},                                       // one match, subfolder
		{Dir: home + "/.codex/worktrees/3c4d/dive", Remotes: remote("github.com/b/dive")},     // several, one remote
		{Dir: home + "/.codex/worktrees/5e6f/dive"},                                           // several, no remote
		{Dir: home + "/.codex/worktrees/7a8b/dive", Remotes: remote("github.com/c/dive")},     // several, no shared remote
		{Dir: home + "/.codex/worktrees/9c0d/nothing", Remotes: remote("github.com/me/tool")}, // no match
		{Dir: home + "/.codex/worktrees/app"},                                                 // no name
	}
	want := map[string]string{
		home + "/git/app":                                 "~/git/app",
		home + "/git/app/web":                             "~/git/app/web",
		home + "/git/app/.claude/worktrees/fix-login":     "~/git/app",
		home + "/git/app/.claude/worktrees/fix-login/web": "~/git/app",
		home + "/git/tool":                                "~/git/tool",
		home + "/lib/dive":                                "~/lib/dive",
		home + "/git/dive":                                "~/git/dive",
		home + "/old/dive":                                "~/old/dive",
		home:                                              "~",
		"":                                                "(none)",
		"/srv/other":                                      "/srv/other",
		home + "/.codex/worktrees/1a2b/tool":              "~/git/tool",
		home + "/.codex/worktrees/1a2b/tool/sub":          "~/git/tool",
		home + "/.codex/worktrees/3c4d/dive":              "~/git/dive",
		home + "/.codex/worktrees/5e6f/dive":              "~/.codex/worktrees/*/dive",
		home + "/.codex/worktrees/7a8b/dive":              "~/.codex/worktrees/*/dive",
		home + "/.codex/worktrees/9c0d/nothing":           "~/.codex/worktrees/*/nothing",
		home + "/.codex/worktrees/app":                    "~/.codex/worktrees/app",
	}
	got := projects(dirs, home)
	for dir, w := range want {
		if got[dir] != w {
			t.Errorf("%q: got %q, want %q", dir, got[dir], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d projects, want %d", len(got), len(want))
	}
}
