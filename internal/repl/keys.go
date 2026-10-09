package repl

// Decoding key input: escape sequences, bracketed paste, cursor reports.

import (
	"bufio"
	"strings"
)

// handleEscape consumes one escape sequence and applies the key it encodes.
//
// It used to understand only a bare ESC [ <letter>. Any sequence with
// parameters — Ctrl/Shift/Alt + arrow ("ESC[1;5D", which Windows Terminal
// sends for Ctrl+Left), Home/End as "ESC[1~"/"ESC[4~", Insert, F5 — stopped
// after the first parameter byte and the rest (";5D", "~") was typed into the
// input as text. SS3 keys (ESC O H for Home in application cursor mode, F1–F4)
// were typed in whole. Now the full sequence is read and an unknown one is
// dropped.
func (lr *LineReader) handleEscape(buf *[]rune, cursor *int) error {
	first, err := readInputRune(lr.rawReader)
	if err != nil {
		return err
	}
	switch first {
	case '[':
		params, final, err := readCSI(lr.rawReader)
		if err != nil {
			return err
		}
		if final == '~' && csiKey(params) == 200 {
			return lr.readBracketedPaste(buf, cursor)
		}
		lr.applyKey(buf, cursor, params, final)
	case 'O':
		final, err := readInputRune(lr.rawReader)
		if err != nil {
			return err
		}
		lr.applyKey(buf, cursor, "", final)
	case '\r', '\n':
		// Alt+Enter types a newline.
		*buf, *cursor = insertRunes(*buf, *cursor, []rune{'\n'})
		lr.refresh(*buf, *cursor)
	case 'b', 'B':
		*cursor = wordLeft(*buf, *cursor)
		lr.redraw(*buf, *cursor)
	case 'f', 'F':
		*cursor = wordRight(*buf, *cursor)
		lr.redraw(*buf, *cursor)
	case 'v', 'V':
		if lr.imageClipboard != nil {
			lr.imageClipboard()
			lr.redraw(*buf, *cursor)
		} else {
			_ = lr.rawReader.UnreadRune()
		}
	case 'm', 'M':
		if lr.togglePanel() {
			lr.redraw(*buf, *cursor)
		} else {
			_ = lr.rawReader.UnreadRune()
		}
	case 127, 8:
		// Alt+Backspace deletes the word before the cursor.
		if len(*buf) == 0 && lr.imageRemove != nil {
			lr.imageRemove()
			lr.redraw(*buf, *cursor)
			return nil
		}
		*buf, *cursor = deleteWordBack(*buf, *cursor)
		lr.refresh(*buf, *cursor)
	default:
		_ = lr.rawReader.UnreadRune()
	}
	return nil
}

// wordModifier reports whether a CSI key's parameters carry Ctrl or Alt
// ("1;5", "1;3"): Ctrl+Left/Right jump by words.
func wordModifier(params string) bool {
	_, mod, ok := strings.Cut(params, ";")
	return ok && (mod == "5" || mod == "3" || mod == "7")
}

// readCSI reads the rest of a control sequence after "ESC [": parameter
// bytes, intermediate bytes and the final byte. A byte that cannot belong to a
// sequence ends it early and is left unread; final is then 0.
func readCSI(r *bufio.Reader) (params string, final rune, err error) {
	var sb strings.Builder
	for {
		c, err := readInputRune(r)
		if err != nil {
			return sb.String(), 0, err
		}
		switch {
		case c >= 0x20 && c <= 0x3f:
			sb.WriteRune(c)
		case c >= 0x40 && c <= 0x7e:
			return sb.String(), c, nil
		default:
			_ = r.UnreadRune()
			return sb.String(), 0, nil
		}
	}
}

// parseCursorReport reads the "row;col" parameters of a cursor position
// report. A missing column is column 1.
func parseCursorReport(params string) (cursorPos, bool) {
	rowS, colS, _ := strings.Cut(params, ";")
	row := csiKey(rowS)
	if row < 1 {
		return cursorPos{}, false
	}
	col := csiKey(colS)
	if col < 1 {
		col = 1
	}
	return cursorPos{row: row, col: col}, true
}

