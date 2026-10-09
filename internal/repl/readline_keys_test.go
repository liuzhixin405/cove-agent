package repl

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// captureStdout swaps os.Stdout (every write in this package goes through
// fmt.Print) for a pipe and returns a function that restores it and hands back
// what was written.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	restored := false
	restore := func() string {
		if restored {
			return ""
		}
		restored = true
		os.Stdout = old
		w.Close()
		out := <-done
		r.Close()
		return out
	}
	t.Cleanup(func() { restore() })
	return restore
}

// typed builds a LineReader whose raw input is the given key bytes, the way
// the terminal delivers them in raw mode.
func typed(keys string) *LineReader {
	lr := New(nil)
	lr.rawReader = bufio.NewReader(strings.NewReader(keys))
	return lr
}

func editOnce(t *testing.T, keys string) (string, error) {
	t.Helper()
	restore := captureStdout(t)
	line, err := typed(keys).editLine()
	restore()
	return line, err
}

// Only a bare ESC [ <letter> was understood. Any sequence with parameters —
// Ctrl/Shift/Alt + arrow ("\x1b[1;5D", which Windows Terminal sends for
// Ctrl+Left), Home/End as "\x1b[1~"/"\x1b[4~", Insert, F5 — stopped after the
// first parameter byte and typed the rest into the input as text (";5D", "~").
func TestEscapeSequencesNeverTypeGarbage(t *testing.T) {
	tests := []struct {
		name, keys, want string
	}{
		{"ctrl+left jumps to the word start", "ab\x1b[1;5Dx\r", "xab"},
		{"ctrl+left then ctrl+right", "ab cd\x1b[1;5D\x1b[1;5D\x1b[1;5Cx\r", "abx cd"},
		{"shift+right moves right", "ab\x1b[D\x1b[D\x1b[1;2Cx\r", "axb"},
		{"home as CSI 1~", "bc\x1b[1~a\r", "abc"},
		{"home as CSI 7~", "bc\x1b[7~a\r", "abc"},
		{"end as CSI 4~", "ac\x1b[D\x1b[D\x1b[4~d\r", "acd"},
		{"home as SS3 H", "bc\x1bOHa\r", "abc"},
		{"end as SS3 F", "ab\x1b[D\x1b[D\x1bOFc\r", "abc"},
		{"insert is ignored", "ab\x1b[2~\r", "ab"},
		{"F5 is ignored", "ab\x1b[15~\r", "ab"},
		{"F1 as SS3 is ignored", "ab\x1bOP\r", "ab"},
		{"ctrl+delete deletes", "abc\x1b[D\x1b[3;5~\r", "ab"},
		{"focus event is ignored", "ab\x1b[I\x1b[O\r", "ab"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := editOnce(t, tc.keys)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("typed %q, got line %q, want %q", tc.keys, got, tc.want)
			}
		})
	}
}

// Every newline in raw mode must be written as \r\n; a bare \n moves down
// without returning to column 0 and staircases the rest of the screen.
func assertNoBareLF(t *testing.T, out string) {
	t.Helper()
	for i := 0; i < len(out); i++ {
		if out[i] == '\n' && (i == 0 || out[i-1] != '\r') {
			t.Fatalf("bare \\n written in raw mode at byte %d: %q", i, out)
		}
	}
}

// Pasting several lines submitted each newline as its own message (and the
// input reader was rebuilt on every call, so whatever the first read had
// already buffered was silently dropped). A paste is one message.
func TestBracketedPasteIsOneMessage(t *testing.T) {
	restore := captureStdout(t)
	got, err := typed("\x1b[200~func main() {\r\tfmt.Println(1)\r}\x1b[201~\r").editLine()
	out := restore()
	if err != nil {
		t.Fatal(err)
	}
	if want := "func main() {\n\tfmt.Println(1)\n}"; got != want {
		t.Fatalf("pasted text = %q, want %q", got, want)
	}
	assertNoBareLF(t, out)
}

func TestImageInputHooksPreserveDraft(t *testing.T) {
	for _, tc := range []struct {
		name, keys, want string
		pastes, clips    int
	}{
		{"image paste", "before\x1b[200~\"screen.png\"\x1b[201~after\r", "beforeafter", 1, 0},
		{"ordinary typing", "screen.png\r", "screen.png", 0, 0},
		{"ordinary paste", "\x1b[200~code\ntext\x1b[201~\r", "code\ntext", 1, 0},
		{"clipboard shortcut", "before\x1bvafter\r", "beforeafter", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := captureStdout(t)
			reader := typed(tc.keys)
			pastes, clips := 0, 0
			reader.SetImageInputHooks(func(text string) bool {
				pastes++
				return text == `"screen.png"`
			}, func() { clips++ }, nil)
			got, err := reader.editLine()
			restore()
			if err != nil || got != tc.want || pastes != tc.pastes || clips != tc.clips {
				t.Fatalf("line=%q err=%v pastes=%d clips=%d", got, err, pastes, clips)
			}
		})
	}
}

