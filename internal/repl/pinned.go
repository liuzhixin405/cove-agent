package repl

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// The pinned input row.
//
// While a task streams, the editable input line used to vanish: the stream
// owned the cursor and anything typed was buffered blind until Enter, with
// no hint that typing was possible at all. Now the last terminal row is
// reserved for the input line and the rows above it become the scroll region
// (DECSTBM), so the stream scrolls inside them and the terminal, not this
// program, keeps the row in place: wrapped lines, wide characters, spinner
// frames overwritten with \r and chunks without a trailing newline all stay
// the terminal's business. Earlier attempts pinned the row by counting the
// rows they had drawn and drifted as soon as the count was off by one.
//
// The one fact the program needs is the cursor row when the stream starts,
// asked from the terminal with a cursor position report (CSI 6 n); the reply
// arrives on stdin and the key loop hands it over (applyKey, case 'R').
// Every draw of the pinned row saves the cursor (DECSC), draws on the last
// row and restores it (DECRC), so the stream never notices. A terminal that
// does not answer, a stdout that is not a terminal, COVE_PIN_INPUT=0 and a
// window shorter than minPinRows fall back to the old behaviour: streaming
// with no input line on screen.

var (
	// pinned reports that the input row is on the last terminal row and the
	// scroll region ends above it; pinRows and pinCols are the terminal size
	// it was set up for. All three are guarded by consoleMu.
	pinned           bool
	pinRows, pinCols int
	pinPanelRows     int
	// cprCh carries a cursor position report (row, column) from the key loop
	// to queryCursorRow; cprMu lets one query at a time own the reply, so
	// two callers racing for it cannot take the other's answer for a
	// missing one.
	cprCh = make(chan cursorPos, 1)
	cprMu sync.Mutex
	// cprMisses counts the cursor queries in a row that went unanswered, and
	// cprRetryAt (unix nanoseconds) is when the next one may be tried, so
	// later turns do not each wait for a terminal that does not answer. One
	// miss used to disable pinning for the whole session, yet the usual
	// cause is a report that arrived where nobody relayed it (the history
	// search, a paste, between two ReadLine calls) rather than a terminal
	// without the feature. A miss now suspends pinning for cprRetryDelay;
	// only cprMaxMisses in a row give up for good, and any report, however
	// late, clears the count.
	cprMisses  atomic.Int32
	cprRetryAt atomic.Int64
)

const (
	cprMaxMisses  = 3
	cprRetryDelay = 30 * time.Second
)

// cprUsable reports whether a cursor query is worth sending now.
func cprUsable() bool {
	return cprMisses.Load() < cprMaxMisses && time.Now().UnixNano() >= cprRetryAt.Load()
}

// noteCPRMiss records an unanswered query.
func noteCPRMiss() {
	cprMisses.Add(1)
	cprRetryAt.Store(time.Now().Add(cprRetryDelay).UnixNano())
}

// resetCPRState forgets earlier misses: the terminal has shown it answers.
func resetCPRState() {
	cprMisses.Store(0)
	cprRetryAt.Store(0)
}

// deliverCursorReport hands the parameters of a CSI ... R sequence to the
// pending cursor query and reports whether they were a cursor report. Every
// reader of the raw input that consumes CSI sequences must pass R through
// here: the key loop, the history search and the bracketed paste.
func deliverCursorReport(params string) bool {
	pos, ok := parseCursorReport(params)
	if !ok {
		return false
	}
	resetCPRState()
	select {
	case cprCh <- pos:
	default:
	}
	return true
}

// cursorPos is a 1-based terminal cursor position.
type cursorPos struct{ row, col int }

const (
	// pinnedReserve is how many bottom rows the pinned mode keeps out of the
	// scroll region: a rule that separates the stream from the input, and
	// the input row itself.
	pinnedReserve = 2
	// minPinRows is the shortest window worth splitting into a scroll region
	// and the reserved rows.
	minPinRows = 8
	// cprTimeout bounds the wait for the terminal's cursor report.
	cprTimeout = 300 * time.Millisecond
	// pinnedPlaceholder is shown on the empty pinned row: the hint that
	// typing during a task is possible and what happens to the text.
	pinnedPlaceholder = "输入指引，回车送入当前任务"
)

// queuedCount is the number of tasks waiting behind the running one and
// steerCount the number of guidance lines the running task has not consumed
// yet; both are shown at the right end of the pinned row, and the task
// runner keeps them current.
var (
	queuedCount atomic.Int32
	steerCount  atomic.Int32
)

// SetQueuedCount records how many tasks are queued behind the running one
// and redraws the pinned row so the note changes at once.
func SetQueuedCount(n int) {
	queuedCount.Store(counterFrom(n))
	redrawPinned()
}