// csiKey returns the first numeric parameter of a sequence ("1;5" -> 1), or
// -1 when there is none.
func csiKey(params string) int {
	head, _, _ := strings.Cut(params, ";")
	if head == "" {
		return -1
	}
	n := 0
	for _, c := range head {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// applyKey performs the editing action for a decoded key. Modifier parameters
// (Ctrl/Shift/Alt) are accepted and ignored: Ctrl+Left moves like Left rather
// than typing ";5D". Keys the editor has no use for do nothing.
func (lr *LineReader) applyKey(buf *[]rune, cursor *int, params string, final rune) {
	switch final {
	case 'R':
		// CSI row ; col R is the terminal's cursor position report, the
		// answer to queryCursorPos; SS3 R (F3) has no parameters.
		deliverCursorReport(params)
	case 'A':
		if lr.moveChoice(-1) {
			return
		}
		if lr.panelHasFocus() && lr.movePanel(-1) {
			return
		}
		if !lr.cycleOption(buf, cursor, -1) {
			lr.historyUp(buf, cursor)
		}
	case 'B':
		if lr.moveChoice(1) {
			return
		}
		if lr.panelHasFocus() && lr.movePanel(1) {
			return
		}
		if !lr.cycleOption(buf, cursor, 1) {
			lr.historyDown(buf, cursor)
		}
	case 'C':
		if wordModifier(params) {
			*cursor = wordRight(*buf, *cursor)
			lr.redraw(*buf, *cursor)
		} else if *cursor < len(*buf) {
			*cursor = clusterRight(*buf, *cursor)
			lr.redraw(*buf, *cursor)
		}
	case 'D':
		if wordModifier(params) {
			*cursor = wordLeft(*buf, *cursor)
			lr.redraw(*buf, *cursor)
		} else if *cursor > 0 {
			*cursor = clusterLeft(*buf, *cursor)
			lr.redraw(*buf, *cursor)
		}
	case 'H':
		*cursor = 0
		lr.redraw(*buf, *cursor)
	case 'Z':
		// Like Tab: only an empty input hands focus to the Agent panel (the
		// help and the design doc say "空输入 Tab 或 Shift+Tab"); with a
		// draft, Shift+Tab still brings focus back from the panel.
		if (len(*buf) == 0 || lr.panelHasFocus()) && lr.togglePanelFocus() {
			lr.redraw(*buf, *cursor)
		}
	case 'F':
		*cursor = len(*buf)
		lr.redraw(*buf, *cursor)
	case '~':
		switch csiKey(params) {
		case 1, 7:
			*cursor = 0
			lr.redraw(*buf, *cursor)
		case 4, 8:
			*cursor = len(*buf)
			lr.redraw(*buf, *cursor)
		case 3:
			if *cursor < len(*buf) {
				end := clusterRight(*buf, *cursor)
				*buf = append((*buf)[:*cursor], (*buf)[end:]...)
				lr.redraw(*buf, *cursor)
			}
		}
	}
}

// readBracketedPaste inserts everything up to the closing ESC[201~ as text.
// Newlines in it become part of the message instead of submitting it, and the
// line is drawn once at the end rather than once per pasted rune.
func (lr *LineReader) readBracketedPaste(buf *[]rune, cursor *int) error {
	var pasted []rune
	defer func() {
		*buf, *cursor = insertRunes(*buf, *cursor, pasted)
		lr.redraw(*buf, *cursor)
	}()
	for {
		r, err := readInputRune(lr.rawReader)
		if err != nil {
			return err
		}
		switch {
		case r == 27:
			next, err := readInputRune(lr.rawReader)
			if err != nil {
				return err
			}
			if next != '[' {
				_ = lr.rawReader.UnreadRune()
				continue
			}
			params, final, err := readCSI(lr.rawReader)
			if err != nil {
				return err
			}
			if final == '~' && csiKey(params) == 201 {
				if lr.imagePaste != nil && lr.imagePaste(string(pasted)) {
					pasted = nil
				}
				return nil
			}
			// A cursor report can land in the middle of a paste; it was
			// dropped with every other sequence, the query timed out, and
			// pinning was switched off.
			if final == 'R' {
				deliverCursorReport(params)
			}
		case r == '\r':
			lr.skipRune('\n')
			pasted = append(pasted, '\n')
		case r == '\n' || r == '\t':
			pasted = append(pasted, r)
		case r >= 32 && r != 127:
			pasted = append(pasted, r)
		}
	}
}

func readInputRune(r *bufio.Reader) (rune, error) {
	ch, _, err := r.ReadRune()
	return ch, err
}

// skipRune consumes the next rune if it is want and already buffered.
func (lr *LineReader) skipRune(want rune) {
	if lr.rawReader.Buffered() == 0 {
		return
	}
	if r, _, err := lr.rawReader.ReadRune(); err == nil && r != want {
		_ = lr.rawReader.UnreadRune()
	}
}

// insertRunes inserts rs at cursor and returns the new buffer and cursor.
func insertRunes(buf []rune, cursor int, rs []rune) ([]rune, int) {
	buf = append(buf, rs...)
	copy(buf[cursor+len(rs):], buf[cursor:len(buf)-len(rs)])
	copy(buf[cursor:], rs)
	return buf, cursor + len(rs)
}

// clusterLeft and clusterRight step over a whole grapheme cluster (an emoji
// with its VS16, ZWJ sequence or skin tone, a letter with combining marks).
// Moving and deleting by rune left the cursor inside a cluster: the terminal
// placed it a cell off, the next character typed glued itself to a stray
// U+FE0F, and Backspace on ⚠️ removed only the invisible selector.
func clusterLeft(buf []rune, cursor int) int {
	if cursor <= 0 {
		return 0
	}
	_, cont := cellWidths(buf)
	i := cursor - 1
	for i > 0 && cont[i] {
		i--
	}
	return i
}

func clusterRight(buf []rune, cursor int) int {
	if cursor >= len(buf) {
		return len(buf)
	}
	_, cont := cellWidths(buf)
	i := cursor + 1
	for i < len(buf) && cont[i] {
		i++
	}
	return i
}
