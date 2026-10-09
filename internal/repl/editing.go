package repl

// Editing commands beyond insert/delete: word motion, the history search,
// the history file, Esc, and drawing an input that takes several rows.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

// isWordRune is what word motion treats as part of a word: letters, digits
// and "_" (a CJK character is a letter).
func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// wordLeft is the start of the word before cursor.
func wordLeft(buf []rune, cursor int) int {
	i := cursor
	for i > 0 && !isWordRune(buf[i-1]) {
		i--
	}
	for i > 0 && isWordRune(buf[i-1]) {
		i--
	}
	return i
}

// wordRight is the end of the word after cursor.
func wordRight(buf []rune, cursor int) int {
	i := cursor
	for i < len(buf) && !isWordRune(buf[i]) {
		i++
	}
	for i < len(buf) && isWordRune(buf[i]) {
		i++
	}
	return i
}

// deleteWordBack deletes from the start of the word before cursor to cursor.
func deleteWordBack(buf []rune, cursor int) ([]rune, int) {
	start := wordLeft(buf, cursor)
	out := append(append([]rune(nil), buf[:start]...), buf[cursor:]...)
	return out, start
}

// escInterruptEnabled: COVE_ESC_INTERRUPT=0 turns the Esc key off, for a
// connection slow enough to split a key's escape sequence across reads (an
// arrow key would otherwise read as Esc followed by text).
func escInterruptEnabled() bool { return os.Getenv("COVE_ESC_INTERRUPT") != "0" }

// ---------------------------------------------------------------------------
// History search (Ctrl+R)
// ---------------------------------------------------------------------------

// reverseSearch runs the incremental history search: typing narrows it,
// Ctrl+R steps to an older match, Enter puts the match on the input line,
// Esc or Ctrl+G gives the line back as it was (ok false).
//
// A query that matches nothing is a failing search, like bash's "failing
// reverse-i-search": the prompt says "无匹配", no entry is shown, and Enter
// (or an arrow key) gives the original line back, the same as Esc. The search
// used to keep the last entry that had matched a shorter query, so with
// "git status" in the history, typing "gitx" still showed it and Enter put
// it on the line. Backspace to a query that matches again finds it again;
// Ctrl+R with no older match keeps the current one, which still matches.
func (lr *LineReader) reverseSearch(orig []rune) (line string, ok bool, err error) {
	var query []rune
	idx := len(lr.history) // index of the current match; len = none
	// find moves idx to the newest match at or before from and reports
	// whether there was one; idx is left alone when there was not.
	find := func(from int) bool {
		q := strings.ToLower(string(query))
		for i := from; i >= 0; i-- {
			if i < len(lr.history) && strings.Contains(strings.ToLower(lr.history[i]), q) {
				idx = i
				return true
			}
		}
		return false
	}
	draw := func() {
		match := ""
		label := "(搜索历史)"
		if idx < len(lr.history) {
			match = lr.history[idx]
		} else if len(query) > 0 {
			label = "(搜索历史 · 无匹配)"
		}
		consoleMu.Lock()
		savedPrompt, savedWidth := lr.prompt, lr.promptWidth
		lr.prompt = "\x1b[36m" + label + "\x1b[0m " + string(query) + " ❯ "
		lr.promptWidth = promptVisibleWidth(lr.prompt)
		m := []rune(match)
		lr.redrawLocked(m, len(m))
		lr.prompt, lr.promptWidth = savedPrompt, savedWidth
		consoleMu.Unlock()
	}
	draw()
	for {
		r, err := readInputRune(lr.rawReader)
		if err != nil {
			return "", false, err
		}
		switch {
		case r == 18: // Ctrl+R: older match
			if idx > 0 {
				_ = find(idx - 1)
			}
		case r == 7: // Ctrl+G
			return string(orig), false, nil
		case r == 27:
			if lr.rawReader.Buffered() > 0 {
				// A key's sequence (an arrow): leave the search with the match.
				if next, _ := readInputRune(lr.rawReader); next == '[' {
					params, final, _ := readCSI(lr.rawReader)
					// The terminal's cursor report is not a key: it was
					// taken for an arrow, ended the search and never
					// reached the waiting query, which then switched
					// pinning off. Relay it and keep searching.
					if final == 'R' && deliverCursorReport(params) {
						continue
					}
				}
				return lr.searchResult(idx, orig)
			}
			return string(orig), false, nil
		case r == '\r' || r == '\n':
			return lr.searchResult(idx, orig)
		case r == 127 || r == 8:
			if len(query) > 0 {
				query = query[:len(query)-1]
				idx = len(lr.history)
				_ = find(len(lr.history) - 1)
			}
		case r >= 32:
			query = append(query, r)
			from := idx
			if from >= len(lr.history) {
				from = len(lr.history) - 1
			}
			if !find(from) {
				idx = len(lr.history) // failing: no stale match to accept
			}
		}
		draw()
	}
}