// SetSteerCount records how many guidance lines were steered into the running
// task and await its next model call, and redraws the pinned row: the note
// used to linger until the next keystroke after the model consumed them.
func SetSteerCount(n int) {
	steerCount.Store(counterFrom(n))
	redrawPinned()
}

// counterFrom narrows a caller's count to the int32 the pinned row's atomics
// hold. The values come from a task list and from keypresses, so saturation is
// theoretical — the clamp is here because the conversion is otherwise
// unchecked.
func counterFrom(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}

// redrawPinned redraws the pinned row when there is one. Callers hold no
// lock; the task runner calls it under its own mutex, which already orders
// before consoleMu everywhere else (PrintAbove under r.mu).
func redrawPinned() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if pinned && activeReader != nil && activeReader.reading {
		activeReader.drawPinnedLocked()
	}
}

// pinnedNote is the dim note at the right end of the pinned row: the pending
// guidance count, the queue count, or both joined by " · "; "" when neither
// applies.
func pinnedNote(steered, queued int32) string {
	var parts []string
	if steered > 0 {
		parts = append(parts, fmt.Sprintf("已插入 %d 条指引", steered))
	}
	if queued > 0 {
		parts = append(parts, fmt.Sprintf("已排队 %d 条", queued))
	}
	return strings.Join(parts, " · ")
}

// pinEnabled reports whether the pinned row may be used at all: stdout is a
// terminal, COVE_PIN_INPUT does not turn it off, and the terminal has not
// recently (or repeatedly) failed to answer a cursor query (cprUsable).
func pinEnabled() bool {
	if !cprUsable() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("COVE_PIN_INPUT"))) {
	case "0", "false", "off", "no":
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// promptVisibleWidth is the display width of a prompt string, skipping its
// escape sequences.
func promptVisibleWidth(p string) int {
	w := 0
	inAnsi := false
	for _, r := range p {
		if r == '\x1b' {
			inAnsi = true
			continue
		}
		if inAnsi {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inAnsi = false
			}
			continue
		}
		w += runeCellWidth(r)
	}
	return w
}

// pinSequence is what to print, with the cursor at pos (1-based) of an h-row
// terminal, to make rows 1..h-pinnedReserve the scroll region and put the
// cursor where the stream continues. With separator set (the start of a
// turn) that is column 1 of the next row, leaving one blank row; without it
// the cursor stays where it is, column included, since the stream may be in
// the middle of a line. Either way, when the cursor would end up in the
// reserved rows, the whole screen is scrolled first so what was on its row
// lands on the region's last row. It returns the row the cursor ends up on.
// Newlines are \r\n: the terminal is in raw mode.
func pinSequence(pos cursorPos, h int, separator bool) (string, int) {
	return pinSequenceReserved(pos, h, separator, pinnedReserve)
}

func pinSequenceReserved(pos cursorPos, h int, separator bool, reserve int) (string, int) {
	bottom := h - reserve
	var sb strings.Builder
	row, col := pos.row, pos.col
	if col < 1 {
		col = 1
	}
	if separator {
		sb.WriteString("\r\n")
		col = 1
		if row < h {
			row++
		}
	}
	if k := row - bottom; k > 0 {
		// A newline on the last row scrolls the screen and stays put, so k
		// of them from there move the cursor's row up to the region's last
		// row, with everything above it.
		if row < h {
			fmt.Fprintf(&sb, "\x1b[%d;1H", h)
		}
		sb.WriteString(strings.Repeat("\r\n", k))
		row = bottom
	}
	fmt.Fprintf(&sb, "\x1b[1;%dr\x1b[%d;%dH", bottom, row, col)
	return sb.String(), row
}

// unpinSequence clears the reserved rows (the rule and the input row) and
// resets the scroll margins without moving the stream's cursor; DECSTBM
// homes the cursor, hence DECSC/DECRC around it.
func unpinSequence(h int) string {
	return unpinSequenceReserved(h, pinnedReserve)
}

func unpinSequenceReserved(h, reserve int) string {
	var output strings.Builder
	output.WriteString("\x1b7")
	for row := h - reserve + 1; row <= h; row++ {
		fmt.Fprintf(&output, "\x1b[%d;1H\x1b[2K", row)
	}
	output.WriteString("\x1b[r\x1b8")
	return output.String()
}

