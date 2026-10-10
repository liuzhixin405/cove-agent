package render

import (
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

const mdSample = "## 标题\n正文 **粗** `code`\n```go\nfmt.Println()\n```\n- 项"

// feed renders chunks through one stream and returns everything it printed.
func feed(m *MarkdownStream, chunks ...string) string {
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(m.Write(c))
	}
	sb.WriteString(m.Flush())
	return sb.String()
}

func plainLines(s string) []string { return strings.Split(ansi.Strip(s), "\n") }

// The brief's case: the sample fed in three chunks renders the heading bold,
// the code block indented and the list item with a bullet.
func TestMarkdownStreamRendersSampleInThreeChunks(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "0")
	out := feed(NewMarkdownStream(), "## 标", "题\n正文 **粗** `co", "de`\n```go\nfmt.Println()\n```\n- 项")

	if !strings.Contains(out, "\x1b[1m标题") {
		t.Errorf("heading is not bold: %q", out)
	}
	if strings.Contains(ansi.Strip(out), "##") {
		t.Errorf("heading markers were printed: %q", out)
	}
	if !strings.Contains(out, "\x1b[1m粗\x1b[22m") {
		t.Errorf("**粗** is not bold: %q", out)
	}
	if !strings.Contains(out, "\x1b[7mcode\x1b[27m") {
		t.Errorf("`code` is not reverse video: %q", out)
	}
	var codeLine, listLine string
	for _, l := range plainLines(out) {
		if strings.Contains(l, "fmt.Println()") {
			codeLine = l
		}
		if strings.Contains(l, "项") {
			listLine = l
		}
	}
	if !strings.HasPrefix(codeLine, "  ") || strings.TrimSpace(codeLine) == "fmt.Println()" {
		t.Errorf("code line is not indented behind a border: %q", codeLine)
	}
	if !strings.HasPrefix(listLine, "•") {
		t.Errorf("list line = %q, want it to start with •", listLine)
	}
	if strings.Contains(ansi.Strip(out), "```") {
		t.Errorf("fence markers were printed: %q", out)
	}
}

// Chunk boundaries are wherever the transport cut the stream, so the output
// must not depend on them: every two-cut split renders like the whole input.
func TestMarkdownStreamIsIndependentOfChunking(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "0")
	want := feed(NewMarkdownStream(), mdSample)
	for i := 0; i <= len(mdSample); i++ {
		for j := i; j <= len(mdSample); j++ {
			got := feed(NewMarkdownStream(), mdSample[:i], mdSample[i:j], mdSample[j:])
			if got != want {
				t.Fatalf("split at %d,%d:\n got %q\nwant %q", i, j, got, want)
			}
		}
	}
}

// On a console that cannot show them, the renderer adds no non-ASCII glyphs of
// its own (the user's text is of course left alone).
func TestMarkdownStreamASCIIModeAddsNoSymbols(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "1")
	out := feed(NewMarkdownStream(), "## 标", "题\n正文 **粗** `co", "de`\n```go\nfmt.Println()\n```\n- 项\n> 引用\n")
	for _, r := range ansi.Strip(out) {
		if r > unicode.MaxASCII && !strings.ContainsRune(mdSample+"引用", r) {
			t.Fatalf("ASCII mode printed %q: %q", r, out)
		}
	}
	var listLine string
	for _, l := range plainLines(out) {
		if strings.Contains(l, "项") {
			listLine = l
		}
	}
	if !strings.HasPrefix(listLine, "- ") {
		t.Errorf("ASCII list line = %q, want \"- 项\"", listLine)
	}
}

// Text streams out as it arrives; only a possible marker is held back.
func TestMarkdownStreamDoesNotWaitForTheNewline(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "0")
	m := NewMarkdownStream()
	if got := m.Write("我来看看文件"); got != "我来看看文件" {
		t.Fatalf("plain text was held back: %q", got)
	}
	if got := m.Write("，先 *"); got != "，先 " {
		t.Fatalf("a lone trailing * must wait for the next chunk: %q", got)
	}
	if got := m.Write("*重点*"); got != "\x1b[1m重点" {
		t.Fatalf("got %q", got)
	}
	if got := m.Write("*"); got != "\x1b[22m" {
		t.Fatalf("the closing ** split across chunks did not end the bold: %q", got)
	}
	if got := m.Flush(); got != "" {
		t.Fatalf("Flush with nothing pending printed %q", got)
	}
	// A marker still held back at the end of the stream is printed as text.
	m.Write("5 *")
	if got := m.Flush(); got != "*" {
		t.Fatalf("Flush = %q, want the held-back *", got)
	}
}

