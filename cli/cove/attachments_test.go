package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

func TestBuildUserMessageParsesInlineAttachments(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(filePath, []byte("hello attachment"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	msg, _, err := buildUserMessage("请总结 @note.txt", dir, nil, "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("buildUserMessage returned error: %v", err)
	}
	if got, want := msg.Content, "请总结"; got != want {
		t.Fatalf("Content = %q, want %q", got, want)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(msg.Parts))
	}
	if msg.Parts[0].Type != "text" {
		t.Fatalf("part type = %q, want text", msg.Parts[0].Type)
	}
	if !strings.Contains(msg.Parts[0].Text, "hello attachment") {
		t.Fatalf("text part missing file content: %q", msg.Parts[0].Text)
	}
}

func TestBuildUserMessageBuildsImagePart(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "img.png")

	// Create a valid 100x100 PNG image
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	// Fill with red
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	msg, warnings, err := buildUserMessage("看图", dir, []string{imgPath}, "deepseek-flash")
	if err != nil {
		t.Fatalf("buildUserMessage returned error: %v", err)
	}
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings for vision model: %v", warnings)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(msg.Parts))
	}
	part := msg.Parts[0]
	if part.Type != "image" {
		t.Fatalf("part type = %q, want image", part.Type)
	}
	if part.MimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png", part.MimeType)
	}
	decoded, err := base64.StdEncoding.DecodeString(part.Data)
	if err != nil {
		t.Fatalf("invalid base64: %v", err)
	}
	original, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, original) {
		t.Fatal("image attachment was re-encoded")
	}
}

func TestBuildImagePartPreservesFlashScreenshot(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2400, 100))); err != nil {
		t.Fatal(err)
	}
	part, _, warning, err := buildImagePart("screen.png", "screen.png", buf.Bytes(), "image/png", "deepseek-flash")
	if err != nil || warning != "" {
		t.Fatalf("buildImagePart: warning=%q err=%v", warning, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(part.Data)
	if err != nil || part.MimeType != "image/png" || !bytes.Equal(decoded, buf.Bytes()) {
		t.Fatalf("original screenshot not preserved: mime=%q err=%v", part.MimeType, err)
	}
}

func TestBuildImagePartRejectsInvalidImage(t *testing.T) {
	if _, _, _, err := buildImagePart("bad.png", "bad.png", []byte("not an image"), "image/png", "deepseek-flash"); err == nil {
		t.Fatal("invalid image was sent as raw data")
	}
}

func TestBuildImagePartPreservesWebP(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString("UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA==")
	if err != nil {
		t.Fatal(err)
	}
	part, _, warning, err := buildImagePart("image.webp", "image.webp", raw, "image/webp", "deepseek-flash")
	if err != nil || warning != "" {
		t.Fatalf("WebP rejected: warning=%q err=%v", warning, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(part.Data)
	if err != nil || part.MimeType != "image/webp" || !bytes.Equal(decoded, raw) {
		t.Fatalf("WebP not preserved: mime=%q err=%v", part.MimeType, err)
	}
}

func TestPreferredDeepSeekVisionModel(t *testing.T) {
	if got := preferredVisionModelForProvider("deepseek", "deepseek-v4-pro"); got != "deepseek-flash" {
		t.Fatalf("vision model = %q, want deepseek-flash", got)
	}
}

func TestBuildUserMessageWarnsNonVisionModel(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "img.png")
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("encode png: %v", err)
	}
	f.Close()

	msg, warnings, err := buildUserMessage("看图", dir, []string{imgPath}, "deepseek-reasoner")
	if err != nil {
		t.Fatalf("buildUserMessage returned error: %v", err)
	}
	if len(msg.Parts) != 1 || msg.Parts[0].Type != "image" || msg.Parts[0].Data == "" {
		t.Fatal("non-vision attachment discarded image before model routing")
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "可能不支持图片") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected warning for non-vision model, got: %v", warnings)
	}
}

// A path-like @token that is not a file used to fail the whole prompt with
// 读取附件失败 (a log line mentioning @babel/core did too); it now stays text
// and a warning says it was not attached.
func TestBuildUserMessageKeepsMissingAttachmentAsText(t *testing.T) {
	for _, in := range []string{"分析 @missing.txt", "error in @babel/core: x"} {
		msg, warnings, err := buildUserMessage(in, t.TempDir(), nil, "")
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if msg.Content != in || len(msg.Parts) != 0 {
			t.Fatalf("%q: content = %q parts = %d, want the text unchanged", in, msg.Content, len(msg.Parts))
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "已按普通文本发送") {
			t.Fatalf("%q: warnings = %q", in, warnings)
		}
	}
}

