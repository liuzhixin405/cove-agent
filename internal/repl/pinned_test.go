package repl

import (
	"strings"
	"testing"
)

// The scroll region ends one row above the input row, and the stream picks
// up right after the previous output: on the next row (after one blank
// separator row when asked for), scrolling first when that row would be the
// input row itself. The cursor row comes from the terminal, not from a count
// of rows drawn.
func TestPinSequencePlacesStreamAboveTheInputRow(t *testing.T) {
	// Two rows are reserved (rule + input), so on a 50-row terminal the
	// region is 1..48 and the stream must end up on row 48 at the latest.
	cases := []struct {
		row, col, h int
		separator   bool
		want        string
		start       int
	}{
		{10, 1, 50, true, "\r\n\x1b[1;48r\x1b[11;1H", 11},
		{10, 7, 50, true, "\r\n\x1b[1;48r\x1b[11;1H", 11},
		{47, 1, 50, true, "\r\n\x1b[1;48r\x1b[48;1H", 48},
		// The separator row would be row 49: scroll once from the last row.
		{48, 1, 50, true, "\r\n\x1b[50;1H\r\n\x1b[1;48r\x1b[48;1H", 48},
		{49, 1, 50, true, "\r\n\r\n\r\n\x1b[1;48r\x1b[48;1H", 48},
		{50, 1, 50, true, "\r\n\r\n\r\n\x1b[1;48r\x1b[48;1H", 48},
		{10, 1, 50, false, "\x1b[1;48r\x1b[10;1H", 10},
		// Pinning while the stream is mid-line keeps the column, scrolled or not.
		{10, 23, 50, false, "\x1b[1;48r\x1b[10;23H", 10},
		{48, 5, 50, false, "\x1b[1;48r\x1b[48;5H", 48},
		{49, 5, 50, false, "\x1b[50;1H\r\n\x1b[1;48r\x1b[48;5H", 48},
		{50, 5, 50, false, "\r\n\r\n\x1b[1;48r\x1b[48;5H", 48},
	}
	for _, c := range cases {
		got, start := pinSequence(cursorPos{c.row, c.col}, c.h, c.separator)
		if got != c.want || start != c.start {
			t.Errorf("pinSequence(%d;%d, %d, %v) = %q, %d; want %q, %d", c.row, c.col, c.h, c.separator, got, start, c.want, c.start)
		}
	}
}

// Leaving pinned mode clears the input row and resets the margins without
// moving the stream cursor: DECSTBM homes the cursor, so it is bracketed by
// DECSC/DECRC.
func TestUnpinSequenceClearsTheInputRowAndResetsMargins(t *testing.T) {
	if got, want := unpinSequence(50), "\x1b7\x1b[49;1H\x1b[2K\x1b[50;1H\x1b[2K\x1b[r\x1b8"; got != want {
		t.Errorf("unpinSequence = %q, want %q", got, want)
	}
}

// The pinned row has no real cursor (that stays with the stream), so the
// caret is drawn in reverse video; an empty buffer shows the running hint.
func TestPinnedInputLineShowsCaretAndPlaceholder(t *testing.T) {
	empty := pinnedInputLine("> ", 2, nil, 0, 80, "type ahead")
	// A cell between the caret and the hint, and the hint dimmed, so it does
	// not read as typed text.
	if !strings.Contains(empty, "\x1b[7m \x1b[27m \x1b[2;90mtype ahead") {
		t.Errorf("empty line lacks caret, gap or dimmed hint: %q", empty)
	}
	mid := pinnedInputLine("> ", 2, []rune("abc"), 1, 80, "hint")
	if want := "> a\x1b[7mb\x1b[27mc"; !strings.Contains(mid, want) {
		t.Errorf("caret not on the cursor rune: %q", mid)
	}
	if strings.Contains(mid, "hint") {
		t.Errorf("hint shown next to typed text: %q", mid)
	}
	end := pinnedInputLine("> ", 2, []rune("abc"), 3, 80, "hint")
	if want := "> abc\x1b[7m \x1b[27m"; !strings.Contains(end, want) {
		t.Errorf("caret at end of text missing: %q", end)
	}
	// A narrow window keeps the row from wrapping onto the stream: prompt +
	// text + caret must stay under the width.
	long := pinnedInputLine("> ", 2, []rune(strings.Repeat("x", 100)), 100, 20, "hint")
	if w := visibleCells(long); w >= 20 {
		t.Errorf("pinned row is %d cells wide in a 20-column window: %q", w, long)
	}
}

// visibleCells counts the display cells of s, skipping escape sequences.
func visibleCells(s string) int {
	n := 0
	inAnsi := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inAnsi = true
		case inAnsi:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inAnsi = false
			}
		default:
			n += runeCellWidth(r)
		}
	}
	return n
}