func TestImageInputHintOnlyOnEmptyDraft(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_CLIENT", "")
	restore := captureStdout(t)
	reader := typed("")
	withActiveReader(t, reader)
	consoleMu.Lock()
	streamingActive = false
	consoleMu.Unlock()
	reader.SetImageInputHooks(func(string) bool { return false }, func() {}, nil)
	if !strings.Contains(reader.placeholder, "@路径") || !strings.Contains(reader.placeholder, "拖入") {
		t.Fatalf("attachment hint missing: %q", reader.placeholder)
	}
	if strings.Contains(reader.placeholder, "Alt+V") != (runtime.GOOS == "windows") {
		t.Fatalf("clipboard hint does not match platform: %q", reader.placeholder)
	}
	reader.redraw(nil, 0)
	out := restore()
	if !strings.Contains(out, "@路径") {
		t.Fatal("empty input did not show attachment hint")
	}
	restore = captureStdout(t)
	reader.redraw([]rune("draft"), 5)
	out = restore()
	if strings.Contains(out, "@路径") || !strings.Contains(out, "draft") {
		t.Fatal("hint remained visible while typing")
	}
	t.Setenv("SSH_CONNECTION", "remote")
	reader.SetImageInputHooks(func(string) bool { return false }, func() {}, nil)
	if strings.Contains(reader.placeholder, "Alt+V") || !strings.Contains(reader.placeholder, "@路径") {
		t.Fatalf("remote hint advertised local clipboard: %q", reader.placeholder)
	}
}

func TestInputStatusPreservesEditorLayout(t *testing.T) {
	restore := captureStdout(t)
	reader := typed("")
	withActiveReader(t, reader)
	consoleMu.Lock()
	streamingActive = false
	consoleMu.Unlock()
	reader.SetInputStatus("pending image 12x8")
	reader.redraw([]rune("draft"), 2)
	if reader.drawnRows != 2 || reader.cursorRow != 1 {
		t.Fatalf("single line rows=%d cursor row=%d", reader.drawnRows, reader.cursorRow)
	}
	reader.redraw([]rune("first\nsecond"), 8)
	if reader.drawnRows != 3 || reader.cursorRow != 2 {
		t.Fatalf("multi line rows=%d cursor row=%d", reader.drawnRows, reader.cursorRow)
	}
	reader.SetInputStatus("")
	reader.redraw([]rune("draft"), 2)
	if reader.drawnRows != 1 || reader.cursorRow != 0 {
		t.Fatalf("cleared status rows=%d cursor row=%d", reader.drawnRows, reader.cursorRow)
	}
	out := restore()
	if !strings.Contains(out, "pending image 12x8") {
		t.Fatal("attachment status not rendered")
	}
	assertNoBareLF(t, out)
}

func TestAgentPanelPreservesDraftAndLayout(t *testing.T) {
	restore := captureStdout(t)
	reader := typed("")
	withActiveReader(t, reader)
	consoleMu.Lock()
	streamingActive = false
	consoleMu.Unlock()
	reader.SetPanelSource(func(index int) ([]string, int) {
		return []string{"Agent Map", fmt.Sprintf("selected %d", index), "unsafe\x1b[2J\ntext"}, 2
	})
	reader.redraw([]rune("draft"), 2)
	if !reader.togglePanel() || reader.drawnRows != 4 || reader.cursorRow != 3 || string(reader.renderBuf) != "draft" || reader.renderCursor != 2 {
		t.Fatal("opening panel changed the draft or cursor layout")
	}
	reader.movePanel(1)
	reader.movePanel(1)
	if reader.panelIndex != 1 || strings.ContainsAny(reader.panelLines[2], "\x1b\n") {
		t.Fatal("panel selection overflowed or control text remained")
	}
	if !reader.closePanel() {
		t.Fatal("panel did not close")
	}
	reader.redraw(reader.renderBuf, reader.renderCursor)
	if reader.drawnRows != 1 || reader.cursorRow != 0 || string(reader.renderBuf) != "draft" || reader.renderCursor != 2 {
		t.Fatal("closing panel changed the draft or retained panel rows")
	}
	assertNoBareLF(t, restore())
}

