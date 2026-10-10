package termui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestStyledWrapsTextInColorAndReset(t *testing.T) {
	if got, want := Styled(Red, "boom"), "\x1b[31mboom\x1b[0m"; got != want {
		t.Errorf("Styled(Red, %q) = %q, want %q", "boom", got, want)
	}
	if got, want := Styled(Bold+BrightCyan, "hi"), "\x1b[1m\x1b[96mhi\x1b[0m"; got != want {
		t.Errorf("Styled(Bold+BrightCyan, %q) = %q, want %q", "hi", got, want)
	}
	// Empty text still emits the pair, so a caller cannot accidentally leave
	// the terminal in a colored state.
	if got, want := Styled(Green, ""), "\x1b[32m\x1b[0m"; got != want {
		t.Errorf("Styled(Green, \"\") = %q, want %q", got, want)
	}
}

// TestStyleConstantsAreWellFormedEscapes catches a typo'd constant (a missing
// final "m", a stray digit) which would otherwise leak raw escape text into the
// user's terminal instead of being consumed as a zero-width control sequence.
func TestStyleConstantsAreWellFormedEscapes(t *testing.T) {
	constants := map[string]string{
		"Reset": Reset, "Bold": Bold, "Dim": Dim, "Italic": Italic, "Underline": Underline,
		"Black": Black, "Red": Red, "Green": Green, "Yellow": Yellow, "Blue": Blue,
		"Magenta": Magenta, "Cyan": Cyan, "White": White, "Gray": Gray,
		"BrightRed": BrightRed, "BrightGreen": BrightGreen, "BrightYellow": BrightYellow,
		"BrightBlue": BrightBlue, "BrightCyan": BrightCyan,
		"ReasoningStyle": ReasoningStyle,
	}
	for name, code := range constants {
		t.Run(name, func(t *testing.T) {
			if ansi.StringWidth(code) != 0 {
				t.Errorf("%s occupies %d display columns, want 0", name, ansi.StringWidth(code))
			}
			if got := ansi.Strip(Styled(code, "X")); got != "X" {
				t.Errorf("ansi.Strip(Styled(%s, \"X\")) = %q, want %q", name, got, "X")
			}
			if w := ansi.StringWidth(Styled(code, "X")); w != 1 {
				t.Errorf("Styled(%s, \"X\") width = %d, want 1", name, w)
			}
		})
	}
}

func TestToolResultSuccess(t *testing.T) {
	got := ToolResult("bash", "ok", false)
	want := "  \x1b[2m✓\x1b[0m \x1b[36m[bash]\x1b[0m ok\x1b[0m"
	if got != want {
		t.Errorf("ToolResult(bash, ok, false)\ngot:  %q\nwant: %q", got, want)
	}
	if plain, wantPlain := ansi.Strip(got), "  ✓ [bash] ok"; plain != wantPlain {
		t.Errorf("stripped = %q, want %q", plain, wantPlain)
	}
}

func TestToolResultErrorUsesRedAndCross(t *testing.T) {
	got := ToolResult("bash", "boom", true)
	want := "  \x1b[2m✗\x1b[0m \x1b[31m[bash]\x1b[0m \x1b[31mboom\x1b[0m"
	if got != want {
		t.Errorf("ToolResult(bash, boom, true)\ngot:  %q\nwant: %q", got, want)
	}
	// The error variant must color the summary too, not just the tool name --
	// that is the only visual difference for a summary with no icon in view.
	if !strings.Contains(got, Red+"boom") {
		t.Errorf("error summary not colored red: %q", got)
	}
	if strings.Contains(got, "✓") {
		t.Errorf("error result still shows the success icon: %q", got)
	}
}

