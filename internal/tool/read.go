package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
	_ "golang.org/x/image/webp"
)

type ReadTool struct{ baseTool }

func NewReadTool() Tool {
	return &ReadTool{baseTool{def: Def{
		Name: "read", Aliases: []string{"Read"},
		Description: "Read a file or directory from the filesystem. Returns text with line numbers, native image content for PNG/JPEG/GIF/WebP (up to 5 MiB and 4096 pixels per side), or a directory listing.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"filePath":{"type":"string","description":"Absolute path to the file or directory to read"},
				"offset":{"type":"integer","description":"Line number to start reading from (1-indexed)"},
				"limit":{"type":"integer","description":"Maximum number of lines to read"}
			},
			"required":["filePath"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Read",
	}}}
}

func (t *ReadTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	path, _ := input["filePath"].(string)
	if path == "" {
		return Result{Data: "Error: filePath is required", IsError: true}, nil
	}

	path, err := resolvePathInCwd(path, tctx, false)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{Data: "Error: file not found: " + path, IsError: true}, nil
		}
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	if info.IsDir() {
		return t.readDir(path)
	}
	return t.readFile(path, input, fileTracker(tctx))
}

func (t *ReadTool) readDir(path string) (Result, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	var sb strings.Builder
	sb.WriteString("Directory: " + path + "\n\n")
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		sb.WriteString(name + "\n")
	}
	return Result{Data: strings.TrimRight(sb.String(), "\n")}, nil
}

func (t *ReadTool) readFile(path string, input Input, files *FileTracker) (Result, error) {
	offset := 0
	limit := 0
	if o, ok := input["offset"].(float64); ok && o > 0 {
		offset = int(o) - 1 // convert to 0-indexed
	}
	if l, ok := input["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	// Always use streaming read — avoids double memory allocation from ReadFile + Split
	return t.readFileStream(path, offset, limit, files)
}

func (t *ReadTool) readFileStream(path string, offset, limit int, files *FileTracker) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return Result{Data: "Error: permission denied", IsError: true}, nil
		}
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	defer func() { _ = f.Close() }()

	maxCollect := limit
	if maxCollect <= 0 {
		maxCollect = 2000
	}
	win := newReadWindow(offset, maxCollect)

	// Every byte read also goes through snap, so the tracker gets a snapshot
	// of the whole file without a second pass over it.
	snap := newSnapshotWriter()
	br := bufio.NewReaderSize(io.TeeReader(f, snap), 64*1024)
	recordSeen := func() error {
		if _, err := io.Copy(io.Discard, br); err != nil {
			return err
		}
		files.record(path, snap.snapshot())
		return nil
	}
	// rewind starts reading (and hashing) over from the first byte.
	rewind := func() error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		snap = newSnapshotWriter()
		br = bufio.NewReaderSize(io.TeeReader(f, snap), 64*1024)
		return nil
	}
	sample, _ := br.Peek(8192)
	switch http.DetectContentType(sample) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
		return readImageResult(path, f), nil
	}
	kind := sniffText(sample)

	totalLines := 0
	if kind == textUTF8 {
		// The sniff sees only the first 8KB, and the rest used to be trusted:
		// a file that turned to GBK (or another code page, or binary) after
		// that came back as invalid UTF-8 with no note. Every line is checked
		// as it is scanned, and the file is read again as what it turned out
		// to be.
		var scanErr error
		totalLines, kind, scanErr = scanUTF8Lines(br, win)
		if scanErr != nil {
			return scanFailure(path, totalLines, scanErr, recordSeen), nil
		}
		if kind != textUTF8 {
			if err := rewind(); err != nil {
				return Result{Data: fmt.Sprintf("Error: reading %s failed: %v", path, err), IsError: true}, nil
			}
			win = newReadWindow(offset, maxCollect)
		}
	}

	var legacy *legacyText
	switch kind {
	case textBinary:
		return binaryFileResult(path), nil
	case textUTF16:
		// The model was told what the file is and how to convert it; writing
		// it again as UTF-8 is one way, which must not then fail with "read it
		// first". A binary file is not recorded: write only produces text.
		_ = recordSeen()
		return Result{Data: fmt.Sprintf("Error: %s is UTF-16 text (often written by PowerShell's > or Out-File). Convert it first, e.g. iconv -f UTF-16 -t UTF-8, or Get-Content | Set-Content -Encoding utf8.", path), IsError: true}, nil
	case textNotUTF8:
		// Whether a file is GBK takes all of it (see decodeGBK). This path
		// used to io.ReadAll the file, plus decoded copies, whatever window
		// was asked for, so a multi-GB cp1252 log exhausted memory. Now the
		// first pass streams the whole file through the tee (hash, line count,
		// GBK verdict) and the second reads only up to the window.
		var scanErr error
		legacy, totalLines, scanErr = scanLegacyLines(br)
		if errors.Is(scanErr, errBinaryContent) {
			return binaryFileResult(path), nil
		}
		if scanErr != nil {
			return scanFailure(path, totalLines, scanErr, recordSeen), nil
		}
		if err := recordSeen(); err != nil {
			return Result{Data: fmt.Sprintf("Error: reading %s failed after line %d: %v", path, totalLines, err), IsError: true}, nil
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return Result{Data: fmt.Sprintf("Error: reading %s failed: %v", path, err), IsError: true}, nil
		}
		if err := fillLegacyWindow(f, win, legacy); err != nil {
			return Result{Data: fmt.Sprintf("Error: reading %s failed: %v", path, err), IsError: true}, nil
		}
	default:
		if err := recordSeen(); err != nil {
			return Result{Data: fmt.Sprintf("Error: reading %s failed after line %d: %v", path, totalLines, err), IsError: true}, nil
		}
	}

	var sb strings.Builder
	sb.WriteString("File: ")
	sb.WriteString(path)
	fmt.Fprintf(&sb, " (%d lines total)", totalLines)
	switch {
	case legacy != nil:
		fmt.Fprintf(&sb, "\n[note: file is %s-encoded; shown decoded here, and edit/write keep it in %s]", legacy.name, legacy.name)
	case kind == textNotUTF8:
		sb.WriteString("\n[note: file is not valid UTF-8, and not GBK either (probably another ANSI code page); non-ASCII text below is garbled, and the edit tool will refuse this file]")
	}
	sb.WriteString("\n\n")
	sb.WriteString(win.sb.String())
	if win.collected == 0 {
		sb.WriteString("(no lines in range)\n")
	}
	result := sb.String()

	if totalLines > offset+maxCollect {
		// The last line is a fixed marker the engine keeps when it truncates a
		// tool result, so the model always learns where to continue.
		result = strings.TrimRight(result, "\n") + fmt.Sprintf("\n... [showing lines %d-%d of %d]\n[next: offset=%d]",
			offset+1, offset+win.collected, totalLines, offset+win.collected+1)
	}

	return Result{Data: strings.TrimRight(result, "\n")}, nil
}