func TestAgentPanelHotkeyAndArrowsPreserveTypedInput(t *testing.T) {
	restore := captureStdout(t)
	reader := typed("draft\x1bm\x1b[B\x1bm\r")
	withActiveReader(t, reader)
	consoleMu.Lock()
	streamingActive = false
	consoleMu.Unlock()
	reader.SetPanelSource(func(index int) ([]string, int) { return []string{"Agent Map"}, 2 })
	line, err := reader.editLine()
	if err != nil || line != "draft" || reader.panelOpen || reader.panelIndex != 0 {
		t.Fatalf("hotkey changed input: line=%q error=%v open=%v selected=%d", line, err, reader.panelOpen, reader.panelIndex)
	}
	assertNoBareLF(t, restore())
}

func TestAgentPanelOpeningKeepsInputFocus(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	reader := typed("")
	reader.history = []string{"previous task"}
	reader.histIdx = 1
	reader.SetPanelSource(func(int) ([]string, int) { return []string{"Agent Map"}, 2 })
	reader.togglePanel()
	buf, cursor := []rune("draft"), 5
	reader.applyKey(&buf, &cursor, "", 'A')
	if string(buf) != "previous task" || reader.panelIndex != 0 {
		t.Fatalf("opening panel stole history navigation: input=%q selected=%d", string(buf), reader.panelIndex)
	}
}

func TestInteractionStatusAndPanelFocus(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	reader := typed("")
	reader.SetInteractionState("执行中")
	reader.SetInputStatus("image.png")
	reader.SetPanelSource(func(int) ([]string, int) { return nil, 2 })
	reader.togglePanel()
	if !reader.togglePanelFocus() || !reader.panelHasFocus() {
		t.Fatal("could not focus the Agent panel")
	}
	buf, cursor := []rune("draft"), 5
	reader.applyKey(&buf, &cursor, "", 'B')
	if reader.panelIndex != 1 || string(buf) != "draft" {
		t.Fatal("panel focus changed the draft instead of selection")
	}
	consoleMu.Lock()
	status := reader.statusLineLocked()
	consoleMu.Unlock()
	for _, text := range []string{"正在处理任务", "正在查看 Agent 面板", "image.png"} {
		if !strings.Contains(status, text) {
			t.Fatalf("status %q lacks %q", status, text)
		}
	}
	reader.applyKey(&buf, &cursor, "", 'Z')
	if reader.panelHasFocus() {
		t.Fatal("Shift+Tab did not return to input focus")
	}
}

func TestInteractionStatusUsesPlainLanguage(t *testing.T) {
	for _, test := range []struct {
		name, state, attachment, want string
		panel, search                 bool
	}{
		{name: "idle", state: "空闲", want: ""},
		{name: "running", state: "执行中", want: "正在处理任务"},
		{name: "stopped", state: "已停止", want: "任务已停止"},
		{name: "panel", state: "空闲", panel: true, want: "正在查看 Agent 面板"},
		{name: "history", state: "空闲", search: true, want: "选择历史会话"},
		{name: "attachment", state: "空闲", attachment: "image.png", want: "image.png"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &LineReader{interactionState: test.state, inputStatus: test.attachment, panelOpen: test.panel, panelFocused: test.panel}
			if test.search {
				reader.choices, reader.choiceSearch = []Choice{{Label: "task"}}, true
			}
			consoleMu.Lock()
			wasStreaming := streamingActive
			streamingActive = false
			status := reader.statusLineLocked()
			streamingActive = wasStreaming
			consoleMu.Unlock()
			if status != test.want {
				t.Fatalf("status = %q, want %q", status, test.want)
			}
		})
	}
}