func TestToolResultPreservesCJKExactly(t *testing.T) {
	got := ToolResult("写文件", "已更新 3 行", false)
	want := "  \x1b[2m✓\x1b[0m \x1b[36m[写文件]\x1b[0m 已更新 3 行\x1b[0m"
	if got != want {
		t.Errorf("ToolResult with CJK\ngot:  %q\nwant: %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Errorf("ToolResult produced invalid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("ToolResult mangled a multi-byte rune: %q", got)
	}
	if plain, wantPlain := ansi.Strip(got), "  ✓ [写文件] 已更新 3 行"; plain != wantPlain {
		t.Errorf("stripped = %q, want %q", plain, wantPlain)
	}
}

// TestToolResultDisplayWidthIgnoresEscapes pins that the escapes ToolResult adds
// cost zero columns: a double-width name must advance the cursor by 2 per rune
// and the color codes by nothing. len() on this string is 46 bytes, which is
// what a width calculation must not use.
func TestToolResultDisplayWidthIgnoresEscapes(t *testing.T) {
	cases := []struct {
		name, summary string
		wantWidth     int
	}{
		// "  " + "✓" + " " + "[bash]" + " " + "ok"
		{"bash", "ok", 2 + 1 + 1 + 6 + 1 + 2},
		// CJK: 写文件 = 6 columns, 已更新 3 行 = 6+1+1+1+2 = 11
		{"写文件", "已更新 3 行", 2 + 1 + 1 + (2 + 6) + 1 + 11},
	}
	for _, tc := range cases {
		got := ToolResult(tc.name, tc.summary, false)
		if w := ansi.StringWidth(got); w != tc.wantWidth {
			t.Errorf("ansi.StringWidth(ToolResult(%q, %q)) = %d, want %d (raw %q)",
				tc.name, tc.summary, w, tc.wantWidth, got)
		}
	}
}

// TestToolResultWithAlreadyStyledSummary covers a summary that arrives with its
// own escapes: they must pass through untouched and still strip away cleanly,
// rather than being escaped, doubled, or counted as visible width.
func TestToolResultWithAlreadyStyledSummary(t *testing.T) {
	summary := Green + "3 passed" + Reset
	got := ToolResult("test", summary, false)

	if !strings.Contains(got, summary) {
		t.Errorf("pre-styled summary not passed through verbatim: %q", got)
	}
	if plain, want := ansi.Strip(got), "  ✓ [test] 3 passed"; plain != want {
		t.Errorf("stripped = %q, want %q", plain, want)
	}
	// Compare DISPLAY WIDTH against display width, not against len(): "✓" is
	// three bytes but occupies one column, so len() would demand 21 for a line
	// that correctly renders in 19.
	if w, want := ansi.StringWidth(got), ansi.StringWidth("  ✓ [test] 3 passed"); w != want {
		t.Errorf("width = %d, want %d", w, want)
	}
}

func TestPermissionPromptExactOutput(t *testing.T) {
	got := PermissionPrompt("Bash", "ls -la")
	want := "\r\x1b[K\a" +
		"\n  \x1b[33m┃\x1b[0m \x1b[1m需要授权\x1b[0m  \x1b[36mBash\x1b[0m\n" +
		"  \x1b[33m┃\x1b[0m ls -la\n" +
		"  \x1b[33m┃\x1b[0m\n"
	if got != want {
		t.Errorf("PermissionPrompt\ngot:  %q\nwant: %q", got, want)
	}
	// The prompt must begin by clearing the current line (the spinner lives
	// there) and ringing the bell, or the box renders on top of spinner frames.
	if !strings.HasPrefix(got, "\r\x1b[K\a") {
		t.Errorf("prompt does not start with clear-line + bell: %q", got)
	}
}

func TestPermissionPromptOmitsDescriptionLineWhenEmpty(t *testing.T) {
	got := PermissionPrompt("Read", "")
	plain := ansi.Strip(got)
	// Header and the empty spacer row only: no description row.
	if n := strings.Count(plain, "┃"); n != 2 {
		t.Errorf("want header + spacer gutter rows, got %d: %q", n, plain)
	}
	if !strings.Contains(plain, "┃ 需要授权  Read\n  ┃\n") {
		t.Errorf("header or spacer wrong: %q", plain)
	}
}

// Every row of a multi-line description sits behind the gutter, at the same
// content column as the first.
func TestPermissionPromptAlignsContinuationRows(t *testing.T) {
	plain := ansi.Strip(PermissionPrompt("bash", "git commit -m \"x\" -m \"- a\n- b\""))
	if !strings.Contains(plain, "  ┃ git commit -m \"x\" -m \"- a\n  ┃ - b\"\n  ┃\n") {
		t.Errorf("description rows not aligned behind the gutter: %q", plain)
	}
}

// The prompt used to be a box with a fixed 34-column top rule: a command
// longer than that broke its right side and the top read as a short tab.
// Now every row starts with the same gutter, whatever the command's width,
// so nothing can be misaligned.
func TestPermissionPromptEveryRowStartsWithTheGutter(t *testing.T) {
	long := "dotnet --version 2>/dev/null && dotnet --list-sdks 2>/dev/null || echo \"dotnet not found\""
	plain := ansi.Strip(PermissionPrompt("bash", long))
	rows := strings.Split(strings.TrimSuffix(strings.TrimPrefix(plain, "\r\a\n"), "\n"), "\n")
	if len(rows) != 3 {
		t.Fatalf("want header, command, spacer; got %d rows: %q", len(rows), plain)
	}
	for _, r := range rows {
		if !strings.HasPrefix(r, "  ┃") {
			t.Errorf("row without the gutter: %q", r)
		}
	}
	if !strings.Contains(plain, "┃ "+long+"\n") {
		t.Errorf("long command was not shown whole on its row: %q", plain)
	}
	if strings.ContainsAny(plain, "╭╮╰╯│") {
		t.Errorf("box drawing survived: %q", plain)
	}
}

// This test used to require the description to be clipped to 60 runes. That
// clipping was the bug: the description is the command being approved, and
// cutting it hid whatever came after column 60. A 200-character command is
// now shown whole.
func TestPermissionPromptShowsLongASCIIDescriptionWhole(t *testing.T) {
	desc := strings.Repeat("a", 200)
	line := descriptionLine(t, PermissionPrompt("Bash", desc))

	if line != desc {
		t.Errorf("200-rune description was altered: got %d runes %q", utf8.RuneCountInString(line), line)
	}
}

// TestPermissionPromptKeepsChineseDescriptionValidUTF8 is the regression guard
// for the byte-slicing bug class in this codebase: shortening a Chinese
// description at a byte offset cuts a 3-byte rune apart and renders as U+FFFD.
// Only a huge description is shortened now (from the middle), so that is the
// case exercised; it used to assert a 60-rune clip, which hid the command.
func TestPermissionPromptKeepsChineseDescriptionValidUTF8(t *testing.T) {
	// ~11,000 bytes of Chinese, past the prompt's byte budget.
	desc := strings.Repeat("更新配置文件并运行测试以确认修复生效", 200)

	out := PermissionPrompt("Edit", desc)

	if !utf8.ValidString(out) {
		t.Errorf("PermissionPrompt produced invalid UTF-8 for a Chinese description")
	}
	if strings.ContainsRune(out, utf8.RuneError) {
		t.Errorf("description was cut mid-rune (contains U+FFFD)")
	}
	// It really was shortened, not passed through whole.
	if strings.Contains(out, desc) {
		t.Errorf("a %d-byte description was not shortened", len(desc))
	}
	// The start survives as a prefix of the original.
	if line := descriptionLine(t, out); !strings.HasPrefix(desc, line) {
		t.Errorf("first description row %q is not a prefix of the original", line)
	}
}

func TestPermissionPromptLeavesShortDescriptionIntact(t *testing.T) {
	// Exactly at the 60-byte guard: must not be touched.
	desc := strings.Repeat("b", 60)
	if line := descriptionLine(t, PermissionPrompt("Bash", desc)); line != desc {
		t.Errorf("60-byte description was modified: got %q", line)
	}
	// A short Chinese description (21 runes / 63 bytes) crosses the byte guard
	// but not the rune budget, so it must survive whole.
	cjk := strings.Repeat("配", 21)
	if line := descriptionLine(t, PermissionPrompt("Bash", cjk)); line != cjk {
		t.Errorf("21-rune Chinese description was modified: got %q, want %q", line, cjk)
	}
}

// descriptionLine pulls the first description row (the gutter row after the
// header) out of a rendered prompt.
func descriptionLine(t *testing.T, prompt string) string {
	t.Helper()
	header := true
	for _, line := range strings.Split(ansi.Strip(prompt), "\n") {
		if !strings.HasPrefix(line, "  ┃") {
			continue
		}
		if header {
			header = false
			continue
		}
		if line == "  ┃" {
			break // the spacer: no description row
		}
		return strings.TrimPrefix(line, "  ┃ ")
	}
	t.Fatalf("no description row in prompt %q", prompt)
	return ""
}

const bannerArt = "\n" +
	"     ______   ____  _    __  ______\n" +
	"    / ____/  / __ \\ | |  / / / ____/\n" +
	"   / /      / / / / | | / / / __/   \n" +
	"  / /___  / /_/ /  | |/ / / /___   \n" +
	"  \\____/  \\____/   |___/ /_____/   \n" +
	"\n"

func TestBannerExactPlainTextWithGit(t *testing.T) {
	got := ansi.Strip(Banner("1.2.3", "gpt-4", "openai", "auto", "/tmp/proj", "main", "", 7, true))
	want := bannerArt +
		"    cove v1.2.3  •  高效、安全的本地 AI 协同编程终端\n\n" +
		"  模型: gpt-4  │  供应商: openai  │  模式: auto\n" +
		"  Git: main\n" +
		"  目录: /tmp/proj\n" +
		"  工具: 7 个\n\n" +
		"  提示: 输入 /help 查看命令，/keys 查看快捷键，Esc 或 Ctrl+C 中断\n\n"
	if got != want {
		t.Errorf("Banner (git)\ngot:  %q\nwant: %q", got, want)
	}
}

func TestBannerOmitsGitLineWhenNotARepo(t *testing.T) {
	got := ansi.Strip(Banner("1.2.3", "gpt-4", "openai", "auto", "/tmp/proj", "main", "", 7, false))
	want := bannerArt +
		"    cove v1.2.3  •  高效、安全的本地 AI 协同编程终端\n\n" +
		"  模型: gpt-4  │  供应商: openai  │  模式: auto\n" +
		"  目录: /tmp/proj\n" +
		"  工具: 7 个\n\n" +
		"  提示: 输入 /help 查看命令，/keys 查看快捷键，Esc 或 Ctrl+C 中断\n\n"
	if got != want {
		t.Errorf("Banner (no git)\ngot:  %q\nwant: %q", got, want)
	}
	// The branch argument must be ignored entirely, not just unlabeled.
	if strings.Contains(got, "main") {
		t.Errorf("branch name leaked into a non-git banner: %q", got)
	}
}

func TestBannerColorsGitBranchGreen(t *testing.T) {
	got := Banner("v", "m", "p", "auto", "/w", "feature/x", "", 1, true)
	if !strings.Contains(got, Green+"feature/x"+Reset) {
		t.Errorf("git branch is not wrapped in green: %q", got)
	}
}

func TestBannerInterpolatesAllArguments(t *testing.T) {
	got := ansi.Strip(Banner("9.9.9-rc1", "claude-opus", "anthropic", "plan", "/srv/工程目录", "发布/v2", "", 42, true))
	for _, want := range []string{
		"cove v9.9.9-rc1",
		"模型: claude-opus",
		"供应商: anthropic",
		"模式: plan",
		"Git: 发布/v2",
		"目录: /srv/工程目录",
		"工具: 42 个",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner missing %q\nfull: %q", want, got)
		}
	}
}

