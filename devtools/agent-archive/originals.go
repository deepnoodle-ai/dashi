package main

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Apps, as recorded in the copy log and the database.
const (
	appClaudeCode = "claude_code"
	appCodex      = "codex"
)

// Kinds of copies, as recorded in the copy log and the database.
const (
	kindSession        = "session"
	kindSubagent       = "subagent"
	kindSubagentMeta   = "subagent_meta"
	kindToolResult     = "tool_result"
	kindPromptLog      = "prompt_log"
	kindSessionTitles  = "session_titles"
	kindFileHistory    = "file_history"
	kindPaste          = "paste"
	kindAttachment     = "attachment"
	kindGeneratedImage = "generated_image"
	kindMemory         = "memory"
	kindOther          = "other"
)

// Top-level archive folders that hold copies. Anything else in the archive
// (snapshots/, logs/, the copy log, the database) is not a copy.
var appFolders = map[string]string{
	appClaudeCode: "claude-code",
	appCodex:      "codex",
}

// An original is one file an app wrote, as listed at the start of a run.
type original struct {
	Path       string // absolute path of the original
	App        string
	Kind       string
	CopyPath   string // archive-relative, slash-separated
	Size       int64  // size read at the start; exactly this many bytes are copied
	ModifiedAt time.Time
}

// What is copied from each app (decision 2), relative to the app's directory.
// A trailing slash marks a folder, copied with everything below it.
var (
	claudeCodeSources = []string{"projects/", "history.jsonl", "file-history/", "paste-cache/"}
	codexSources      = []string{
		"sessions/", "archived_sessions/", "session_index.jsonl", "history.jsonl",
		"attachments/", "generated_images/", "memories/",
	}
)

// listResult is what one app's listing found.
type listResult struct {
	Originals []original
	Warnings  []string
	// Complete is true when the app's directory exists and every listed
	// folder was read without error, so a copy whose original was not seen
	// can be marked gone.
	Complete bool
}

// listOriginals lists the originals of one app below dir.
func listOriginals(app, dir string) listResult {
	var res listResult
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		res.Warnings = append(res.Warnings, app+": no directory at "+dir+"; skipped")
		return res
	}
	res.Complete = true
	sources := claudeCodeSources
	if app == appCodex {
		sources = codexSources
	}
	byCopyPath := map[string]int{}
	add := func(rel string, info fs.FileInfo) {
		if !info.Mode().IsRegular() || skipName(app, path.Base(rel)) {
			return
		}
		o := original{
			Path:       filepath.Join(dir, filepath.FromSlash(rel)),
			App:        app,
			Kind:       classify(app, rel),
			CopyPath:   copyPathFor(app, rel),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		}
		// Two originals for one copy happen when Codex moves a session file
		// between sessions/ and archived_sessions/ mid-listing. Keep the
		// larger, then the newer.
		if i, ok := byCopyPath[o.CopyPath]; ok {
			prev := res.Originals[i]
			if o.Size > prev.Size || (o.Size == prev.Size && o.ModifiedAt.After(prev.ModifiedAt)) {
				res.Originals[i] = o
			}
			return
		}
		byCopyPath[o.CopyPath] = len(res.Originals)
		res.Originals = append(res.Originals, o)
	}
	for _, src := range sources {
		if !strings.HasSuffix(src, "/") {
			info, err := os.Lstat(filepath.Join(dir, src))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				res.Complete = false
				res.Warnings = append(res.Warnings, "list "+src+": "+err.Error())
				continue
			}
			add(src, info)
			continue
		}
		root := filepath.Join(dir, filepath.FromSlash(strings.TrimSuffix(src, "/")))
		if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				res.Complete = false
				res.Warnings = append(res.Warnings, "list "+p+": "+err.Error())
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil // removed while listing
				}
				res.Complete = false
				res.Warnings = append(res.Warnings, "list "+p+": "+err.Error())
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			add(filepath.ToSlash(rel), info)
			return nil
		})
		if err != nil {
			res.Complete = false
			res.Warnings = append(res.Warnings, "list "+src+": "+err.Error())
		}
	}
	return res
}

