package tool

import (
	"context"
	"encoding/json"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type GlobTool struct{ baseTool }

func NewGlobTool() Tool {
	return &GlobTool{baseTool{def: Def{
		Name: "glob", Description: "Find files matching glob patterns. Support ** for recursive matching.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"pattern":{"type":"string","description":"Glob pattern (e.g. **/*.go, src/**/*.ts)"},
				"path":{"type":"string","description":"Directory to search in (defaults to cwd)"}
			},
			"required":["pattern"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Glob",
	}}}
}

// matchGlob reports whether relPath matches pattern, with `**` crossing
// directory separators.
//
// filepath.Match alone cannot do this: its `*` never crosses a separator and it
// has no notion of `**` at all, so the documented `src/**/*.ts` form silently
// matched nothing. A leading `**/` was special-cased against the basename,
// which is why only that one form appeared to work.
//
// Semantics, matching the common doublestar convention:
//   - `**` matches zero or more path segments, so `src/**/*.ts` matches both
//     `src/a.ts` and `src/a/b/c.ts`.
//   - a bare `*.go` (no separator in the pattern) matches on the basename, so
//     the shorthand people actually type keeps working.
//   - anything else is matched segment-by-segment via filepath.Match, so `?`,
//     `*` and character classes behave as before within a segment.
func matchGlob(pattern, relPath string) bool {
	pattern = filepath.ToSlash(pattern)
	relPath = filepath.ToSlash(relPath)

	if !strings.Contains(pattern, "/") {
		ok, _ := filepath.Match(pattern, path.Base(relPath))
		return ok
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(relPath, "/"))
}

// matchSegments matches pattern segments against path segments, treating "**"
// as "zero or more segments".
func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Trailing "**" swallows whatever is left, including nothing.
			if len(pat) == 1 {
				return true
			}
			// Try consuming 0, 1, 2 … segments here.
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := filepath.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

func (t *GlobTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	pattern, _ := input["pattern"].(string)
	basePath, _ := input["path"].(string)
	if basePath == "" {
		basePath = tctx.Cwd
	}
	if basePath == "" {
		basePath = "."
	}

	// Same boundary as the read tool: listing files outside the project is a
	// way to read it too.
	basePath, err := resolvePathInCwd(basePath, tctx, false)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	// projectFiles honors .gitignore inside a repository and, unlike the old
	// walk, does not hide every dot-directory — .github/workflows is exactly
	// what "find the CI config" is looking for.
	files, err := projectFiles(ctx, basePath)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	var picked []string
	for _, rel := range files {
		if matchGlob(pattern, rel) {
			picked = append(picked, rel)
		}
	}
	// Links out of the working directory are left out, as grep leaves them
	// out: the file tools refuse them, so listing them only invites a read
	// that fails (or a workaround through the shell).
	// Only the entries shown are checked (one Lstat each): checking every
	// match first cost seconds in a large repository on Windows. Entries past
	// the cap are counted unchecked, so the overflow count is an upper bound.
	const limit = 200
	confine := newFileConfiner(tctx.Cwd, basePath)
	// Results are shown relative to the working directory, as grep shows
	// them, so `glob path=src` returns src/pkg/a.go that read/edit accept;
	// relative to `path` they were "pkg/a.go", which read reported missing.
	prefix := ""
	if tctx.Cwd != "" {
		if rel, err := filepath.Rel(tctx.Cwd, basePath); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			prefix = rel
		} else if err != nil || rel != "." {
			prefix = basePath
		}
	}
	var matches []string
	rest := 0
	for i, rel := range picked {
		if len(matches) == limit {
			rest = len(picked) - i
			break
		}
		if confine.allow(rel) {
			matches = append(matches, filepath.Join(prefix, filepath.FromSlash(rel)))
		}
	}

	if len(matches) == 0 {
		return Result{Data: "No files found for: " + pattern}, nil
	}
	if rest > 0 {
		return Result{Data: strings.Join(matches, "\n") + "\n... and up to " + strconv.Itoa(rest) + " more files"}, nil
	}
	return Result{Data: strings.Join(matches, "\n")}, nil
}

func (t *GlobTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("glob is read-only")
}