func readImageResult(path string, reader io.Reader) Result {
	const maxBytes = 5 * 1024 * 1024
	const maxDim = 4096
	raw, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}
	}
	if len(raw) > maxBytes {
		return Result{Data: "Error: image exceeds the 5 MiB read limit; resize it or attach it with /attach", IsError: true}
	}
	configuration, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Result{Data: "Error: invalid binary image: " + err.Error(), IsError: true}
	}
	if configuration.Width <= 0 || configuration.Height <= 0 || configuration.Width > maxDim || configuration.Height > maxDim {
		return Result{Data: fmt.Sprintf("Error: image dimensions %dx%d exceed the 4096-pixel read limit; resize it or attach it with /attach", configuration.Width, configuration.Height), IsError: true}
	}
	return Result{
		Data:  fmt.Sprintf("Image: %s (%s, %dx%d)", path, format, configuration.Width, configuration.Height),
		Parts: []api.MessagePart{{Type: "image", MimeType: "image/" + format, Data: base64.StdEncoding.EncodeToString(raw), FileName: filepath.Base(path)}},
	}
}

// errBinaryContent is a NUL byte found past the sniffed start of a file.
var errBinaryContent = errors.New("binary content")

func binaryFileResult(path string) Result {
	return Result{Data: fmt.Sprintf("Error: %s is a binary file; read only shows text. Use bash (file, xxd, or the format's own tool) to inspect it.", path), IsError: true}
}