// An explicit --file / /attach path is still an error when it cannot be read.
func TestBuildUserMessageFailsForMissingExplicitPath(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := buildUserMessage("分析", dir, []string{filepath.Join(dir, "missing.txt")}, ""); err == nil {
		t.Fatal("expected an error for a missing explicit attachment")
	}
}

// Taking an @token used to rebuild the message with strings.Fields, so a
// pasted code block lost its newlines and indentation.
func TestInlineAttachmentKeepsTheRestOfTheText(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := "看 @a.txt 里的问题:\n```go\nfunc f() {\n\treturn  1\n}\n```\n@a.txt 再看一次"
	msg, _, err := buildUserMessage(in, dir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "看 里的问题:\n```go\nfunc f() {\n\treturn  1\n}\n```\n再看一次"
	if msg.Content != want {
		t.Fatalf("content = %q, want %q", msg.Content, want)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("parts = %d, want 1 (the same file twice)", len(msg.Parts))
	}
	if got, _ := extractInlineAttachments("@a.txt @a.txt 看", func(string) bool { return true }); got != "看" {
		t.Fatalf("adjacent tokens: %q", got)
	}
}

func TestSplitQuotedFieldsSupportsSpacesInPaths(t *testing.T) {
	got, err := splitQuotedFields(`"screen shot.png" logs/app.log 'notes final.txt'`)
	if err != nil {
		t.Fatalf("splitQuotedFields returned error: %v", err)
	}
	want := []string{"screen shot.png", "logs/app.log", "notes final.txt"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("fields = %#v, want %#v", got, want)
	}
}

func TestImageInputPasteOnlyConsumesImages(t *testing.T) {
	dir := t.TempDir()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 12, 8))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "screen shot.png")
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text string
		want bool
	}{
		{path, true},
		{`"` + path + `"`, true},
		{`'` + path + `' '` + path + `'`, true},
		{`"screen shot.png"`, true},
		{`"` + path + `" please analyze`, false},
		{`"` + path + `" "missing.png"`, false},
		{"```\n" + path + "\n```", false},
		{"", false},
	} {
		var paths []string
		input := newImageInput(&paths)
		if got := input.paste(tc.text, dir); got != tc.want {
			t.Fatalf("paste %q = %v, want %v", tc.text, got, tc.want)
		}
		if tc.want && (len(paths) != 1 || paths[0] != path || !strings.Contains(input.summary(), "12x8")) {
			t.Fatalf("paths=%v summary=%q", paths, input.summary())
		}
		if !tc.want && len(paths) != 0 {
			t.Fatal("rejected paste partially attached files")
		}
	}
}

func TestImageInputClipboardValidationAndCleanup(t *testing.T) {
	var paths []string
	input := newImageInput(&paths)
	t.Cleanup(input.close)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 12, 8))); err != nil {
		t.Fatal(err)
	}
	input.clipboard = func(context.Context) ([]byte, error) { return data.Bytes(), nil }
	for repeat := 0; repeat < 2; repeat++ {
		if err := input.pasteClipboard(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(paths) != 1 {
		t.Fatalf("clipboard duplicate paths=%v", paths)
	}
	path := paths[0]
	msg, _, err := buildUserMessage("look", "", paths, "deepseek-flash")
	if err != nil || len(msg.Parts) != 1 || msg.Parts[0].Type != "image" {
		t.Fatalf("clipboard message parts=%d err=%v", len(msg.Parts), err)
	}
	input.clipboard = func(context.Context) ([]byte, error) { return []byte("invalid"), nil }
	if err := input.pasteClipboard(context.Background()); err == nil || len(paths) != 1 {
		t.Fatal("invalid clipboard changed pending attachments")
	}
	input.prune()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("pending clipboard image was deleted")
	}
	paths = nil
	input.prune()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unused clipboard image remains: %v", err)
	}
}

func BenchmarkImageInputClipboardReuse(b *testing.B) {
	var paths []string
	input := newImageInput(&paths)
	b.Cleanup(input.close)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1024, 768))); err != nil {
		b.Fatal(err)
	}
	input.clipboard = func(context.Context) ([]byte, error) { return data.Bytes(), nil }
	ctx := context.Background()
	if err := input.pasteClipboard(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if err := input.pasteClipboard(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func TestImageInputClipboardRejectsUnsafeData(t *testing.T) {
	var pngData, jpegData bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 12, 8))
	if err := png.Encode(&pngData, picture); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, picture, nil); err != nil {
		t.Fatal(err)
	}
	unsafeData := append([]byte(nil), pngData.Bytes()...)
	binary.BigEndian.PutUint32(unsafeData[16:20], 65536)
	binary.BigEndian.PutUint32(unsafeData[20:24], 65536)
	binary.BigEndian.PutUint32(unsafeData[29:33], crc32.ChecksumIEEE(unsafeData[12:29]))
	for _, raw := range [][]byte{jpegData.Bytes(), unsafeData} {
		var paths []string
		input := newImageInput(&paths)
		input.clipboard = func(context.Context) ([]byte, error) { return raw, nil }
		if err := input.pasteClipboard(context.Background()); err == nil || len(paths) != 0 || input.tempDir != "" {
			t.Fatalf("unsafe clipboard accepted: err=%v attachments=%d", err, len(paths))
		}
	}
	var paths []string
	input := newImageInput(&paths)
	t.Cleanup(input.close)
	input.clipboard = func(context.Context) ([]byte, error) { return pngData.Bytes(), nil }
	if err := input.pasteClipboard(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	if err := input.pasteClipboard(context.Background()); err == nil || len(paths) != 1 {
		t.Fatalf("missing cached file accepted: err=%v attachments=%d", err, len(paths))
	}
}