func (lr *LineReader) searchResult(idx int, orig []rune) (string, bool, error) {
	if idx < len(lr.history) {
		lr.histIdx = idx
		return lr.history[idx], true, nil
	}
	return string(orig), false, nil
}

// ---------------------------------------------------------------------------
// History file
// ---------------------------------------------------------------------------

// historyFileMax is how many entries the history file keeps.
const historyFileMax = 1000

var (
	historyMu   sync.Mutex
	historyPath string // "" = history is not persisted (tests, -p)
	// historyFilter, when set, decides which lines the history keeps
	// (SetHistoryFilter).
	historyFilter func(line string) bool
)

// SetHistoryFilter makes the editor keep in its history (memory and file)
// only the lines keep accepts; nil keeps every line. The application knows
// which lines carry secrets: every submitted line used to be recorded, so
// "/api-key sk-..." sat in the history file in plain text and came back
// with Up. Lines already in the file are filtered when it is loaded too.
func SetHistoryFilter(keep func(line string) bool) {
	historyMu.Lock()
	historyFilter = keep
	historyMu.Unlock()
}

// historyKeeps reports whether the history may record line.
func historyKeeps(line string) bool {
	historyMu.Lock()
	keep := historyFilter
	historyMu.Unlock()
	return keep == nil || keep(line)
}

// SetHistoryFile makes the editor keep its history in path across restarts
// (one JSON string per line, so multi-line entries survive). Call it before
// New. The history used to live only in memory.
func SetHistoryFile(path string) {
	historyMu.Lock()
	historyPath = path
	historyMu.Unlock()
}

func loadHistory() []string {
	historyMu.Lock()
	path := historyPath
	historyMu.Unlock()
	entries := readHistoryFile(path)
	kept := entries[:0]
	for _, e := range entries {
		if historyKeeps(e) {
			kept = append(kept, e)
		}
	}
	return kept
}

func readHistoryFile(path string) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var s string
		if json.Unmarshal(sc.Bytes(), &s) == nil && s != "" {
			out = append(out, s)
		}
	}
	if len(out) > historyFileMax {
		out = out[len(out)-historyFileMax:]
	}
	return out
}

// appendHistory adds line to the history file, compacting the file to the
// newest historyFileMax entries once it has grown to twice that.
func appendHistory(line string) {
	historyMu.Lock()
	defer historyMu.Unlock()
	if historyPath == "" {
		return
	}
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(historyPath), 0o700)
	f, err := os.OpenFile(historyPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
	info, _ := f.Stat()
	_ = f.Close()
	if info != nil && info.Size() > 2*1024*1024 {
		entries := readHistoryFile(historyPath)
		var sb strings.Builder
		for _, e := range entries {
			if historyFilter != nil && !historyFilter(e) {
				continue // recorded before the filter was set
			}
			if d, err := json.Marshal(e); err == nil {
				sb.Write(d)
				sb.WriteByte('\n')
			}
		}
		_ = os.WriteFile(historyPath, []byte(sb.String()), 0o600)
	}
}

// ---------------------------------------------------------------------------
// Multi-row input
// ---------------------------------------------------------------------------

// maxInputRows is the most rows the input is drawn on; a longer input falls
// back to the one-row window (a block taller than the screen cannot be
// redrawn in place).
const maxInputRows = 12

// inputRow is one drawn row of a multi-row input.
type inputRow struct {
	prefix string // the prompt on the first row, spaces under it after
	text   []rune
	start  int // buffer index of text[0]
}

// layoutInput splits buf into rows of at most w-1 cells after the prompt,
// breaking at newlines, and finds the cursor's row and its offset in it.
func layoutInput(prompt string, promptWidth int, buf []rune, cursor, w int) (rows []inputRow, curRow, curOff int) {
	avail := w - 1 - promptWidth
	if avail < 8 {
		avail = 8
	}
	indent := strings.Repeat(" ", promptWidth)
	row := inputRow{prefix: prompt}
	cells := 0
	// Measured by grapheme cluster (cellWidths), on the runes as drawn: a
	// tab is shown as a space.
	shown := make([]rune, len(buf))
	for i, r := range buf {
		if r == '\t' {
			r = ' '
		}
		shown[i] = r
	}
	widths, _ := cellWidths(shown)
	for i, r := range buf {
		if i == cursor {
			curRow, curOff = len(rows), len(row.text)
		}
		if r == '\n' {
			rows = append(rows, row)
			row = inputRow{prefix: indent, start: i + 1}
			cells = 0
			continue
		}
		disp := r
		if r == '\t' {
			disp = ' '
		}
		cw := widths[i]
		if cells+cw > avail {
			rows = append(rows, row)
			row = inputRow{prefix: indent, start: i}
			cells = 0
			if i == cursor {
				curRow, curOff = len(rows), 0
			}
		}
		row.text = append(row.text, disp)
		cells += cw
	}
	if cursor >= len(buf) {
		curRow, curOff = len(rows), len(row.text)
	}
	rows = append(rows, row)
	return rows, curRow, curOff
}