func TestChoiceMenuSelectionSearchAndCancellation(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	reader := typed("")
	reader.redraw([]rune("/h"), 2)
	reader.showInlineSuggestions([]string{"/help\t帮助", "/history\t历史会话"}, 0)
	buf, cursor := []rune("/h"), 2
	reader.applyKey(&buf, &cursor, "", 'B')
	accepted, execute := reader.acceptChoice(&buf, &cursor)
	if !accepted || execute || string(buf) != "/history" {
		t.Fatal("completion did not fill the selected command without executing it")
	}
	reader.ShowChoices("会话", []Choice{
		{Value: "/resume one", Label: "修复图片", Preview: "图片识别失败"},
		{Value: "/resume two", Label: "优化输入", Description: "今天", Preview: "输入框布局"},
	})
	reader.redraw([]rune("输入"), 2)
	consoleMu.Lock()
	lines := reader.displayRowsLocked()
	consoleMu.Unlock()
	if len(lines) != 3 || !strings.Contains(lines[2], "输入框布局") {
		t.Fatalf("filtered preview = %v", lines)
	}
	buf, cursor = []rune("输入"), 2
	accepted, execute = reader.acceptChoice(&buf, &cursor)
	if !accepted || !execute || string(buf) != "/resume two" {
		t.Fatal("history selection did not submit the selected session command")
	}
	reader.ShowChoices("会话", []Choice{{Value: "/resume one", Label: "修复图片"}})
	reader.redraw([]rune("无匹配"), 3)
	if handled, execute := reader.acceptChoice(&buf, &cursor); !handled || execute || !reader.choiceSearch {
		t.Fatal("unmatched history search did not keep Enter inside the picker")
	}
	if !reader.dismissChoices(&buf, &cursor) || len(buf) != 0 {
		t.Fatal("history cancellation did not clear the search")
	}
}

func TestChoiceMenuEOFDoesNotSubmitSearch(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	reader := typed("search query")
	reader.ShowChoices("会话", []Choice{{Value: "/resume one", Label: "修复图片"}})
	line, err := reader.editLine()
	if line != "" || !errors.Is(err, ErrExit) {
		t.Fatalf("EOF submitted a search as a task: %q, %v", line, err)
	}
}

func TestApprovalPreviewOwnsSelectionAndClearsAfterAnswer(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	reader := typed("")
	reader.SetInteractionState("执行中")
	reader.panelLines = []string{"Agent Map"}
	external := make(chan ExternalAnswer, 1)
	external <- ExternalAnswer{Answer: "n"}
	var preview []string
	var status string
	answer, ok := AskWith(AskSpec{
		Title: "等待授权", Preview: []string{"操作: git status\x1b[2J\n", "[y] 允许 [n] 拒绝"},
		Accepts: func(line string) bool { return line == "n" },
		Timeout: time.Second,
		External: func() (<-chan ExternalAnswer, func()) {
			consoleMu.Lock()
			preview = reader.displayRowsLocked()
			status = reader.statusLineLocked()
			consoleMu.Unlock()
			if reader.ShowChoices("会话", []Choice{{Value: "/resume one", Label: "one"}}) {
				t.Error("history picker replaced a pending approval")
			}
			return external, nil
		},
	})
	if !ok || answer != "n" || len(preview) != 2 || strings.ContainsAny(preview[0], "\x1b\n") {
		t.Fatalf("approval preview or answer incorrect: %v, %q, %v", preview, answer, ok)
	}
	if status != "等待你确认授权" {
		t.Fatalf("approval state not explicit: %q", status)
	}
	consoleMu.Lock()
	remaining := reader.displayRowsLocked()
	consoleMu.Unlock()
	if len(remaining) != 1 || remaining[0] != "Agent Map" {
		t.Fatalf("approval preview survived its answer: %v", remaining)
	}
}

func TestInteractionStatusDoesNotRedrawUnchangedState(t *testing.T) {
	restore := captureStdout(t)
	reader := typed("")
	withActiveReader(t, reader)
	consoleMu.Lock()
	streamingActive = false
	consoleMu.Unlock()
	reader.redraw(nil, 0)
	for range 10 {
		reader.SetInteractionState("空闲")
	}
	for range 10 {
		reader.SetInteractionState("执行中")
	}
	output := restore()
	if strings.Count(output, "正在处理任务") != 1 || strings.Contains(output, "空闲") {
		t.Fatalf("unchanged input state was repeatedly redrawn: %q", output)
	}
	if !strings.Contains(output, reader.placeholder) {
		t.Fatal("status row hid the idle input placeholder")
	}
}

func TestChoiceMenuKeepsSelectionInShortViewport(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	reader := typed("")
	reader.ShowChoices("会话", []Choice{
		{Label: "one"}, {Label: "two"}, {Label: "three"},
		{Label: "four"}, {Label: "five"}, {Label: "six"},
	})
	for range 5 {
		reader.moveChoice(1)
	}
	consoleMu.Lock()
	rows := reader.displayRowsLocked()[:reader.panelRows(8)]
	consoleMu.Unlock()
	if !strings.Contains(strings.Join(rows, "\n"), "> six") {
		t.Fatalf("short viewport lost the selected item: %v", rows)
	}
}