// pinnedInputLine renders the pinned row: prompt, the visible window of the
// buffer and a reverse-video caret, since the real cursor stays with the
// stream. An empty buffer shows placeholder, dimmed and a cell away from the
// caret so it reads as a hint rather than as typed text. The row is kept
// under w cells so it can never wrap onto the stream, which also keeps the
// last column free of a pending wrap.
func pinnedInputLine(prompt string, promptWidth int, buf []rune, cursor, w int, placeholder string) string {
	maxVis := w - promptWidth - 2
	if maxVis < 1 {
		maxVis = 1
	}
	var sb strings.Builder
	sb.WriteString("\x1b[0m")
	sb.WriteString(prompt)
	if len(buf) == 0 {
		sb.WriteString("\x1b[7m \x1b[27m ")
		ph, _ := truncateRunesByCells([]rune(placeholder), maxVis-2)
		sb.WriteString("\x1b[2;90m" + string(ph) + "\x1b[0m")
		return sb.String()
	}
	shown := displayRunes(buf)
	disp, _, _, start := inputDisplayWindow(shown, cursor, maxVis)
	rel := cursor - start
	if rel < 0 {
		rel = 0
	}
	if rel > len(disp) {
		rel = len(disp)
	}
	sb.WriteString(string(disp[:rel]))
	if rel < len(disp) {
		// The caret covers the whole cluster under the cursor: ending the
		// reverse video between "⚠" and its U+FE0F split the sequence, and
		// the terminal drew the halves as separate cells.
		_, cont := cellWidths(disp)
		next := rel + 1
		for next < len(disp) && cont[next] {
			next++
		}
		sb.WriteString("\x1b[7m" + string(disp[rel:next]) + "\x1b[27m" + string(disp[next:]))
	} else {
		sb.WriteString("\x1b[7m \x1b[27m")
	}
	sb.WriteString("\x1b[0m")
	return sb.String()
}

// queryCursorRow asks the terminal where the cursor is and waits for the
// key loop to relay the answer. Callers must not hold consoleMu: the loop
// takes it while it edits. A terminal that does not answer in time is
// remembered (noteCPRMiss), and pinning waits a while before asking again.
func queryCursorPos() (cursorPos, bool) {
	cprMu.Lock()
	defer cprMu.Unlock()
	if !cprUsable() {
		return cursorPos{}, false
	}
	select {
	case <-cprCh: // a stale report from an earlier, abandoned query
	default:
	}
	termPrint("\x1b[6n")
	select {
	case pos := <-cprCh:
		return pos, true
	case <-time.After(cprTimeout):
		noteCPRMiss()
		return cursorPos{}, false
	}
}

// pinAtCursor enters pinned mode from the current cursor position, with a
// blank separator row before the stream when separator is set (the start of
// a turn) and none otherwise (resuming after a prompt, or a ReadLine that
// began while the stream was already running). Called without consoleMu,
// with streamingActive already set. When pinning turns out impossible it
// prints the plain "\n" the unpinned start of a turn always printed, so the
// fallback looks exactly as before; when another caller pinned meanwhile it
// leaves things alone.
func pinAtCursor(separator bool) {
	pos, ok := queryCursorPos()
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if pinned || !streamingActive {
		return
	}
	lr := activeReader
	fallback := func() {
		if separator {
			termPrint("\n")
		}
	}
	if !ok || lr == nil || !lr.reading {
		fallback()
		return
	}
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || h < minPinRows || w < 20 {
		fallback()
		return
	}
	// pinned is set before the reserved rows are measured: the candidate
	// block pads itself to a fixed height only while pinned, so measuring
	// it unpinned stored the natural height, drawPinnedLocked then saw the
	// padded one, unpinned and pinned again - an unpin/cursor-query/repin
	// loop for as long as a candidate list was open.
	pinned = true
	pinPanelRows = lr.panelRows(h)
	seq, _ := pinSequenceReserved(pos, h, separator, pinnedReserve+pinPanelRows)
	termPrint(seq)
	if separator {
		streamMidLine = false
	}
	pinRows, pinCols = h, w
	lr.drawPinnedLocked()
}

// pinIfStreaming is ReadLine's side of pinning: when a turn started while no
// editor was reading (the main loop was between two ReadLine calls, so
// BeginOutput could not pin), the editor pins as soon as it is back, from
// wherever the stream's cursor is. The query is answered through this very
// key loop, so it runs on its own goroutine. Callers hold no lock.
func pinIfStreaming() {
	consoleMu.Lock()
	want := streamingActive && !pinned && activeReader != nil && activeReader.reading && pinEnabled()
	consoleMu.Unlock()
	if want {
		startPin(func() { pinAtCursor(false) })
	}
}

// pinWG tracks the detached pinners: pinIfStreaming and pinAtCursor run off
// the key loop, because the loop is what answers the terminal's cursor query.
// waitForPin joins them, so a caller about to hand the terminal over — a test
// swapping os.Stdout, or shutdown — does not read it while one is still
// writing.
var pinWG sync.WaitGroup

