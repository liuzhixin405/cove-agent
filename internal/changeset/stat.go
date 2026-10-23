package changeset

import (
	"errors"
	"strconv"
	"strings"
)

// FileStat is the line count of one file in a patch.
type FileStat struct {
	Path           string
	Added, Removed int
	Binary         bool
}

// Stat is the line count of a whole patch.
type Stat struct {
	Files          []FileStat
	Added, Removed int
}

// Summarize parses a `git diff --binary` patch without a work tree.
//
// It reads `diff --git a/X b/X` headers, `Binary files`/`GIT binary patch`
// markers, and `+`/`-` lines inside hunks. Lines before a file's first `@@`
// are header lines (`---`/`+++` included) and are not counted; inside a hunk
// every `+`/`-` line counts, even one whose content starts with `++`/`--`.
// `\ No newline at end of file` is not counted.
func Summarize(patch []byte) (Stat, error) {
	var stat Stat
	current := -1
	inHunk := false
	for _, line := range strings.Split(string(patch), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if rest, ok := strings.CutPrefix(line, "diff --git "); ok {
			stat.Files = append(stat.Files, FileStat{Path: diffPath(rest)})
			current, inHunk = len(stat.Files)-1, false
			continue
		}
		if current < 0 {
			continue
		}
		file := &stat.Files[current]
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk && (line == "GIT binary patch" || strings.HasPrefix(line, "Binary files ")):
			file.Binary = true
		case inHunk && strings.HasPrefix(line, "+"):
			file.Added++
			stat.Added++
		case inHunk && strings.HasPrefix(line, "-"):
			file.Removed++
			stat.Removed++
		}
	}
	if current < 0 {
		return Stat{}, errors.New("not a git patch: no `diff --git` header")
	}
	return stat, nil
}

// diffPath extracts X from "a/X b/X". For identical a/b paths (the common
// case, and the only one that is unambiguous when X contains " b/") the two
// halves are split by length; otherwise the last " b/" separates them.
//
// With git's default core.quotePath, names with non-ASCII bytes, `"` or
// control characters are C-quoted (`"a/\346\226\207.txt" "b/..."`); those
// tokens are unquoted and the b-side path is returned.
func diffPath(rest string) string {
	if strings.HasPrefix(rest, `"`) || strings.HasSuffix(rest, `"`) {
		if path, ok := quotedDiffPath(rest); ok {
			return path
		}
	}
	if n := len(rest); n%2 == 1 && strings.HasPrefix(rest, "a/") {
		half := (n - 5) / 2
		if half > 0 && rest[2+half:2+half+3] == " b/" && rest[2:2+half] == rest[n-half:] {
			return rest[2 : 2+half]
		}
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return rest
}

// quotedDiffPath parses `A B` where either token may be C-quoted and returns
// B without its `b/` prefix.
func quotedDiffPath(rest string) (string, bool) {
	var second string
	if strings.HasPrefix(rest, `"`) {
		end := quotedEnd(rest)
		if end < 0 || end+1 >= len(rest) || rest[end+1] != ' ' {
			return "", false
		}
		second = rest[end+2:]
	} else {
		i := strings.Index(rest, ` "`)
		if i < 0 {
			return "", false
		}
		second = rest[i+1:]
	}
	if strings.HasPrefix(second, `"`) {
		unquoted, err := strconv.Unquote(second)
		if err != nil {
			return "", false
		}
		second = unquoted
	}
	path, ok := strings.CutPrefix(second, "b/")
	return path, ok && path != ""
}

// quotedEnd returns the index of the closing quote of the C-quoted token at
// the start of s, or -1.
func quotedEnd(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}