// scanFailure reports a scan that stopped early. Scanner errors were
// previously dropped, so a line longer than the 1MB buffer (minified JS, a
// single-line JSON blob) or a mid-read IO failure silently returned a partial
// file that looked complete. Surfacing it is essential: a model that edits
// based on truncated content corrupts the file.
func scanFailure(path string, lines int, err error, recordSeen func() error) Result {
	if errors.Is(err, bufio.ErrTooLong) {
		// The model goes on to inspect the file with bash; recording it
		// keeps edit usable on it while still catching later changes.
		_ = recordSeen()
		return Result{Data: fmt.Sprintf(
			"Error: %s contains a line longer than the 1MB read limit (stopped at line %d). "+
				"Use bash with head/cut, or grep, to inspect it.", path, lines+1),
			IsError: true}
	}
	return Result{Data: fmt.Sprintf("Error: reading %s failed after line %d: %v", path, lines, err), IsError: true}
}

func newReadScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 256*1024), 1024*1024) // support long lines
	return sc
}

// readWindow numbers and collects the lines read shows: offset+1 through
// offset+max.
type readWindow struct {
	offset, max, collected int
	sb                     strings.Builder
}

func newReadWindow(offset, maxLines int) *readWindow {
	w := &readWindow{offset: offset, max: maxLines}
	// Pre-size for ~80 chars per line, bounded: a huge limit asked the
	// builder for limit*80 bytes up front.
	w.sb.Grow(min(maxLines, 2000) * 80)
	return w
}

// wants reports whether line n (1-based) is in the window.
func (w *readWindow) wants(n int) bool { return n > w.offset && w.collected < w.max }

func (w *readWindow) full() bool { return w.collected >= w.max }

func (w *readWindow) add(n int, line string) {
	w.sb.WriteString(strconv.Itoa(n))
	w.sb.WriteString(": ")
	w.sb.WriteString(clipReadLine(line))
	w.sb.WriteByte('\n')
	w.collected++
}

// scanUTF8Lines counts every line of r and collects the window's, as long as
// the lines are UTF-8 text. It stops at the first line that is not, returning
// what the file turned out to be (textNotUTF8 or textBinary).
func scanUTF8Lines(r io.Reader, win *readWindow) (int, textKind, error) {
	sc := newReadScanner(r)
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.IndexByte(line, 0) >= 0 {
			return n, textBinary, nil
		}
		if !utf8.Valid(line) {
			return n, textNotUTF8, nil
		}
		n++
		if win.wants(n) {
			win.add(n, string(line))
		}
	}
	return n, textUTF8, sc.Err()
}

// scanLegacyLines counts the lines of a file that is not UTF-8 and decides,
// as decodeGBK would for the whole file, whether it is GBK/GB18030, without
// holding more than one line in memory.
func scanLegacyLines(r io.Reader) (*legacyText, int, error) {
	sc := newReadScanner(r)
	var gs gbkStream
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.IndexByte(line, 0) >= 0 {
			return nil, n, errBinaryContent
		}
		gs.feed(line)
		n++
	}
	if err := sc.Err(); err != nil {
		return nil, n, err
	}
	if lt, ok := gs.result(); ok {
		return &lt, n, nil
	}
	return nil, n, nil
}

// fillLegacyWindow collects the window's lines of a non-UTF-8 file, decoded
// when it is GBK/GB18030 and raw otherwise, and stops reading once it is full.
func fillLegacyWindow(r io.Reader, win *readWindow, legacy *legacyText) error {
	sc := newReadScanner(r)
	for n := 1; !win.full() && sc.Scan(); n++ {
		if !win.wants(n) {
			continue
		}
		text := string(sc.Bytes())
		if legacy != nil {
			if decoded, err := legacy.enc.NewDecoder().Bytes(sc.Bytes()); err == nil {
				text = string(decoded)
			}
		}
		win.add(n, text)
	}
	return sc.Err()
}

// readMaxLineChars caps one line of read output. A minified bundle or a JSON
// blob on a single line would otherwise fill the context by itself.
const readMaxLineChars = 2000

// clipReadLine cuts line to readMaxLineChars characters and says how many
// were left out.
func clipReadLine(line string) string {
	if len(line) <= readMaxLineChars {
		return line
	}
	n := utf8.RuneCountInString(line)
	if n <= readMaxLineChars {
		return line
	}
	return textutil.HeadRunes(line, readMaxLineChars) + fmt.Sprintf("…[+%d chars]", n-readMaxLineChars)
}

func (t *ReadTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("read is read-only")
}
