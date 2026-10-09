package repl

import (
	"testing"
)

// "/" alone is the request for the quick command list; the candidate list
// used to fill in its first entry on Enter, so the list was unreachable
// without Esc first.
func TestSlashAloneSubmitsInsteadOfFillingTheFirstCandidate(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	reader := typed("")
	reader.redraw([]rune("/"), 1)
	reader.showInlineSuggestions([]string{"/acceptance\t验收", "/help\t帮助"}, 0)
	buf, cursor := []rune("/"), 1
	accepted, execute := reader.acceptChoice(&buf, &cursor)
	if accepted || execute || string(buf) != "/" {
		t.Fatalf("accepted=%v execute=%v buf=%q, want the bare slash submitted", accepted, execute, string(buf))
	}
}

// The multi-row editor is bounded by the terminal height: 12 input rows on
// a 12-row terminal scrolled the screen and left ghost rows.
func TestInputRowCapLeavesRoomOnShortTerminals(t *testing.T) {
	for h, want := range map[int]int{0: maxInputRows, 24: maxInputRows, 14: maxInputRows, 12: 10, 5: 3, 2: 1, 1: 1} {
		if got := inputRowCap(h); got != want {
			t.Fatalf("inputRowCap(%d) = %d, want %d", h, got, want)
		}
	}
	lr := &LineReader{promptWidth: 2}
	eleven := []rune("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl")
	if !lr.needsMultiRow(eleven, 80, 24) {
		t.Fatal("12 rows fit a 24-row terminal")
	}
	if lr.needsMultiRow(eleven, 80, 12) {
		t.Fatal("12 rows do not fit a 12-row terminal: the single-row window must be used")
	}
}

// While the input line is pinned, the candidate block keeps a fixed height:
// every change in its row count meant unpin, cursor query and repin.
func TestPinnedCandidateBlockHasFixedHeight(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	reader := typed("")
	reader.showInlineSuggestions([]string{"/help\t帮助", "/history\t历史会话"}, 0)
	if rows := len(reader.displayRowsLocked()); rows >= choiceBlockRows {
		t.Fatalf("unpinned candidate rows = %d, want the natural height", rows)
	}
	pinned = true
	defer func() { pinned = false }()
	if rows := len(reader.displayRowsLocked()); rows != choiceBlockRows {
		t.Fatalf("pinned candidate rows = %d, want %d", rows, choiceBlockRows)
	}
	reader.showInlineSuggestions([]string{"/help\t帮助"}, 0)
	if rows := len(reader.displayRowsLocked()); rows != choiceBlockRows {
		t.Fatalf("pinned candidate rows after narrowing = %d, want %d", rows, choiceBlockRows)
	}
}

// ← → Backspace Delete move and delete by grapheme cluster: on "a⚠️b" the
// cursor never lands between ⚠ and its U+FE0F, and Backspace removes both.
func TestCursorAndDeleteByGraphemeCluster(t *testing.T) {
	buf := []rune("a⚠️b")
	if got := clusterRight(buf, 1); got != 3 {
		t.Fatalf("clusterRight from 1 = %d, want 3", got)
	}
	if got := clusterLeft(buf, 3); got != 1 {
		t.Fatalf("clusterLeft from 3 = %d, want 1", got)
	}
	if got := clusterLeft(buf, 1); got != 0 {
		t.Fatalf("clusterLeft from 1 = %d, want 0", got)
	}
	if got := clusterRight(buf, 4); got != 4 {
		t.Fatalf("clusterRight at end = %d, want 4", got)
	}
	restore := captureStdout(t)
	defer restore()
	reader := typed("")
	line, cursor := []rune("a⚠️b"), 3
	start := clusterLeft(line, cursor)
	line = append(line[:start], line[cursor:]...)
	if string(line) != "ab" {
		t.Fatalf("backspace over the cluster left %q", string(line))
	}
	reader.redraw(line, start)
}

// pinAtCursor measures the reserved rows after setting pinned, so the fixed
// candidate block it reserves is the one drawPinnedLocked sees: measured
// unpinned (natural height) the first draw judged the block resized and
// repinned, which measured it again, forever.
func TestPinMeasuresTheCandidateBlockAsPinned(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	lr := typed("")
	withActiveReader(t, lr)
	lr.showInlineSuggestions([]string{"/help\t帮助", "/history\t历史会话"}, 0)
	consoleMu.Lock()
	streamingActive = true
	pinned = true
	pinPanelRows = lr.panelRows(50)
	pinRows, pinCols = 50, 80
	repinStreak = 0
	repinned := lr.repinIfResizedLocked(80, 50)
	reserved := pinPanelRows
	consoleMu.Unlock()
	t.Cleanup(func() {
		consoleMu.Lock()
		pinned, pinRows, pinCols, pinPanelRows, repinStreak = false, 0, 0, 0, 0
		consoleMu.Unlock()
	})
	if repinned {
		t.Fatal("a freshly measured pinned candidate block was judged resized")
	}
	if reserved != choiceBlockRows {
		t.Fatalf("reserved %d rows for the candidate block, want %d", reserved, choiceBlockRows)
	}
}
