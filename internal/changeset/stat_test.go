package changeset

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/workspace"
)

func statByPath(stat Stat) map[string]FileStat {
	files := make(map[string]FileStat, len(stat.Files))
	for _, file := range stat.Files {
		files[file.Path] = file
	}
	return files
}

func TestSummarizeTextAndBinary(t *testing.T) {
	root := fixture(t)
	baseline, err := workspace.Capture(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	patch := candidatePatch(t, baseline, map[string][]byte{
		"value.txt": []byte("good\n"),
		"new.txt":   []byte("one\ntwo\n"),
		"blob.bin":  {0, 1, 2, 0, 255, 0, 'x'},
	})
	if !strings.Contains(string(patch), "GIT binary patch") {
		t.Fatalf("fixture patch is not binary:\n%s", patch)
	}
	stat, err := Summarize(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(stat.Files) != 3 {
		t.Fatalf("files = %+v", stat.Files)
	}
	files := statByPath(stat)
	want := map[string]FileStat{
		"value.txt": {Path: "value.txt", Added: 1, Removed: 1},
		"new.txt":   {Path: "new.txt", Added: 2},
		"blob.bin":  {Path: "blob.bin", Binary: true},
	}
	for path, expected := range want {
		if files[path] != expected {
			t.Fatalf("%s = %+v, want %+v", path, files[path], expected)
		}
	}
	if stat.Added != 3 || stat.Removed != 1 {
		t.Fatalf("totals = +%d -%d", stat.Added, stat.Removed)
	}
}

func TestSummarizeHandlesNoNewlineAndBinary(t *testing.T) {
	root := fixture(t)
	baseline, err := workspace.Capture(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	patch := candidatePatch(t, baseline, map[string][]byte{
		"value.txt": []byte("good"),
		"blob.bin":  {0, 0, 0, 1},
	})
	if !strings.Contains(string(patch), `\ No newline at end of file`) {
		t.Fatalf("fixture patch lacks the no-newline marker:\n%s", patch)
	}
	stat, err := Summarize(patch)
	if err != nil {
		t.Fatal(err)
	}
	files := statByPath(stat)
	if got := files["value.txt"]; got != (FileStat{Path: "value.txt", Added: 1, Removed: 1}) {
		t.Fatalf("value.txt = %+v", got)
	}
	if got := files["blob.bin"]; !got.Binary || got.Added != 0 || got.Removed != 0 {
		t.Fatalf("blob.bin = %+v", got)
	}
	if len(stat.Files) != 2 || stat.Added != 1 || stat.Removed != 1 {
		t.Fatalf("stat = %+v", stat)
	}
}

func TestSummarizeCountsContentThatLooksLikeHeaders(t *testing.T) {
	patch := "diff --git a/x.txt b/x.txt\n" +
		"--- a/x.txt\n+++ b/x.txt\n" +
		"@@ -1,2 +1,2 @@\n" +
		"---old\n" + // removed line whose content is "--old"
		"+++new\n" + // added line whose content is "++new"
		" keep\n"
	stat, err := Summarize([]byte(patch))
	if err != nil {
		t.Fatal(err)
	}
	if len(stat.Files) != 1 || stat.Files[0] != (FileStat{Path: "x.txt", Added: 1, Removed: 1}) {
		t.Fatalf("stat = %+v", stat)
	}
}

func TestSummarizeUnquotesGitPaths(t *testing.T) {
	root := fixture(t)
	baseline, err := workspace.Capture(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"文档.txt": []byte("中文\n"), "plain name.txt": []byte("x\n")}
	quoted := `say "hi".txt`
	if runtime.GOOS != "windows" { // `"` is not a valid Windows file name character
		files[quoted] = []byte("q\n")
	}
	patch := candidatePatch(t, baseline, files)
	if !strings.Contains(string(patch), `"a/\346\226\207\346\241\243.txt"`) {
		t.Fatalf("fixture patch does not quote the CJK name:\n%s", patch)
	}
	stat, err := Summarize(patch)
	if err != nil {
		t.Fatal(err)
	}
	got := statByPath(stat)
	if len(stat.Files) != len(files) {
		t.Fatalf("files = %+v", stat.Files)
	}
	for name := range files {
		if got[name] != (FileStat{Path: name, Added: 1}) {
			t.Fatalf("%q = %+v (all: %+v)", name, got[name], stat.Files)
		}
	}
	// Synthetic mixed header: only one side quoted.
	mixed := "diff --git a/x.txt \"b/\\346\\226\\207.txt\"\n@@ -1 +1 @@\n-a\n+b\n"
	stat, err = Summarize([]byte(mixed))
	if err != nil || len(stat.Files) != 1 || stat.Files[0].Path != "文.txt" {
		t.Fatalf("mixed header: %+v %v", stat, err)
	}
}

func TestSummarizeRejectsGarbage(t *testing.T) {
	if _, err := Summarize([]byte("not a patch")); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := Summarize(nil); err == nil {
		t.Fatal("empty patch accepted")
	}
}