func TestImageInputCanceledClipboardDoesNotRead(t *testing.T) {
	var paths []string
	input := newImageInput(&paths)
	called := false
	input.clipboard = func(context.Context) ([]byte, error) { called = true; return nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := input.pasteClipboard(ctx); !errors.Is(err, context.Canceled) || called || len(paths) != 0 || input.tempDir != "" {
		t.Fatalf("err=%v called=%v paths=%v temp=%q", err, called, paths, input.tempDir)
	}
}

func TestImageInputSubmitReportsQueueRejection(t *testing.T) {
	t.Cleanup(func() { repl.SetQueuedCount(0) })
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newREPLTaskRunner(nil)
	runner.queueStore = session.NewQueueStore(filepath.Join(blocked, "queues"))
	runner.queueSession = "previous"
	runner.queueID = "test"
	runner.queue = []api.Message{{Role: "user", Content: "earlier"}}
	runner.paused = true
	feedback, accepted := runner.submitWithFeedback(api.Message{Role: "user", Content: "image"})
	if accepted || !strings.Contains(feedback, "未接收") || len(runner.queue) != 1 {
		t.Fatalf("accepted=%v feedback=%q queue=%d", accepted, feedback, len(runner.queue))
	}
	runner.queueStore = nil
	runner.persistenceError = ""
	feedback, accepted = runner.submitWithFeedback(api.Message{Role: "user", Content: "image"})
	if !accepted || !strings.Contains(feedback, "已排队") || len(runner.queue) != 2 {
		t.Fatalf("accepted=%v feedback=%q queue=%d", accepted, feedback, len(runner.queue))
	}
}

func TestAddAttachmentsNormalizesAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	var attached []string
	addAttachments([]string{"note.txt", "@note.txt"}, dir, &attached)
	if len(attached) != 1 {
		t.Fatalf("attached = %#v, want one normalized path", attached)
	}
	if attached[0] != filePath {
		t.Fatalf("attached path = %q, want %q", attached[0], filePath)
	}
}

func TestProcessImageResizeAndCompress(t *testing.T) {
	// Create a large 2048x2048 PNG (should be resized to 1568x1568)
	img := image.NewRGBA(image.Rect(0, 0, 2048, 2048))
	for y := 0; y < 2048; y++ {
		for x := 0; x < 2048; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: 255})
		}
	}
	// Encode as PNG then process
	f, err := os.CreateTemp(t.TempDir(), "large-*.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}

	// Process
	processed, mime, err := processImage(raw)
	if err != nil {
		t.Fatalf("processImage failed: %v", err)
	}
	if mime != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %s", mime)
	}
	if len(processed) > maxImageBytes {
		t.Fatalf("processed image too large: %d bytes > %d", len(processed), maxImageBytes)
	}
	// Decode result to verify dimensions
	decoded, _, err := decodeAndCheck(processed)
	if err != nil {
		t.Fatalf("failed to decode processed image: %v", err)
	}
	bounds := decoded.Bounds()
	if bounds.Dx() > maxImageDim || bounds.Dy() > maxImageDim {
		t.Fatalf("processed image dimensions %dx%d exceed max %d", bounds.Dx(), bounds.Dy(), maxImageDim)
	}
	t.Logf("original=%d bytes -> processed=%d bytes (%dx%d)", len(raw), len(processed), bounds.Dx(), bounds.Dy())
}

func decodeAndCheck(data []byte) (image.Image, string, error) {
	return image.Decode(bytes.NewReader(data))
}

func TestDecodeImageFormats(t *testing.T) {
	dir := t.TempDir()

	// PNG
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	pngPath := filepath.Join(dir, "test.png")
	f, _ := os.Create(pngPath)
	png.Encode(f, img)
	f.Close()
	raw, _ := os.ReadFile(pngPath)
	if _, err := decodeImage(raw); err != nil {
		t.Errorf("failed to decode PNG: %v", err)
	}

	// Unsupported format
	if _, err := decodeImage([]byte("not an image")); err == nil {
		t.Error("expected error for invalid image data")
	}
}

