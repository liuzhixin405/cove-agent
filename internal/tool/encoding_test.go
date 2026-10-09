package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestReadReturnsNativeImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "screen.png")
	var original bytes.Buffer
	if err := png.Encode(&original, image.NewRGBA(image.Rect(0, 0, 16, 8))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := NewReadTool().Call(context.Background(), Input{"filePath": path}, Context{Cwd: dir})
	if err != nil || result.IsError {
		t.Fatalf("image read failed: result=%+v err=%v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Parts []api.MessagePart `json:"parts"`
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Parts) != 1 || payload.Parts[0].Type != "image" || payload.Parts[0].MimeType != "image/png" || payload.Parts[0].FileName != "screen.png" {
		t.Fatalf("image block missing: %+v", payload.Parts)
	}
	decoded, err := base64.StdEncoding.DecodeString(payload.Parts[0].Data)
	if err != nil || !bytes.Equal(decoded, original.Bytes()) {
		t.Fatalf("image pixels changed: %v", err)
	}
	if strings.Contains(result.Data, payload.Parts[0].Data) {
		t.Fatal("image encoded into plain text")
	}
}

func TestReadImageLimits(t *testing.T) {
	for _, scenario := range []string{"bytes", "dimensions"} {
		t.Run(scenario, func(t *testing.T) {
			width := 16
			if scenario == "dimensions" {
				width = 4097
			}
			var raw bytes.Buffer
			if err := png.Encode(&raw, image.NewRGBA(image.Rect(0, 0, width, 8))); err != nil {
				t.Fatal(err)
			}
			data := raw.Bytes()
			if scenario == "bytes" {
				data = append(data, make([]byte, 5*1024*1024+1-len(data))...)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "large.png")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := NewReadTool().Call(context.Background(), Input{"filePath": path}, Context{Cwd: dir})
			if err != nil || !result.IsError || len(result.Parts) != 0 {
				t.Fatalf("oversized image was not refused: result=%+v err=%v", result, err)
			}
			limit := "5 MiB"
			if scenario == "dimensions" {
				limit = "4096-pixel"
			}
			if !strings.Contains(result.Data, limit) {
				t.Fatalf("missing limit explanation: %q", result.Data)
			}
		})
	}
}

func TestReadRefusesBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logo.png")
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{0, 1, 2, 250}, 500)...)
	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := NewReadTool().Call(context.Background(), Input{"filePath": path}, Context{Cwd: dir})
	if !res.IsError || len(res.Parts) != 0 || !strings.Contains(res.Data, "binary") {
		t.Fatalf("read of a PNG = %q, want a binary-file error", res.Data[:min(len(res.Data), 120)])
	}
}

// "中文注释" in GBK: valid bytes for the file, invalid UTF-8.
var gbkComment = []byte{0x2f, 0x2f, 0x20, 0xd6, 0xd0, 0xce, 0xc4, 0xd7, 0xa2, 0xca, 0xcd, 0x0a}

// latin1Text is ISO-8859-1 "café au lait, crème brûlée": not UTF-8, and not
// GBK either (0xE9 0x20 is no GBK sequence).
var latin1Text = []byte("caf\xe9 au lait, cr\xe8me br\xfbl\xe9e\n")

func mustGBK(t *testing.T, s string) []byte {
	t.Helper()
	b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Chinese Windows tools still write GBK (code page 936). read used to show it
// as mojibake with a note, which left the model unable to work with the file.
func TestReadDecodesGBKText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.c")
	os.WriteFile(path, append(append([]byte{}, gbkComment...), []byte("int main(void) { return 0; }\n")...), 0o644)
	res, _ := NewReadTool().Call(context.Background(), Input{"filePath": path}, Context{Cwd: dir})
	if res.IsError || !strings.Contains(res.Data, "1: // 中文注释") {
		t.Fatalf("read of a GBK file = %q, want the decoded text", res.Data)
	}
	if !strings.Contains(res.Data, "GBK") {
		t.Fatalf("read of a GBK file = %q, want a note naming the encoding", res.Data)
	}
}

func TestReadFlagsTextThatIsNeitherUTF8NorGBK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.txt")
	os.WriteFile(path, latin1Text, 0o644)
	res, _ := NewReadTool().Call(context.Background(), Input{"filePath": path}, Context{Cwd: dir})
	if res.IsError || !strings.Contains(res.Data, "not valid UTF-8") || strings.Contains(res.Data, "GBK-encoded") {
		t.Fatalf("read of a Latin-1 file = %q, want the content with a not-UTF-8 note", res.Data)
	}
}

// Replacing text in a GBK file used to be refused, because writing the
// model's UTF-8 into it would mix two encodings in one file. It is now decoded,
// edited, and encoded back.
func TestEditKeepsGBKEncoding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.c")
	os.WriteFile(path, append(append([]byte{}, gbkComment...), []byte("int x = 1;\n")...), 0o644)
	res, _ := NewEditTool().Call(context.Background(), Input{"filePath": path, "oldString": "int x = 1;", "newString": "int x = 2; // 改"}, Context{Cwd: dir})
	if res.IsError {
		t.Fatal(res.Data)
	}
	want := mustGBK(t, "// 中文注释\nint x = 2; // 改\n")
	if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
		t.Fatalf("file = % x, want GBK % x", got, want)
	}
}

func TestEditRefusesTextGBKCannotRepresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.c")
	orig := append(append([]byte{}, gbkComment...), []byte("int x = 1;\n")...)
	os.WriteFile(path, orig, 0o644)
	res, _ := NewEditTool().Call(context.Background(), Input{"filePath": path, "oldString": "int x = 1;", "newString": "int x = 2; // ✅ done"}, Context{Cwd: dir})
	if !res.IsError || !strings.Contains(res.Data, "GBK") || !strings.Contains(res.Data, "✅") {
		t.Fatalf("edit adding an emoji to a GBK file = %q, want an error naming the character", res.Data)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, orig) {
		t.Fatal("the file was modified")
	}
}

// A file with four-byte GB18030 sequences is GB18030, not GBK: writing it back
// as GBK would fail on (or drop) the characters only GB18030 has.
func TestEditKeepsGB18030Encoding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	enc := simplifiedchinese.GB18030.NewEncoder()
	orig, err := enc.Bytes([]byte("中文 😀\nx = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, orig, 0o644)
	res, _ := NewEditTool().Call(context.Background(), Input{"filePath": path, "oldString": "x = 1", "newString": "x = 2 改"}, Context{Cwd: dir})
	if res.IsError {
		t.Fatal(res.Data)
	}
	want, _ := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte("中文 😀\nx = 2 改\n"))
	if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
		t.Fatalf("file = % x, want GB18030 % x", got, want)
	}
}

// Bytes that are neither UTF-8 nor GBK stay refused: writing UTF-8 into them
// would leave the file in two encodings that no editor shows correctly.
func TestEditRefusesTextThatIsNeitherUTF8NorGBK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.txt")
	os.WriteFile(path, latin1Text, 0o644)
	res, _ := NewEditTool().Call(context.Background(), Input{"filePath": path, "oldString": "au lait", "newString": "noir"}, Context{Cwd: dir})
	if !res.IsError || !strings.Contains(res.Data, "UTF-8") {
		t.Fatalf("edit of a Latin-1 file = %q, want a UTF-8 error", res.Data)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, latin1Text) {
		t.Fatal("the file was modified")
	}
}

// Every accented letter here is followed by an ASCII letter, which happens to
// form a valid GBK double-byte sequence (é+i, ï+v, ...). Real Chinese text is
// mostly GB2312 hanzi with both bytes >= 0xA1; decoding this as GBK would show
// the model rare hanzi and let it write more of them into a Latin-1 file.
func TestLatin1TextThatHappensToBeValidGBKIsNotDecoded(t *testing.T) {
	text := []byte("caf\xe9ine na\xefve r\xe9sum\xe9s \xe0la carte\n")
	if _, err := simplifiedchinese.GBK.NewDecoder().Bytes(text); err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeGBK(text); ok {
		t.Fatal("Latin-1 text was taken for GBK")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "menu.txt")
	os.WriteFile(path, text, 0o644)
	res, _ := NewEditTool().Call(context.Background(), Input{"filePath": path, "oldString": "carte", "newString": "menu"}, Context{Cwd: dir})
	if !res.IsError {
		t.Fatalf("edit of a Latin-1 file = %q, want it refused", res.Data)
	}
}

func TestBinaryishBytesAreNotDecodedAsGBK(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(1 + rng.Intn(255)) // no NUL, so sniffText calls it text
	}
	if _, ok := decodeGBK(data); ok {
		t.Fatal("random bytes were taken for GBK")
	}
}

// write over an existing GBK file keeps its encoding, as it keeps CRLF and BOM.
func TestWriteKeepsExistingGBKEncoding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.c")
	os.WriteFile(path, mustGBK(t, "// 旧注释\r\nint x;\r\n"), 0o644)
	res, _ := NewWriteTool().Call(context.Background(), Input{"filePath": path, "content": "// 新注释\nint y;\n"}, Context{Cwd: dir})
	if res.IsError {
		t.Fatal(res.Data)
	}
	want := mustGBK(t, "// 新注释\r\nint y;\r\n")
	if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
		t.Fatalf("file = % x, want GBK with CRLF % x", got, want)
	}
}

func TestWriteRefusesTextGBKCannotRepresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.c")
	orig := mustGBK(t, "// 旧注释\n")
	os.WriteFile(path, orig, 0o644)
	res, _ := NewWriteTool().Call(context.Background(), Input{"filePath": path, "content": "// 🚀 launch\n"}, Context{Cwd: dir})
	if !res.IsError || !strings.Contains(res.Data, "GBK") {
		t.Fatalf("write of an emoji over a GBK file = %q, want a GBK error", res.Data)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, orig) {
		t.Fatal("the file was modified")
	}
}