func TestMarkdownStreamCodeBlockIsNotFormatted(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "0")
	out := feed(NewMarkdownStream(), "~~~\nx := **y** `z`\n# not a heading\n~~~\nafter **b**\n")
	plain := ansi.Strip(out)
	if !strings.Contains(plain, "x := **y** `z`") {
		t.Errorf("code was formatted: %q", out)
	}
	if !strings.Contains(plain, "# not a heading") {
		t.Errorf("a # line inside code became a heading: %q", out)
	}
	if !strings.Contains(out, "\x1b[1mb\x1b[22m") {
		t.Errorf("formatting did not resume after the fence closed: %q", out)
	}
}

// A ``` fence is not closed by ~~~, and the block stays open across chunks.
func TestMarkdownStreamFenceNeedsMatchingMarker(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "0")
	m := NewMarkdownStream()
	out := feed(m, "```\n~~~\n", "- still code\n```\n- item\n")
	lines := plainLines(out)
	var sawTilde, sawCodeDash, sawItem bool
	for _, l := range lines {
		switch {
		case strings.HasSuffix(l, "~~~") && strings.HasPrefix(l, "  "):
			sawTilde = true
		case strings.HasSuffix(l, "- still code") && strings.HasPrefix(l, "  "):
			sawCodeDash = true
		case strings.HasPrefix(l, "• item"):
			sawItem = true
		}
	}
	if !sawTilde || !sawCodeDash || !sawItem {
		t.Errorf("fence handling wrong (tilde=%v dash=%v item=%v): %q", sawTilde, sawCodeDash, sawItem, lines)
	}
}

func TestMarkdownStreamLineShapes(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "0")
	cases := []struct{ in, want string }{
		{"#hashtag\n", "#hashtag"},     // no space: not a heading
		{"  - nested\n", "  • nested"}, // indentation kept
		{"* star\n", "• star"},         // * bullet
		{"1. first\n", "1. first"},     // ordered lists are left as written
		{"> quoted\n", "│ quoted"},     // block quote
		{"2 * 3 = 6\n", "2 * 3 = 6"},   // a lone * is text
		{"**unclosed\n", "unclosed"},   // styles never leak past the line
		{"----\n", "----"},             // not a bullet
		{"###### h6\n", "h6"},          // deepest heading
		{"####### seven\n", "####### seven"},
	}
	for _, c := range cases {
		out := feed(NewMarkdownStream(), c.in)
		if got := plainLines(out)[0]; got != c.want {
			t.Errorf("%q rendered as %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(c.in, "**unclosed") && !strings.HasSuffix(strings.TrimSuffix(out, "\n"), "\x1b[0m") {
			t.Errorf("an unclosed ** was not reset at the end of the line: %q", out)
		}
	}
}

// Flush mid-line (a tool call interrupts the answer) closes open styles so
// the tool block is not printed bold, and the stream starts a fresh line
// afterwards.
func TestMarkdownStreamFlushClosesStyles(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "0")
	m := NewMarkdownStream()
	out := m.Write("## 正在")
	out += m.Flush()
	if !strings.HasSuffix(out, "\x1b[0m") {
		t.Errorf("Flush left the heading style open: %q", out)
	}
	if got := m.Write("- next\n"); ansi.Strip(got) != "• next\n" {
		t.Errorf("after Flush the next text is not a new line: %q", got)
	}
	if got := m.Flush(); got != "" {
		t.Errorf("Flush with nothing pending printed %q", got)
	}
}

// Tables were drawn with box-drawing glyphs whatever the console, so on one
// that cannot show them (textmode.PreferASCII) every table was mojibake.
func TestMarkdownTableASCIIMode(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "1")
	out := feed(NewMarkdownStream(), "| 名称 | 值 |\n|---|---|\n| a | 1 |\n\n")
	plain := ansi.Strip(out)
	for _, r := range plain {
		if r > unicode.MaxASCII && !strings.ContainsRune("名称值", r) {
			t.Fatalf("ASCII-mode table printed %q:\n%s", r, plain)
		}
	}
	for _, want := range []string{"+------+----+", "| 名称 | 值 |", "| a    | 1  |"} {
		if !strings.Contains(plain, want) {
			t.Errorf("ASCII table lacks %q:\n%s", want, plain)
		}
	}
}

func TestMarkdownRendersWholeText(t *testing.T) {
	out := Markdown("# 标题\n\n- 第一步\n- 第二步\n")
	if !strings.Contains(out, "标题") || !strings.Contains(out, "第一步") || !strings.Contains(out, "第二步") {
		t.Fatalf("content lost: %q", out)
	}
	// The heading is rendered (bold), so the text went through the renderer
	// rather than being returned verbatim. (The bullet glyph depends on the
	// console: "-" in ASCII mode, so it is not asserted.)
	if !strings.Contains(out, "[1m标题[0m") {
		t.Fatalf("heading not rendered: %q", out)
	}
	if Markdown("") != "" {
		t.Fatal("empty input must render to nothing")
	}
}
