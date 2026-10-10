package automation

import (
	"os"
	"path/filepath"
	"testing"
)

// StatePath names the file Open would use, and creates nothing.
func TestStatePathMatchesOpenWithoutCreating(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	project := t.TempDir()
	path, err := StatePath(root, project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("StatePath must not create the state directory")
	}
	store, err := Open(root, project)
	if err != nil {
		t.Fatal(err)
	}
	if store.Path() != path {
		t.Fatalf("StatePath %q != Open().Path() %q", path, store.Path())
	}
}
