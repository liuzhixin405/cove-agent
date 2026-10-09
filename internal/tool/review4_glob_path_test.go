package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// glob with a path used to return matches relative to that path ("pkg/a.go"
// for path=src), which read/edit then could not find; grep returns them
// relative to the working directory, and so does glob now.
func TestGlobWithPathReturnsPathsRelativeToCwd(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "pkg", "a.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewGlobTool().Call(context.Background(), Input{"pattern": "**/*.go", "path": "src"}, Context{Cwd: dir})
	if err != nil || res.IsError {
		t.Fatalf("glob: %v %s", err, res.Data)
	}
	want := filepath.Join("src", "pkg", "a.go")
	if !strings.Contains(res.Data, want) {
		t.Fatalf("glob = %q, want a line %q", res.Data, want)
	}
	read, err := NewReadTool().Call(context.Background(), Input{"filePath": want}, Context{Cwd: dir})
	if err != nil || read.IsError {
		t.Fatalf("read of a glob result failed: %v %s", err, read.Data)
	}
}
