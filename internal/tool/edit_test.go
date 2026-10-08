package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempFile(t *testing.T, content string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "sample.go")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	return dir, path
}

func mustCall(t *testing.T, dir, oldS, newS string, all bool) Result {
	t.Helper()
	et := NewEditTool()
	res, err := et.Call(context.Background(), Input{
		"filePath":   "sample.go",
		"oldString":  oldS,
		"newString":  newS,
		"replaceAll": all,
	}, Context{Cwd: dir})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	return res
}

func TestEditTool_ExactMatchUnchanged(t *testing.T) {
	dir, path := writeTempFile(t, "package main\n\nfunc main() {}\n")
	res := mustCall(t, dir, "func main() {}", "func main() { println(\"hi\") }", false)
	if res.IsError {
		t.Fatalf("expected exact match to succeed, got error: %s", res.Data)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "println") {
		t.Fatalf("expected replacement applied, got: %s", got)
	}
}

func TestCancelledFileToolsLeaveContentUnchanged(t *testing.T) {
	for _, fileTool := range []Tool{NewWriteTool(), NewEditTool()} {
		t.Run(fileTool.Def().Name, func(t *testing.T) {
			dir, path := writeTempFile(t, "original\n")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			result, err := fileTool.Call(ctx, Input{
				"filePath": path, "content": "replacement\n", "oldString": "original", "newString": "replacement",
			}, Context{Cwd: dir})
			if err != nil || !result.IsError || result.Data != "Error: context canceled" {
				t.Fatalf("cancellation result = %+v, %v", result, err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "original\n" {
				t.Fatalf("cancelled tool changed file: data=%q err=%v", data, err)
			}
		})
	}
}

func TestEditTool_FuzzySingleLineWhitespace(t *testing.T) {
	// Real file has irregular internal spacing; model's oldString is
	// normalized/tidy. Exact match fails, normalized match should succeed
	// unambiguously and auto-apply.
	dir, path := writeTempFile(t, "package main\n\nfunc  main()   {\n\tprintln(\"a\")\n}\n")
	res := mustCall(t, dir, "func main() {", "func main() { // entry", false)
	if res.IsError {
		t.Fatalf("expected fuzzy single-line match to succeed, got error: %s", res.Data)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "func main() { // entry") {
		t.Fatalf("expected fuzzy replacement applied, got: %s", got)
	}
	if strings.Contains(string(got), "func  main()") {
		t.Fatalf("original mismatched line should have been replaced, got: %s", got)
	}
}

func TestEditTool_FuzzyMultiLineBlock(t *testing.T) {
	original := "package main\n\nfunc add(a int, b int) int {\n    return a + b\n}\n"
	dir, path := writeTempFile(t, original)
	// Model's oldString has different indentation width (spaces collapsed
	// differently) but the same tokens per line.
	oldS := "func add(a int, b int) int {\n  return a + b\n}"
	newS := "func add(a int, b int) int {\n\treturn a + b + 1\n}"
	res := mustCall(t, dir, oldS, newS, false)
	if res.IsError {
		t.Fatalf("expected fuzzy multi-line match to succeed, got error: %s", res.Data)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "a + b + 1") {
		t.Fatalf("expected multi-line replacement applied, got: %s", got)
	}
	// The line after the matched block must survive untouched.
	if !strings.HasSuffix(string(got), "}\n") {
		t.Fatalf("expected trailing newline preserved, got: %q", got)
	}
}

func TestEditTool_FuzzyAmbiguousNormalizedMatch(t *testing.T) {
	// Neither line is an exact substring match for oldString (so the exact
	// path falls through with count==0), but both normalize to the same
	// text — must NOT auto-apply, and the error must mention the ambiguity
	// so the model can disambiguate with more context instead of guessing.
	dir, _ := writeTempFile(t, "x  := 1\ny := 2\nx :=  1\n")
	res := mustCall(t, dir, "x := 1", "x := 99", false)
	if !res.IsError {
		t.Fatalf("expected ambiguous normalized match to be rejected, got success: %s", res.Data)
	}
	if !strings.Contains(res.Data, "ambiguous") {
		t.Fatalf("expected ambiguity to be reported, got: %s", res.Data)
	}
}

func TestEditTool_NoMatchNoHintOnUnrelatedFile(t *testing.T) {
	dir, _ := writeTempFile(t, "package main\n\nfunc main() {}\n")
	res := mustCall(t, dir, "totally unrelated text that appears nowhere near this file content", "x", false)
	if !res.IsError {
		t.Fatalf("expected no-match error")
	}
	if !strings.Contains(res.Data, "not found") {
		t.Fatalf("expected base not-found message, got: %s", res.Data)
	}
}

func TestEditTool_ExactMultipleMatchesStillRequiresReplaceAll(t *testing.T) {
	dir, _ := writeTempFile(t, "foo\nfoo\nfoo\n")
	res := mustCall(t, dir, "foo", "bar", false)
	if !res.IsError || !strings.Contains(res.Data, "matches 3 times") {
		t.Fatalf("expected exact-match ambiguity error unchanged, got: %s", res.Data)
	}
}