// isCopied reports whether copy copies the file at an app-relative path:
// it is below one of the app's sources and not skipped by name.
func isCopied(app, rel string) bool {
	sources := claudeCodeSources
	if app == appCodex {
		sources = codexSources
	}
	for _, src := range sources {
		if rel == src || (strings.HasSuffix(src, "/") && strings.HasPrefix(rel, src)) {
			return !skipName(app, path.Base(rel))
		}
	}
	return false
}

// skipName reports files inside copied folders that are never copied:
// Codex's SQLite files (built from the session files) and Finder's
// .DS_Store files, which change on their own.
func skipName(app, name string) bool {
	if name == ".DS_Store" || strings.HasPrefix(name, tempPrefix) {
		return true
	}
	return app == appCodex && strings.Contains(name, ".sqlite")
}

// codexSessionName matches a Codex session file name, which starts with the
// date the session began.
var codexSessionName = regexp.MustCompile(`^rollout-(\d{4})-(\d{2})-(\d{2})T`)

// copyPathFor maps an app-relative original path to its archive path.
// The layout mirrors the originals (decision 3), except that every Codex
// session file goes under codex/sessions/YYYY/MM/DD/ by the date in its
// name, so moving one into archived_sessions/ never makes a second copy.
func copyPathFor(app, rel string) string {
	if app == appCodex && (strings.HasPrefix(rel, "sessions/") || strings.HasPrefix(rel, "archived_sessions/")) {
		name := path.Base(rel)
		if m := codexSessionName.FindStringSubmatch(name); m != nil {
			rel = path.Join("sessions", m[1], m[2], m[3], name)
		}
	}
	p := appFolders[app] + "/" + rel
	if strings.HasSuffix(p, ".jsonl") {
		p += ".gz"
	}
	return p
}

// classify gives the kind of an app-relative path. It accepts both an
// original's path and a copy's path with the app folder and any ".gz"
// removed, so recovery can classify a copy from its archive path alone.
func classify(app, rel string) string {
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	if app == appClaudeCode {
		switch {
		case rel == "history.jsonl":
			return kindPromptLog
		case parts[0] == "file-history":
			return kindFileHistory
		case parts[0] == "paste-cache":
			return kindPaste
		case parts[0] != "projects" || len(parts) < 3:
			return kindOther
		case len(parts) == 3 && strings.HasSuffix(name, ".jsonl"):
			return kindSession
		case parts[2] == "memory":
			return kindMemory
		case len(parts) >= 5 && parts[3] == "tool-results":
			return kindToolResult
		case len(parts) == 5 && parts[3] == "subagents" && strings.HasSuffix(name, ".jsonl"):
			return kindSubagent
		case len(parts) == 5 && parts[3] == "subagents" && strings.HasSuffix(name, ".meta.json"):
			return kindSubagentMeta
		}
		return kindOther
	}
	switch {
	case (parts[0] == "sessions" || parts[0] == "archived_sessions") && strings.HasSuffix(name, ".jsonl"):
		return kindSession
	case rel == "session_index.jsonl":
		return kindSessionTitles
	case rel == "history.jsonl":
		return kindPromptLog
	case parts[0] == "attachments":
		return kindAttachment
	case parts[0] == "generated_images":
		return kindGeneratedImage
	case parts[0] == "memories":
		return kindMemory
	}
	return kindOther
}

// classifyCopyPath gives the app and kind of an archive path, or ok false
// when the path is not inside an app folder.
func classifyCopyPath(copyPath string) (app, kind string, ok bool) {
	for a, folder := range appFolders {
		if rel, found := strings.CutPrefix(copyPath, folder+"/"); found {
			rel = strings.TrimSuffix(rel, ".gz")
			return a, classify(a, rel), true
		}
	}
	return "", "", false
}
