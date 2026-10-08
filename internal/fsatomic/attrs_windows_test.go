//go:build windows

package fsatomic

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func fileAttrs(t *testing.T, path string) uint32 {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := windows.GetFileAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func setFileAttrs(t *testing.T, path string, attrs uint32) {
	t.Helper()
	p, _ := windows.UTF16PtrFromString(path)
	if err := windows.SetFileAttributes(p, attrs); err != nil {
		t.Fatal(err)
	}
}

// Renaming a new file into place used to drop the replaced file's
// attributes: a hidden .env came back visible. Its alternate data streams
// (Zone.Identifier, tool metadata) were lost too.
func TestWriteFileKeepsWindowsAttributesAndStreams(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+":meta", []byte("kept"), 0o644); err != nil {
		t.Skipf("filesystem without alternate data streams: %v", err)
	}
	setFileAttrs(t, path, windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_ARCHIVE)

	if err := WriteFile(path, []byte("A=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "A=2\n" {
		t.Fatalf("content = %q", b)
	}
	if a := fileAttrs(t, path); a&windows.FILE_ATTRIBUTE_HIDDEN == 0 {
		t.Errorf("hidden attribute lost: attrs = %#x", a)
	}
	if b, err := os.ReadFile(path + ":meta"); err != nil || string(b) != "kept" {
		t.Errorf("alternate data stream lost: %q, %v", b, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestWriteFileRootKeepsWindowsAttributesAndStreams(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private.env")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+":meta", []byte("kept"), 0600); err != nil {
		t.Skipf("filesystem without alternate data streams: %v", err)
	}
	setFileAttrs(t, path, windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_ARCHIVE)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := WriteFileRoot(root, "private.env", []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "after" {
		t.Fatalf("body = %q", data)
	}
	if fileAttrs(t, path)&windows.FILE_ATTRIBUTE_HIDDEN == 0 {
		t.Fatal("hidden attribute lost")
	}
	if data, err := os.ReadFile(path + ":meta"); err != nil || string(data) != "kept" {
		t.Fatalf("alternate stream lost: %q, %v", data, err)
	}
}

// The fallback when ReplaceFileW cannot be used restores the attributes too.
func TestRenameKeepingAttributesFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.ini")
	tmp := filepath.Join(dir, "new.tmp")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	setFileAttrs(t, path, windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_SYSTEM)
	if err := renameKeepingAttributes(tmp, path, fileAttrs(t, path)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("content = %q", b)
	}
	if a := fileAttrs(t, path); a&(windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_SYSTEM) != windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_SYSTEM {
		t.Errorf("attributes lost: %#x", a)
	}
}