func TestChoiceMenuUnavailableInFallbackInput(t *testing.T) {
	reader := typed("")
	reader.fallbackReader = reader.rawReader
	if reader.ShowChoices("会话", []Choice{{Value: "/resume one", Label: "one"}}) {
		t.Fatal("fallback input opened an unusable picker")
	}
}

func TestChoiceKeyboardAcceptanceDoesNotPrematurelySubmit(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	reader := New(func(input string) []string {
		if input == "/he" {
			return []string{"/help\t帮助", "/hello\t问候"}
		}
		return nil
	})
	reader.rawReader = bufio.NewReader(io.MultiReader(
		strings.NewReader("/he"), strings.NewReader("\r"),
		strings.NewReader(" tail"), strings.NewReader("\r"),
	))
	consoleMu.Lock()
	streamingActive = false
	consoleMu.Unlock()
	line, err := reader.editLine()
	if err != nil || line != "/help tail" {
		t.Fatalf("first Enter submitted instead of accepting a candidate: %q, %v", line, err)
	}
}

func TestChoiceKeyboardSearchAndArrowResumesSelectedSession(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	reader := typed("")
	reader.rawReader = bufio.NewReader(io.MultiReader(
		strings.NewReader("输入"), strings.NewReader("\x1b[B"), strings.NewReader("\r"),
	))
	consoleMu.Lock()
	streamingActive = false
	consoleMu.Unlock()
	reader.ShowChoices("会话", []Choice{
		{Value: "/resume one", Label: "输入一"},
		{Value: "/resume two", Label: "输入二"},
		{Value: "/resume other", Label: "其他任务"},
	})
	line, err := reader.editLine()
	if err != nil || line != "/resume two" {
		t.Fatalf("keyboard selection did not resume the filtered session: %q, %v", line, err)
	}
}

func TestAgentPanelEscapeClosesWithoutInterrupt(t *testing.T) {
	restore := captureStdout(t)
	reader := typed("draft\x1bm\x1b")
	withActiveReader(t, reader)
	consoleMu.Lock()
	streamingActive = false
	consoleMu.Unlock()
	reader.SetPanelSource(func(index int) ([]string, int) { return []string{"Agent Map"}, 1 })
	_, err := reader.editLine()
	if errors.Is(err, ErrEscape) || reader.panelOpen || string(reader.renderBuf) != "draft" {
		t.Fatalf("Escape interrupted or cleared draft: error=%v open=%v draft=%q", err, reader.panelOpen, string(reader.renderBuf))
	}
	assertNoBareLF(t, restore())
}

func TestAgentPanelClosedDoesNotReadSnapshot(t *testing.T) {
	restore := captureStdout(t)
	reader := typed("")
	reads := 0
	reader.SetPanelSource(func(index int) ([]string, int) {
		reads++
		return []string{"Agent Map"}, 1
	})
	for range 10 {
		reader.RefreshPanel()
	}
	if reads != 0 {
		t.Fatal("closed panel kept reading agent snapshots")
	}
	reader.togglePanel()
	if reads != 1 {
		t.Fatal("opening panel did not load the current snapshot")
	}
	reader.closePanel()
	reader.RefreshPanel()
	if reads != 1 {
		t.Fatal("closing panel did not stop snapshot reads")
	}
	_ = restore()
}

func TestAgentPanelAutomaticRefreshIsThrottled(t *testing.T) {
	reader := typed("")
	reads := 0
	reader.SetPanelSource(func(index int) ([]string, int) {
		reads++
		return []string{"Agent Map"}, 2
	})
	reader.togglePanel()
	for range 10 {
		reader.RefreshPanel()
	}
	if reads != 1 {
		t.Fatalf("snapshot reads = %d, want 1 before the refresh interval", reads)
	}
	reader.movePanel(1)
	if reads != 2 || reader.panelIndex != 1 {
		t.Fatal("selection did not immediately refresh the panel")
	}
	reader.closePanel()
	reader.togglePanel()
	if reads != 3 {
		t.Fatal("reopening did not immediately refresh the panel")
	}
	lastRefresh := reader.panelRefreshed
	lrBefore := lastRefresh.Add(panelRefreshInterval - 1)
	reader.refreshPanel(lrBefore, false)
	if reads != 3 {
		t.Fatal("automatic refresh ran before the interval elapsed")
	}
	reader.refreshPanel(lastRefresh.Add(panelRefreshInterval), false)
	if reads != 4 {
		t.Fatal("automatic refresh did not resume when the interval elapsed")
	}
	reader.closePanel()
	reader.refreshPanel(lastRefresh.Add(2*panelRefreshInterval), false)
	if reads != 4 {
		t.Fatal("closed panel kept refreshing after the interval elapsed")
	}
}