// needsMultiRow reports whether buf is drawn on several rows: it has a
// newline or does not fit one row, and fits maxInputRows.
// inputRowCap is how many rows the input may take on a terminal h rows
// high: maxInputRows, but always leaving two rows (status and a line of
// output) so a 12-row input on a 12-row terminal does not scroll the screen
// and leave ghost rows the next clear cannot reach.
func inputRowCap(h int) int {
	if h <= 0 {
		return maxInputRows
	}
	return max(1, min(maxInputRows, h-2))
}

func (lr *LineReader) needsMultiRow(buf []rune, w, h int) bool {
	multi := false
	width := 0
	widths, _ := cellWidths(buf)
	for i, r := range buf {
		if r == '\n' {
			multi = true
			break
		}
		width += widths[i]
	}
	if !multi && width <= w-1-lr.promptWidth {
		return false
	}
	rows, _, _ := layoutInput(lr.prompt, lr.promptWidth, buf, len(buf), w)
	return len(rows) <= inputRowCap(h)
}

// clearRowsLocked erases a multi-row input: up to its first row, then to
// the end of the screen.
func (lr *LineReader) clearRowsLocked() {
	if lr.cursorRow > 0 {
		termPrint(fmt.Sprintf("\x1b[%dA", lr.cursorRow))
	}
	termPrint("\x1b[0m\r\x1b[J")
	lr.drawnRows, lr.cursorRow = 0, 0
}

// redrawMultiLocked draws buf on several rows and puts the cursor on its row
// by re-printing that row up to it (the terminal's own width rules then
// place it, as in the one-row editor).
func (lr *LineReader) redrawMultiLocked(buf []rune, cursor, w, h int) {
	if lr.drawnRows > 0 {
		lr.clearRowsLocked()
	} else {
		termPrint("\x1b[0m\x1b[?25h\r\x1b[2K")
	}
	rows, curRow, curOff := layoutInput(lr.prompt, lr.promptWidth, buf, cursor, w)
	if len(rows) > inputRowCap(h) {
		shown := displayRunes(buf)
		disp, _, _, start := inputDisplayWindow(shown, cursor, w-lr.promptWidth-1)
		rows = []inputRow{{prefix: lr.prompt, text: disp}}
		curRow, curOff = 0, cursor-start
	}
	if len(buf) == 0 && permInputCh == nil {
		placeholder := lr.placeholder
		if lr.choiceSearch {
			placeholder = "搜索会话"
		}
		text, _ := truncateRunesByCells([]rune(placeholder), w-lr.promptWidth-1)
		rows[0].prefix += "\x1b[90m"
		rows[0].text = text
	}
	if text := lr.statusLineLocked(); text != "" {
		status, _ := truncateRunesByCells([]rune(text), w-1)
		rows = append([]inputRow{{prefix: "\x1b[2m", text: status}}, rows...)
		curRow++
	}
	if lines := lr.displayRowsLocked(); len(lines) > 0 {
		count := lr.panelRows(h)
		if available := h - len(rows) - 1; count > available {
			count = max(0, available)
		}
		panel := make([]inputRow, 0, count)
		for _, line := range lines[:count] {
			text, _ := truncateRunesByCells([]rune(line), w-1)
			panel = append(panel, inputRow{text: text})
		}
		rows = append(panel, rows...)
		curRow += len(panel)
	}
	for i, row := range rows {
		if i > 0 {
			termPrint("\r\n")
		}
		termPrint("\x1b[0m" + row.prefix + string(row.text) + "\x1b[0m")
	}
	if up := len(rows) - 1 - curRow; up > 0 {
		termPrint(fmt.Sprintf("\x1b[%dA", up))
	}
	termPrint("\r" + rows[curRow].prefix + "\x1b[0m" + string(rows[curRow].text[:curOff]))
	lr.drawnRows, lr.cursorRow = len(rows), curRow
	lr.lineDrawn = true
}
