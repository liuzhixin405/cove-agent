package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An .svg has an image/* MIME type by extension but is not a raster image;
// it used to be refused as "unsupported image format" and the whole message
// dropped. It is text, and goes as a text attachment.
func TestSVGAttachmentIsSentAsText(t *testing.T) {
	dir := t.TempDir()
	svg := `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`
	if err := os.WriteFile(filepath.Join(dir, "icon.svg"), []byte(svg), 0o644); err != nil {
		t.Fatal(err)
	}
	part, _, _, err := buildAttachmentPart(dir, "icon.svg", "")
	if err != nil {
		t.Fatalf("svg attachment refused: %v", err)
	}
	if part.Type == "image" || (!strings.Contains(part.Text, "<svg") && part.Data == "") {
		t.Fatalf("svg part = %+v, want a text/file part, not a vision part", part)
	}
}