// The terminal's answer to a cursor position query arrives on stdin as
// CSI row ; col R. It is data for queryCursorRow, never text in the line.
func TestCursorPositionReportIsNotTypedAndReachesTheQuery(t *testing.T) {
	select {
	case <-cprCh:
	default:
	}
	got, err := editOnce(t, "\x1b[12;7Rab\r")
	if err != nil || got != "ab" {
		t.Fatalf("line = %q, %v; want \"ab\"", got, err)
	}
	select {
	case pos := <-cprCh:
		if pos != (cursorPos{12, 7}) {
			t.Errorf("reported position = %+v, want 12;7", pos)
		}
	default:
		t.Fatal("the report did not reach the query channel")
	}
	// F3 as SS3 R carries no parameters and is not a report.
	got, err = editOnce(t, "a\x1bORb\r")
	if err != nil || got != "ab" {
		t.Fatalf("SS3 R: line = %q, %v", got, err)
	}
	select {
	case pos := <-cprCh:
		t.Errorf("SS3 R was taken for a cursor report: %+v", pos)
	default:
	}
}

func TestParseCursorReport(t *testing.T) {
	for in, want := range map[string]cursorPos{"12;7": {12, 7}, "3": {3, 1}, "40;120": {40, 120}} {
		if got, ok := parseCursorReport(in); !ok || got != want {
			t.Errorf("parseCursorReport(%q) = %+v, %v; want %+v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", ";5", "0;1", "x;y", "1;5;9x"} {
		if got, ok := parseCursorReport(in); ok && in != "1;5;9x" {
			t.Errorf("parseCursorReport(%q) = %+v, true; want false", in, got)
		}
	}
}

// withPinned puts the console in the state BeginOutput leaves it in once the
// terminal has answered: streaming, with the input row pinned to row h.
func withPinned(t *testing.T, lr *LineReader, w, h int) {
	t.Helper()
	withActiveReader(t, lr)
	consoleMu.Lock()
	streamingActive = true
	pinned, pinRows, pinCols = true, h, w
	pinPanelRows = 0
	consoleMu.Unlock()
	t.Cleanup(func() {
		consoleMu.Lock()
		pinned, pinRows, pinCols = false, 0, 0
		pinPanelRows = 0
		consoleMu.Unlock()
	})
}

// While pinned, the input row is drawn on the last terminal row between a
// cursor save and restore, so the stream's cursor is where it was.
func TestPinnedRedrawSavesAndRestoresTheCursor(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	withPinned(t, lr, 80, 50)
	lr.redraw([]rune("hi"), 2)
	out := restore()
	if !strings.HasPrefix(out, "\x1b7") || !strings.HasSuffix(out, "\x1b8") {
		t.Fatalf("draw is not bracketed by DECSC/DECRC: %q", out)
	}
	if !strings.Contains(out, "\x1b[50;1H\x1b[2K") {
		t.Errorf("draw does not target the last row: %q", out)
	}
	if !strings.Contains(out, "\x1b[49;1H\x1b[2K\x1b[2m───") {
		t.Errorf("no rule on the row above the input: %q", out)
	}
	if !strings.Contains(out, "hi\x1b[7m \x1b[27m") {
		t.Errorf("typed text missing from the pinned row: %q", out)
	}
	if strings.Contains(out, "\r\x1b[2K") {
		t.Errorf("draw erased the stream's row: %q", out)
	}
	if strings.Contains(out, "已排队") {
		t.Errorf("queue note shown with nothing queued: %q", out)
	}
}

func TestAgentPanelPinnedRowsAndRelease(t *testing.T) {
	restore := captureStdout(t)
	reader := New(nil)
	reader.panelLines = []string{"Agent Map", "agent-one", "details"}
	withPinned(t, reader, 80, 50)
	consoleMu.Lock()
	pinPanelRows = 3
	consoleMu.Unlock()
	reader.redraw([]rune("draft"), 2)
	consoleMu.Lock()
	unpinLocked()
	consoleMu.Unlock()
	output := restore()
	if !strings.Contains(output, "\x1b[46;1H\x1b[2KAgent Map") || !strings.Contains(output, unpinSequenceReserved(50, 5)) {
		t.Fatalf("panel rows not reserved or cleared: %q", output)
	}
	if string(reader.renderBuf) != "draft" || reader.renderCursor != 2 {
		t.Fatal("pinned panel changed draft or cursor")
	}
	assertNoBareLF(t, output)
}

func TestAgentPanelSmallTerminalLeavesStreamRoom(t *testing.T) {
	reader := New(nil)
	reader.panelLines = make([]string, panelMaxRows)
	if reader.panelRows(8) != 3 || reader.panelRows(4) != 0 {
		t.Fatal("panel did not respect terminal height")
	}
	sequence, row := pinSequenceReserved(cursorPos{row: 8, col: 1}, 8, false, 5)
	if row != 3 || !strings.Contains(sequence, "\x1b[1;3r") {
		t.Fatalf("panel left no safe stream region: %q, %d", sequence, row)
	}
}

func TestPinnedAgentHintVisibility(t *testing.T) {
	for _, test := range []struct {
		name      string
		available bool
		open      bool
		wantHint  bool
	}{
		{name: "closed", available: true, wantHint: true},
		{name: "open", available: true, open: true},
		{name: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			restore := captureStdout(t)
			reader := New(nil)
			if test.available {
				reader.SetPanelSource(func(int) ([]string, int) { return nil, 0 })
			}
			reader.panelOpen = test.open
			withPinned(t, reader, 80, 50)
			reader.redraw(nil, 0)
			output := restore()
			if got := strings.Contains(output, "Alt+M 查看 Agent"); got != test.wantHint {
				t.Fatalf("Agent hint visible = %v, want %v: %q", got, test.wantHint, output)
			}
			if !strings.Contains(output, "输入指引，回车送入当前任务") {
				t.Fatalf("running input guidance missing: %q", output)
			}
			if strings.Contains(reader.placeholder, "Alt+M") {
				t.Fatal("Agent hint was added to the idle placeholder")
			}
		})
	}
}

// With tasks waiting behind the running one, their count sits at the right
// end of the input row.
func TestPinnedRedrawShowsTheQueueCount(t *testing.T) {
	SetQueuedCount(2)
	t.Cleanup(func() { SetQueuedCount(0) })
	restore := captureStdout(t)
	lr := New(nil)
	withPinned(t, lr, 80, 50)
	lr.redraw(nil, 0)
	out := restore()
	// "已排队 2 条" is 11 cells wide (four CJK cells of two, three of one), so
	// it starts at column 69 of 80 and ends one cell before the last column.
	if !strings.Contains(out, "\x1b[50;69H\x1b[2m已排队 2 条\x1b[0m") {
		t.Errorf("queue note missing or misplaced: %q", out)
	}
}

// Guidance typed while a task runs is steered into that task rather than
// queued; until the next model call consumes it the pinned row says how many
// such lines are waiting, in the same corner as the queue count, and both
// notes share the corner when both apply.
func TestPinnedRedrawShowsTheSteerCount(t *testing.T) {
	SetSteerCount(1)
	t.Cleanup(func() { SetSteerCount(0); SetQueuedCount(0) })
	restore := captureStdout(t)
	lr := New(nil)
	// 120 columns: the placeholder plus both notes would not fit in 80, and
	// a note that does not fit is (rightly) left out.
	withPinned(t, lr, 120, 50)
	lr.redraw(nil, 0)
	out := restore()
	// "已插入 1 条指引" is 15 cells wide (six CJK cells of two, three of one),
	// so it starts at column 105 of 120.
	if !strings.Contains(out, "\x1b[50;105H\x1b[2m已插入 1 条指引\x1b[0m") {
		t.Errorf("steer note missing or misplaced: %q", out)
	}

	SetQueuedCount(2)
	restore = captureStdout(t)
	lr.redraw(nil, 0)
	out = restore()
	// Both notes: 15 + 4 (" · ", the middle dot being East Asian Ambiguous
	// and measured 2 like everywhere else) + 11 = 30 cells, starting at
	// column 90. Measured 1, a CJK terminal drew the row one column over its
	// width and wrapped it.
	if !strings.Contains(out, "\x1b[50;90H\x1b[2m已插入 1 条指引 · 已排队 2 条\x1b[0m") {
		t.Errorf("combined note missing or misplaced: %q", out)
	}
}

// Enter while pinned echoes the submitted line into the stream, on its own
// row, so the transcript shows what was queued; blind type-ahead used to
// leave no trace on screen at all.
func TestEnterWhilePinnedEchoesIntoTheStream(t *testing.T) {
	restore := captureStdout(t)
	lr := typed("hi\r")
	withPinned(t, lr, 80, 50)
	consoleMu.Lock()
	streamMidLine = true // the model left a partial line
	consoleMu.Unlock()
	line, err := lr.editLine()
	out := restore()
	if err != nil || line != "hi" {
		t.Fatalf("line = %q, %v", line, err)
	}
	i := strings.Index(out, "\x1b8") // the last pinned draw's restore
	if i < 0 {
		t.Fatalf("no pinned draw: %q", out)
	}
	echo := out[strings.LastIndex(out, "\x1b8")+2:]
	if !strings.HasPrefix(echo, "\r\n") {
		t.Errorf("echo did not break the partial stream line first: %q", echo)
	}
	if !strings.Contains(echo, "hi\r\n") {
		t.Errorf("submitted line not echoed into the stream: %q", echo)
	}
}

// Enter on an empty pinned row is a no-op for the main loop, so it must leave
// no trace in the stream either: every empty Enter used to print a bare
// "⚡ ❯" row into the model's output, cutting its sentence in two.
func TestEmptyEnterWhilePinnedEchoesNothing(t *testing.T) {
	restore := captureStdout(t)
	lr := typed("\r")
	withPinned(t, lr, 80, 50)
	consoleMu.Lock()
	streamMidLine = true
	consoleMu.Unlock()
	line, err := lr.editLine()
	out := restore()
	if err != nil || line != "" {
		t.Fatalf("line = %q, %v", line, err)
	}
	if strings.Contains(out, PromptRunning()+"\r\n") || strings.Contains(out, "❯ \r\n") {
		t.Errorf("empty Enter echoed a prompt row into the stream: %q", out)
	}
	// The partial stream line was not broken either.
	echo := out[strings.LastIndex(out, "\x1b8")+2:]
	if strings.Contains(echo, "\r\n") {
		t.Errorf("empty Enter broke the stream's line: %q", echo)
	}
}

// Streaming without a pinned row (the terminal gave no cursor report) keeps
// the old contract: nothing is echoed, nothing is drawn.
func TestEnterWhileStreamingUnpinnedStaysSilent(t *testing.T) {
	restore := captureStdout(t)
	lr := typed("hi\r")
	withActiveReader(t, lr)
	consoleMu.Lock()
	streamingActive = true
	consoleMu.Unlock()
	_, _ = lr.editLine()
	if out := restore(); strings.Contains(out, "hi") {
		t.Errorf("unpinned streaming echoed the line: %q", out)
	}
}

// Ending the turn unpins before the idle input line is redrawn in place.
func TestEndOutputUnpinsBeforeRedrawing(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	withPinned(t, lr, 80, 50)
	EndOutput()
	out := restore()
	unpin := strings.Index(out, unpinSequence(50))
	redraw := strings.Index(out, "\r\x1b[2K")
	if unpin < 0 || redraw < 0 || unpin > redraw {
		t.Fatalf("unpin %d, redraw %d in %q", unpin, redraw, out)
	}
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if pinned {
		t.Error("still pinned after EndOutput")
	}
}

// A permission prompt in the middle of a pinned stream unpins so the
// question and its answer line render in the flow, and pinning resumes
// only through the terminal's cursor report (none here, so plain streaming).
func TestPromptInputUnpins(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	withPinned(t, lr, 80, 50)
	BeginPromptInput()
	consoleMu.Lock()
	stillPinned := pinned
	consoleMu.Unlock()
	EndPromptInput()
	out := restore()
	if stillPinned {
		t.Error("BeginPromptInput left the row pinned")
	}
	if !strings.Contains(out, unpinSequence(50)) {
		t.Errorf("no unpin sequence: %q", out)
	}
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if !streamingActive {
		t.Error("EndPromptInput did not resume streaming")
	}
}

// Changing the guidance count redraws the pinned row at once, so "已插入 1
// 条指引" disappears when the model consumes it instead of waiting for the
// next keystroke.
func TestSetSteerCountRedrawsThePinnedRow(t *testing.T) {
	t.Cleanup(func() { SetSteerCount(0) })
	lr := New(nil)
	withPinned(t, lr, 80, 50)
	restore := captureStdout(t)
	SetSteerCount(1)
	out := restore()
	if !strings.Contains(out, "已插入 1 条指引") {
		t.Fatalf("pinned row not redrawn on SetSteerCount: %q", out)
	}
}

// A window that changed size while pinned invalidates the saved stream
// cursor: after the shrink it may sit in the reserved rows, where output
// neither scrolls nor stays visible. The row is released and pinned again
// from the terminal's real cursor position instead of only moving margins.
func TestResizeWhilePinnedReleasesAndRepins(t *testing.T) {
	restore := captureStdout(t)
	lr := New(nil)
	withPinned(t, lr, 80, 50)
	consoleMu.Lock()
	repinned := lr.repinIfResizedLocked(80, 40)
	stillPinned := pinned
	consoleMu.Unlock()
	// The repin is detached (it asks the key loop where the cursor is), so it
	// must be joined before this test takes os.Stdout back: -race caught the
	// pinning goroutine reading it while captureStdout's restore wrote it.
	waitForPin()
	out := restore()
	if !repinned || stillPinned {
		t.Fatalf("resize not handled: repinned=%v pinned=%v", repinned, stillPinned)
	}
	if !strings.Contains(out, unpinSequence(50)) {
		t.Errorf("old reserved rows not released: %q", out)
	}
	consoleMu.Lock()
	same := lr.repinIfResizedLocked(80, 40)
	consoleMu.Unlock()
	if same {
		t.Error("an unchanged size was treated as a resize")
	}
}