// startPin runs f on the pinning goroutine's bookkeeping. f may start more
// pinned work through startPin; the counter is already up when it does, so a
// concurrent waitForPin cannot see it reach zero in between.
func startPin(f func()) {
	pinWG.Add(1)
	go func() {
		defer pinWG.Done()
		f()
	}()
}

// waitForPin blocks until no detached pinning work is outstanding. Tests call
// it before they take os.Stdout back.
func waitForPin() { pinWG.Wait() }

// unpinLocked leaves pinned mode. Callers hold consoleMu.
func unpinLocked() {
	if !pinned {
		return
	}
	termPrint(unpinSequenceReserved(pinRows, pinnedReserve+pinPanelRows))
	pinned = false
	pinPanelRows = 0
}

// drawPinnedLocked draws the reserved rows — a dim rule on the row above the
// last, the input line on the last — and puts the cursor back where the
// stream left it. When guidance awaits the next model call or tasks are
// queued, their counts sit at the right end of the input row (pinnedNote). A resized window moves the scroll region and clears the
// rows the reserved rows used to occupy. Callers hold consoleMu with pinned
// set.
func (lr *LineReader) drawPinnedLocked() {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || h < minPinRows || w < 20 {
		w, h = pinCols, pinRows
	}
	if lr.repinIfResizedLocked(w, h) {
		return
	}
	var sb strings.Builder
	sb.WriteString("\x1b7")
	rows := lr.displayRowsLocked()
	if len(rows) > pinPanelRows {
		rows = rows[:pinPanelRows]
	}
	for index := 0; index < pinPanelRows; index++ {
		line := ""
		if index < len(rows) {
			line = rows[index]
		}
		text, _ := truncateRunesByCells([]rune(line), w-1)
		fmt.Fprintf(&sb, "\x1b[%d;1H\x1b[2K%s", h-pinnedReserve-pinPanelRows+1+index, string(text))
	}
	separator := strings.Repeat("─", w-1)
	if text := lr.statusLineLocked(); text != "" {
		status, _ := truncateRunesByCells([]rune(text), w-1)
		separator = string(status)
	}
	fmt.Fprintf(&sb, "\x1b[%d;1H\x1b[2K\x1b[2m%s\x1b[0m", h-1, separator)
	fmt.Fprintf(&sb, "\x1b[%d;1H\x1b[2K", h)
	p := PromptRunning()
	placeholder := pinnedPlaceholder
	if lr.panelSource != nil && !lr.panelOpen {
		placeholder = "Alt+M 查看 Agent；" + placeholder
	}
	line := pinnedInputLine(p, promptVisibleWidth(p), lr.renderBuf, lr.renderCursor, w, placeholder)
	sb.WriteString(line)
	if note := pinnedNote(steerCount.Load(), queuedCount.Load()); note != "" {
		nw := promptVisibleWidth(note)
		if promptVisibleWidth(line)+nw+2 < w {
			fmt.Fprintf(&sb, "\x1b[%d;%dH\x1b[2m%s\x1b[0m", h, w-nw, note)
		}
	}
	sb.WriteString("\x1b8")
	termPrint(sb.String())
	lr.lineDrawn = false
}

// repinIfResizedLocked handles a window whose height changed while pinned.
// Moving the margins is not enough: the stream cursor saved by DECSC may now
// sit inside the reserved rows, where output neither scrolls nor stays
// visible. So the reserved rows are released and, from the terminal's real
// cursor position, the row is pinned again (pinIfStreaming asks the terminal
// where the cursor is; ReadLine's key loop relays the answer). It reports
// whether it did so. Callers hold consoleMu.
func (lr *LineReader) repinIfResizedLocked(w, h int) bool {
	if !pinned || h == pinRows && lr.panelRows(h) == pinPanelRows {
		pinCols = w
		repinStreak = 0
		return false
	}
	// A repin that leads straight to another repin is a measurement that
	// disagrees with itself; after a few rounds keep the current reservation
	// and draw into it (clipped) rather than flicker and spend cursor
	// queries until pinning is disabled for the session.
	if repinStreak >= maxRepinStreak {
		pinCols = w
		return false
	}
	repinStreak++
	termPrint(unpinSequenceReserved(pinRows, pinnedReserve+pinPanelRows))
	pinned = false
	pinPanelRows = 0
	pinRows, pinCols = 0, 0
	if lr.reading && streamingActive {
		startPin(pinIfStreaming)
	}
	return true
}

// repinStreak counts consecutive repins without a stable draw in between;
// maxRepinStreak is where drawPinnedLocked stops repinning and clips instead.
var repinStreak int

const maxRepinStreak = 3