// TestBannerEmitsNoStrayEscapeBytes guards against an unterminated or
// malformed sequence: anything ansi.Strip cannot consume would be printed as
// literal garbage in the terminal.
func TestBannerEmitsNoStrayEscapeBytes(t *testing.T) {
	outputs := []string{
		Banner("1.0", "m", "p", "auto", "/w", "main", "", 3, true),
		Banner("1.0", "m", "p", "auto", "/w", "main", "", 3, false),
		ToolResult("bash", "ok", false),
		ToolResult("bash", "boom", true),
		PermissionPrompt("Bash", "ls"),
		PermissionPrompt("Bash", ""),
	}
	for _, out := range outputs {
		plain := ansi.Strip(out)
		if strings.ContainsRune(plain, 0x1b) {
			t.Errorf("stripped output still contains an ESC byte: %q", plain)
		}
		if !utf8.ValidString(out) {
			t.Errorf("output is not valid UTF-8: %q", out)
		}
	}
}

func TestGutterBoxKeepsPreRenderedBody(t *testing.T) {
	body := "\x1b[1m计划\x1b[0m\n  • 第一步"
	out := GutterBox("需要确认", "整树回退", body, false)
	if !strings.Contains(out, "需要确认") || !strings.Contains(out, "整树回退") {
		t.Fatalf("header missing: %q", out)
	}
	if !strings.Contains(out, "\x1b[1m计划\x1b[0m") {
		t.Fatalf("pre-rendered styles must survive: %q", out)
	}
	if strings.Count(out, "┃") < 4 {
		t.Fatalf("every body line needs the gutter: %q", out)
	}
	escaped := GutterBox("需要授权", "bash", "echo \x1b[2J", true)
	if strings.Contains(escaped, "\x1b[2J") {
		t.Fatalf("escapeBody must make controls visible: %q", escaped)
	}
}
