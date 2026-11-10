package diffs

import (
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// parseDiff splits git unified diff output into files, deriving status and +/- counts from the text so the
// same parser serves `git diff` on the mirror and GitHub's diff media type.
func parseDiff(text string) []proto.DiffFile {
	var files []proto.DiffFile
	var cur *proto.DiffFile
	var buf []string
	inHunk := false
	flush := func() {
		if cur != nil {
			cur.Patch = strings.Join(buf, "\n")
			files = append(files, *cur)
		}
		cur, buf, inHunk = nil, nil, false
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			a, b := splitHeader(strings.TrimPrefix(line, "diff --git "))
			cur = &proto.DiffFile{Path: b, Status: "modified"}
			if a != b {
				cur.OldPath = a
			}
			buf = []string{line}
			continue
		}
		if cur == nil {
			continue
		}
		buf = append(buf, line)
		if inHunk {
			switch {
			case strings.HasPrefix(line, "+"):
				cur.Additions++
			case strings.HasPrefix(line, "-"):
				cur.Deletions++
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "new file mode"):
			cur.Status = "added"
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Status = "deleted"
		case strings.HasPrefix(line, "rename from "):
			cur.Status, cur.OldPath = "renamed", strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to "):
			cur.Path = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "Binary files "), line == "GIT binary patch":
			cur.Binary = true
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		}
	}
	flush()
	for i := range files {
		if files[i].Status != "renamed" {
			files[i].OldPath = ""
		}
		files[i].Patch = strings.TrimRight(files[i].Patch, "\n")
	}
	return files
}

// splitHeader splits "a/x b/y" into x and y. Paths with spaces are ambiguous in this header; the
// rename/---/+++ lines refine them, so for the plain case split at the midpoint.
func splitHeader(s string) (string, string) {
	if i := len(s) / 2; len(s)%2 == 1 && s[i] == ' ' && strings.TrimPrefix(s[:i], "a/") == strings.TrimPrefix(s[i+1:], "b/") {
		return strings.TrimPrefix(s[:i], "a/"), strings.TrimPrefix(s[i+1:], "b/")
	}
	if i := strings.Index(s, " b/"); i >= 0 {
		return strings.TrimPrefix(s[:i], "a/"), s[i+3:]
	}
	return s, s
}