func TestImageInputRemoveOnlyOnEmptyDraft(t *testing.T) {
	for _, tc := range []struct {
		keys  string
		calls int
	}{
		{"\x1b\x7f\r", 1},
		{"word\x1b\x7f\r", 0},
	} {
		restore := captureStdout(t)
		reader := typed(tc.keys)
		calls := 0
		reader.SetImageInputHooks(nil, nil, func() { calls++ })
		line, err := reader.editLine()
		restore()
		if err != nil || line != "" || calls != tc.calls {
			t.Fatalf("line=%q err=%v removal calls=%d", line, err, calls)
		}
	}
}

func TestPinnedInputStatusPreservesStream(t *testing.T) {
	restore := captureStdout(t)
	reader := New(nil)
	withPinned(t, reader, 24, 50)
	reader.SetInputStatus("pending image " + strings.Repeat("x", 100))
	reader.redraw([]rune("draft"), 2)
	out := restore()
	if !strings.Contains(out, "\x1b[49;1H\x1b[2K\x1b[2mpending image ") || strings.Contains(out, strings.Repeat("x", 100)) {
		t.Fatal("pinned attachment status missing or not truncated")
	}
	if !strings.HasPrefix(out, "\x1b7") || !strings.HasSuffix(out, "\x1b8") {
		t.Fatal("status changed the stream cursor")
	}
}

// Consoles without bracketed paste (conhost) deliver a paste as plain key
// input. A person cannot type Enter and further keys in the same read, so a
// newline with more input already waiting behind it is part of a paste.
func TestUnbracketedPasteIsOneMessage(t *testing.T) {
	restore := captureStdout(t)
	got, err := typed("line one\r\nline two\rline three\r").editLine()
	out := restore()
	if err != nil {
		t.Fatal(err)
	}
	if want := "line one\nline two\nline three"; got != want {
		t.Fatalf("pasted text = %q, want %q", got, want)
	}
	assertNoBareLF(t, out)
}

// A closed stdin (terminal gone, pipe ended) returned io.EOF, which the REPL
// loop treats as "reinitialise and read again" — forever, at full CPU,
// printing the same error line.
func TestEndOfInputEndsTheSession(t *testing.T) {
	if _, err := editOnce(t, ""); !errors.Is(err, ErrExit) {
		t.Fatalf("err = %v, want ErrExit", err)
	}
	line, err := editOnce(t, "last words")
	if err != nil || line != "last words" {
		t.Fatalf("got (%q, %v), want the unterminated line back", line, err)
	}
}

// inputDisplayWindow advanced its window start one rune at a time and
// re-measured the whole prefix at each step, so every keystroke cost O(n²) in
// the input length: pasting a long log froze the editor.
func TestInputDisplayWindowIsLinearInInputLength(t *testing.T) {
	buf := []rune(strings.Repeat("汉字abc", 40000)) // 200k runes
	done := make(chan struct{})
	go func() {
		inputDisplayWindow(buf, len(buf), 80)
		inputDisplayWindow(buf, len(buf)/2, 80)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("inputDisplayWindow did not finish in 5s on a 200k-rune input")
	}
}

func TestLongPasteIsAcceptedQuickly(t *testing.T) {
	text := strings.Repeat("0123456789", 5000) // 50k runes, no newline
	done := make(chan string, 1)
	restore := captureStdout(t)
	go func() {
		line, _ := typed("\x1b[200~" + text + "\x1b[201~\r").editLine()
		done <- line
	}()
	select {
	case got := <-done:
		restore()
		if got != text {
			t.Fatalf("pasted %d runes, got %d back", len(text), len(got))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a 50k-rune paste did not finish in 10s")
	}
}

// CJK Extension B and later planes are double width. They were measured as
// one column, so the visible window overflowed the terminal, soft-wrapped,
// and the next redraw left a ghost row behind.
func TestInputDisplayWindowFitsSupplementaryIdeographs(t *testing.T) {
	buf := []rune(strings.Repeat("\U00020021\U0002A700", 30))
	disp, _, _, _ := inputDisplayWindow(buf, len(buf), 20)
	if w := textutil.Width(string(disp)); w > 20 {
		t.Fatalf("visible window is %d columns, budget 20: %q", w, string(disp))
	}
}