func TestStripAlpha(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	result := stripAlpha(img)
	if result == nil {
		t.Fatal("stripAlpha returned nil")
	}
	if result.Bounds().Dx() != 10 || result.Bounds().Dy() != 10 {
		t.Error("stripAlpha changed dimensions")
	}
}

func TestDetectMimeType(t *testing.T) {
	// PNG magic bytes
	pngSig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	mime := detectMimeType("screenshot.png", pngSig)
	if !strings.HasPrefix(mime, "image/png") {
		t.Errorf("expected image/png, got %s", mime)
	}

	// Extension-based
	mime = detectMimeType("document.pdf", []byte("%PDF"))
	if !strings.Contains(mime, "pdf") {
		t.Errorf("expected pdf mime, got %s", mime)
	}
}

func TestResizeImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4000, 2000))
	resized := resizeImage(img, 4000, 2000, 1568)
	bounds := resized.Bounds()
	if bounds.Dx() != 1568 || bounds.Dy() != 784 {
		t.Errorf("expected 1568x784, got %dx%d", bounds.Dx(), bounds.Dy())
	}

	// Tall image
	img2 := image.NewRGBA(image.Rect(0, 0, 100, 3000))
	resized2 := resizeImage(img2, 100, 3000, 1568)
	bounds2 := resized2.Bounds()
	if bounds2.Dx() != 52 || bounds2.Dy() != 1568 {
		t.Errorf("expected 52x1568, got %dx%d", bounds2.Dx(), bounds2.Dy())
	}
}

// TestBuildTextPartKeepsValidUTF8 is the regression test for truncating an
// attached text file on a byte boundary: the data is validated as UTF-8 and
// then a raw body[:limit] cut a multi-byte rune in half, shipping invalid
// UTF-8 to the provider.
func TestBuildTextPartKeepsValidUTF8(t *testing.T) {
	// Well over the 200KB text limit, all multi-byte runes, and sized so the
	// limit lands mid-rune (200*1024 is not a multiple of 3).
	data := []byte(strings.Repeat("配", 120*1024))
	if !utf8.Valid(data) {
		t.Fatal("fixture is not valid UTF-8")
	}

	part, _, _, err := buildTextPart("big.md", "/tmp/big.md", data, "text/markdown")
	if err != nil {
		t.Fatalf("buildTextPart: %v", err)
	}
	if !utf8.ValidString(part.Text) {
		t.Fatal("attachment text is not valid UTF-8 after truncation")
	}
	if !strings.Contains(part.Text, "内容已截断") {
		t.Fatal("truncation notice missing")
	}
}

// TestDecodeImageRejectsDeclaredBomb covers the header check added so a small
// file declaring enormous dimensions is refused instead of driving the
// decoder's up-front width*height allocation.
func TestDecodeImageRejectsDeclaredBomb(t *testing.T) {
	// Build a real PNG, then rewrite IHDR to declare 60000x60000 (3.6e9 pixels)
	// and fix the chunk CRC so the header parses. The pixel data stays tiny —
	// which is exactly the shape of a decompression bomb.
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	raw := buf.Bytes()

	// Layout: 8-byte signature, then IHDR (4 len + 4 type + 13 data + 4 CRC).
	const ihdrType = 8 + 4 // offset of the "IHDR" type field
	const ihdrData = ihdrType + 4
	binary.BigEndian.PutUint32(raw[ihdrData+0:], 60000) // width
	binary.BigEndian.PutUint32(raw[ihdrData+4:], 60000) // height
	crc := crc32.ChecksumIEEE(raw[ihdrType : ihdrData+13])
	binary.BigEndian.PutUint32(raw[ihdrData+13:], crc)

	_, err := decodeImage(raw)
	if err == nil {
		t.Fatal("decodeImage accepted a 60000x60000 declaration")
	}
	if !strings.Contains(err.Error(), "图片过大") {
		t.Fatalf("expected a size refusal, got: %v", err)
	}
	if _, _, _, err := buildImagePart("bomb.png", "bomb.png", raw, "image/png", "deepseek-flash"); err == nil {
		t.Fatal("attachment bypassed the dimension refusal")
	}
}

// TestDecodeImageAcceptsSmallPNG guards against the header check rejecting
// ordinary images.
func TestDecodeImageAcceptsSmallPNG(t *testing.T) {
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	got, err := decodeImage(buf.Bytes())
	if err != nil {
		t.Fatalf("decodeImage rejected a valid 8x8 PNG: %v", err)
	}
	if got.Bounds().Dx() != 8 || got.Bounds().Dy() != 8 {
		t.Fatalf("decoded bounds = %v, want 8x8", got.Bounds())
	}
}
