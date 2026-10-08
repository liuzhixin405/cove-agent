package repl

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

type Completer func(input string) []string

type LineReader struct {
	history        []string
	histIdx        int
	completer      Completer
	prompt         string
	promptWidth    int
	placeholder    string
	fallbackReader *bufio.Reader
	rawReader      *bufio.Reader
	reading        bool
	renderBuf      []rune
	renderCursor   int
	lineDrawn      bool
	completionBase string
	completionList []string
	completionIdx  int
	activeHint     string
	// drawnRows and cursorRow describe a multi-row input drawn below the
	// output (redrawMultiLocked): how many rows it takes and which one the
	// cursor is on, so the next redraw or erase can go back to its top.
	drawnRows int
	cursorRow int
	ownerWake func() <-chan struct{}
	ownerPoll func()
}

func (lr *LineReader) SetOwnerEventHook(wake func() <-chan struct{}, poll func()) {
	lr.ownerWake, lr.ownerPoll = wake, poll
}

type ownerInput struct {
	source io.Reader
	lr     *LineReader
}

type inputResult struct {
	data []byte
	err  error
}

func (in ownerInput) Read(dst []byte) (int, error) {
	if in.lr.ownerPoll == nil {
		return in.source.Read(dst)
	}
	result := make(chan inputResult, 1)
	go func() {
		data := make([]byte, len(dst))
		count, err := in.source.Read(data)
		result <- inputResult{data: data[:count], err: err}
	}()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var wake <-chan struct{}
		if in.lr.ownerWake != nil {
			wake = in.lr.ownerWake()
		}
		select {
		case read := <-result:
			return copy(dst, read.data), read.err
		case <-wake:
			in.lr.ownerPoll()
		case <-ticker.C:
			in.lr.ownerPoll()
		}
	}
}

var ErrExit = fmt.Errorf("exit")
var ErrInterrupt = fmt.Errorf("interrupt")

// ErrEscape is a bare Esc on an empty input line: the front end interrupts
// the running task with it (Esc on a line with text clears the text).
var ErrEscape = fmt.Errorf("escape")

func New(completer Completer) *LineReader {
	lr := &LineReader{
		completer:   completer,
		placeholder: "(按 / 显示命令，Ctrl+J 换行)",
	}
	lr.SetPrompt(Prompt()) // derives promptWidth correctly (skips ANSI codes)
	lr.history = loadHistory()
	lr.histIdx = len(lr.history)
	return lr
}

// SetPrompt changes the prompt string and recalculates its visual width.
// ANSI escape sequences are skipped so the width reflects only visible cells.
func (lr *LineReader) SetPrompt(p string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	lr.prompt = p
	lr.promptWidth = promptVisibleWidth(p)
}

// waitStreamingDone is obsolete: streaming output no longer erases the input
// line, so ReadLine may enter raw mode immediately even while a task streams
// (this is what enables blind type-ahead into the task queue). Kept removed to
// avoid the multi-second cooked-mode stall it used to impose on every
// mid-task ReadLine.

func (lr *LineReader) ReadLine() (string, error) {
	if lr.ownerPoll != nil {
		lr.ownerPoll()
	}
	if shouldUseFallbackReadline() {
		return lr.fallbackRead()
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return lr.fallbackRead()
	}
	defer func() {
		termPrint("\x1b[0m\x1b[?25h")
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}()

	// The reader lives as long as the LineReader. It used to be rebuilt on
	// every call, so whatever the previous read had already buffered — the
	// rest of a paste, keys typed ahead — was thrown away with it.
	if lr.rawReader == nil {
		lr.rawReader = bufio.NewReaderSize(ownerInput{source: os.Stdin, lr: lr}, rawInputBufferSize)
	}
	// Bracketed paste makes the terminal wrap pasted text in ESC[200~ …
	// ESC[201~, so its newlines can be told apart from Enter. Terminals that
	// do not support it ignore the request.
	termPrint("\x1b[0m\x1b[?25h\x1b[?2004h")
	defer termPrint("\x1b[?2004l")

	consoleMu.Lock()
	activeReader = lr
	lr.reading = true
	lr.renderBuf = nil
	lr.renderCursor = 0
	lr.lineDrawn = false
	consoleMu.Unlock()

	defer func() {
		consoleMu.Lock()
		if activeReader == lr {
			activeReader = nil
		}
		lr.reading = false
		lr.lineDrawn = false
		consoleMu.Unlock()
	}()

	return lr.editLine()
}

// editLine runs the raw-mode editor on lr.rawReader until a line is submitted.
func (lr *LineReader) editLine() (string, error) {
	var buf []rune
	cursor := 0
	lr.redraw(buf, cursor)
	pinIfStreaming()

	for {
		r, err := readInputRune(lr.rawReader)
		if err != nil {
			return lr.endOfInput(buf, err)
		}

		// A prompt waiting on a one-key answer (y/a/p/n, c/s, an option
		// number) takes the key itself on an empty line: no Enter needed.
		if len(buf) == 0 && lr.rawReader.Buffered() == 0 && promptKeyAnswers(r) {
			return lr.submit([]rune{r}), nil
		}

		switch r {
		case 3:
			consoleMu.Lock()
			lr.eraseLineLocked()
			consoleMu.Unlock()
			termPrint("\r\n")
			return "", ErrInterrupt
		case 4:
			if len(buf) == 0 {
				consoleMu.Lock()
				lr.eraseLineLocked()
				releaseForExitLocked()
				consoleMu.Unlock()
				termPrint("\r\n")
				return "", ErrExit
			}
		case '\r', '\n':
			// A person cannot press Enter and further keys within one read,
			// so a newline with input already waiting behind it is part of a
			// paste on a console without bracketed paste (conhost). It used
			// to submit each pasted line as a separate message.
			if lr.rawReader.Buffered() > 0 {
				if r == '\r' {
					lr.skipRune('\n')
				}
				buf, cursor = insertRunes(buf, cursor, []rune{'\n'})
				lr.refresh(buf, cursor)
				continue
			}
			// Ctrl+J types a newline; so does Enter after a trailing "\"
			// (shell-style continuation).
			if r == '\n' {
				buf, cursor = insertRunes(buf, cursor, []rune{'\n'})
				lr.refresh(buf, cursor)
				continue
			}
			if cursor == len(buf) && cursor > 0 && buf[cursor-1] == '\\' {
				buf[cursor-1] = '\n'
				lr.refresh(buf, cursor)
				continue
			}
			return lr.submit(buf), nil
		case 1: // Ctrl+A
			cursor = 0
			lr.redraw(buf, cursor)
		case 5: // Ctrl+E
			cursor = len(buf)
			lr.redraw(buf, cursor)
		case 11: // Ctrl+K: delete to the end
			buf = buf[:cursor]
			lr.refresh(buf, cursor)
		case 21: // Ctrl+U: delete to the start
			buf = append([]rune(nil), buf[cursor:]...)
			cursor = 0
			lr.refresh(buf, cursor)
		case 23: // Ctrl+W: delete the word before the cursor
			buf, cursor = deleteWordBack(buf, cursor)
			lr.refresh(buf, cursor)
		case 18: // Ctrl+R: search the history
			if line, ok, err := lr.reverseSearch(buf); err != nil {
				return lr.endOfInput(buf, err)
			} else if ok {
				buf, cursor = []rune(line), len([]rune(line))
			}
			lr.redraw(buf, cursor)
		case 12:
			// Ctrl+L clears the screen and keeps what is being typed.
			consoleMu.Lock()
			lr.renderBuf = append(lr.renderBuf[:0], buf...)
			lr.renderCursor = cursor
			consoleMu.Unlock()
			ClearScreen()
		case 127, 8:
			lr.resetCompletionCycle()
			if cursor > 0 {
				copy(buf[cursor-1:], buf[cursor:])
				buf = buf[:len(buf)-1]
				cursor--
				lr.refresh(buf, cursor)
			}
		case 27:
			lr.resetCompletionCycle()
			// A key's escape sequence arrives in one read; an ESC with nothing
			// behind it is the Esc key. It clears the line, or on an empty
			// line interrupts the running task.
			if lr.rawReader.Buffered() == 0 && escInterruptEnabled() {
				if len(buf) > 0 {
					buf, cursor = nil, 0
					lr.redraw(buf, cursor)
					continue
				}
				consoleMu.Lock()
				lr.eraseLineLocked()
				consoleMu.Unlock()
				return "", ErrEscape
			}
			if err := lr.handleEscape(&buf, &cursor); err != nil {
				return lr.endOfInput(buf, err)
			}
		case '\t':
			if lr.rawReader.Buffered() > 0 {
				// A tab inside pasted text is text, not a completion request.
				buf, cursor = insertRunes(buf, cursor, []rune{'\t'})
				lr.refresh(buf, cursor)
				continue
			}
			lr.complete(&buf, &cursor)
		default:
			if r >= 32 {
				lr.resetCompletionCycle()
				buf, cursor = insertRunes(buf, cursor, []rune{r})
				lr.refresh(buf, cursor)
			}
		}
	}
}

// submit ends the line: erases the editor, echoes the line into the
// transcript, records it in the history and returns it.
func (lr *LineReader) submit(buf []rune) string {
	line := string(buf)
	consoleMu.Lock()
	lr.eraseLineLocked()
	// 关键点：在按下回车后，先把用户输入的内容打印到终端，使之成为历史可见内容。
	// 流式输出进行中：输入行钉在底部时，把内容回显到上方的输出流里（先另起一行，
	// 不接在模型未完成的句子后面），让排队的指令在记录里可见；没有钉住时不回显，
	// 否则会把提示符+内容插进流式文本里造成错乱。
	switch {
	case !streamingActive:
		termPrint(lr.prompt + normalizeOutputNewlines(line) + "\r\n")
	case pinned && line != "":
		// An empty Enter is ignored by the main loop, so it leaves
		// no trace here either; it used to print a bare prompt row
		// into the model's output at every press.
		breakStreamLineLocked()
		termPrint(PromptRunning() + normalizeOutputNewlines(line) + "\r\n")
	}
	consoleMu.Unlock()

	// One-key prompt answers are not worth recalling; lines the history
	// filter refuses (secrets) are not kept at all.
	if len([]rune(line)) > 1 && (len(lr.history) == 0 || lr.history[len(lr.history)-1] != line) && historyKeeps(line) {
		lr.history = append(lr.history, line)
		appendHistory(line)
	}
	lr.histIdx = len(lr.history)
	return line
}

// rawInputBufferSize is large so that a pasted block normally arrives in one
// buffer fill: the paste heuristic in editLine looks at what is buffered.
const rawInputBufferSize = 64 * 1024

// endOfInput maps a read error to what the REPL loop expects. A closed stdin
// (terminal gone, pipe ended) used to come back as io.EOF, which the loop
// treats as "reinitialise and read again" — forever, at full CPU, printing the
// same error line. Text typed before the end is still delivered; the next call
// then reports ErrExit.
func (lr *LineReader) endOfInput(buf []rune, err error) (string, error) {
	if !errors.Is(err, io.EOF) {
		return "", err
	}
	consoleMu.Lock()
	lr.eraseLineLocked()
	if len(buf) == 0 {
		releaseForExitLocked()
	}
	consoleMu.Unlock()
	termPrint("\r\n")
	if len(buf) > 0 {
		return string(buf), nil
	}
	return "", ErrExit
}

// releaseForExitLocked hands the terminal back before ErrExit ends the
// session. The pinned row's scroll region used to be reset only by EndOutput
// and BeginPromptInput, so Ctrl+D (or a closed stdin) during a streaming turn
// exited with DECSTBM still set and the user's shell went on scrolling inside
// a region two rows short of the window. Streaming is ended too, so the
// goodbye line and anything else printed on the way out lands in the normal
// flow instead of being treated as part of the stream. Callers hold
// consoleMu.
func releaseForExitLocked() {
	breakStreamLineLocked()
	unpinLocked()
	streamingActive = false
	streamMidLine = false
}

// ReleaseTerminal is releaseForExitLocked for a front end that ends the
// session some other way (a /exit command, a signal) while a turn may still
// be streaming: it resets the pinned row's scroll region so the shell gets
// its whole window back. Safe to call when nothing is pinned.
func ReleaseTerminal() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	releaseForExitLocked()
}

// refresh redraws the input line and its command hints, unless more input is
// already waiting. Redrawing after every rune of a paste made a long paste
// quadratic (each redraw copies and measures the whole buffer); the line is
// drawn once the burst has been consumed.
func (lr *LineReader) refresh(buf []rune, cursor int) {
	if lr.rawReader != nil && lr.rawReader.Buffered() > 0 {
		consoleMu.Lock()
		lr.renderBuf = append(lr.renderBuf[:0], buf...)
		lr.renderCursor = cursor
		consoleMu.Unlock()
		return
	}
	lr.redraw(buf, cursor)
	if lr.completer == nil || len(buf) == 0 || buf[0] != '/' {
		return
	}
	line := string(buf)
	suggestions := lr.completer(line)
	if len(suggestions) > 0 && len(suggestions) <= 10 {
		lr.showInlineSuggestions(suggestions, lr.promptWidth+cursor)
	} else if len(suggestions) > 10 {
		lr.showCommandCountHint(len(suggestions), lr.promptWidth+cursor)
	}
}

func (lr *LineReader) redraw(buf []rune, cursor int) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	lr.activeHint = ""
	lr.redrawLocked(buf, cursor)
}

func (lr *LineReader) eraseLineLocked() {
	// While streaming, the input line is not on the current terminal line
	// (it is off screen or pinned to the last row), so that line holds
	// streamed output; erasing it would corrupt the stream.
	if streamingActive {
		lr.lineDrawn = false
		return
	}
	if lr.drawnRows > 1 {
		lr.clearRowsLocked()
	} else {
		termPrint("\x1b[0m\x1b[?25h\r\x1b[2K")
		lr.drawnRows, lr.cursorRow = 0, 0
	}
	lr.lineDrawn = false
}

func (lr *LineReader) redrawLocked(buf []rune, cursor int) {
	cursor = clampCursor(cursor, len(buf))
	lr.renderBuf = append(lr.renderBuf[:0], buf...)
	lr.renderCursor = cursor
	if streamingActive {
		if pinned {
			lr.drawPinnedLocked()
		}
		return
	}
	// Draw on the current line.
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 20 {
		w = 80
	}
	// A multi-line or long input takes several rows (editing.go).
	if lr.needsMultiRow(buf, w) {
		lr.redrawMultiLocked(buf, cursor, w)
		return
	}
	if lr.drawnRows > 1 {
		lr.clearRowsLocked()
	}
	lr.drawnRows, lr.cursorRow = 1, 0
	termPrint("\x1b[0m\x1b[?25h\r\x1b[2K")
	maxVis := w - lr.promptWidth - 1
	if maxVis < 1 {
		maxVis = 1
	}
	shown := displayRunes(buf)
	disp, _, used, start := inputDisplayWindow(shown, cursor, maxVis)
	termPrint(lr.prompt)
	if len(buf) == 0 && lr.placeholder != "" {
		ph, _ := truncateRunesByCells([]rune(lr.placeholder), maxVis)
		termPrint("\x1b[90m" + string(ph) + "\x1b[0m")
	} else {
		termPrint("\x1b[0m" + string(disp) + "\x1b[0m")
		if lr.activeHint != "" {
			// The hint follows the whole visible text, not the cursor, so
			// its room is what that text leaves. It used to be measured
			// from the cursor: with the cursor moved left, the row
			// overflowed by the text right of it and soft-wrapped, and the
			// next redraw left the wrapped half behind.
			rem := w - lr.promptWidth - used - 1
			termPrint(truncateAnsi(lr.activeHint, rem))
		}
	}
	// Position the cursor by re-emitting the prompt plus the visible text to the
	// left of the cursor, letting the terminal advance the cursor with its own
	// width rules. This avoids the half-cell drift that plain column arithmetic
	// (\x1b[NC) causes with East Asian ambiguous-width glyphs such as the prompt
	// arrow when running in a CJK terminal.
	termPrint("\r")
	left := shown[start:cursor]
	termPrint(lr.prompt + "\x1b[0m" + string(left))
	lr.lineDrawn = true
}

func (lr *LineReader) historyUp(buf *[]rune, cursor *int) {
	if len(lr.history) == 0 || lr.histIdx <= 0 {
		return
	}
	lr.histIdx--
	*buf = []rune(lr.history[lr.histIdx])
	*cursor = len(*buf)
	lr.redraw(*buf, *cursor)
}

func (lr *LineReader) historyDown(buf *[]rune, cursor *int) {
	if lr.histIdx < len(lr.history)-1 {
		lr.histIdx++
		*buf = []rune(lr.history[lr.histIdx])
		*cursor = len(*buf)
		lr.redraw(*buf, *cursor)
	} else if lr.histIdx == len(lr.history)-1 {
		lr.histIdx = len(lr.history)
		*buf = nil
		*cursor = 0
		lr.redraw(*buf, *cursor)
	}
}

func (lr *LineReader) fallbackRead() (string, error) {
	if lr.fallbackReader == nil {
		lr.fallbackReader = bufio.NewReader(ownerInput{source: os.Stdin, lr: lr})
	}
	termPrint(lr.prompt)
	line, err := lr.fallbackReader.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				return trimmed, nil
			}
			termPrint("\n")
			return "", ErrExit
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ---------------------------------------------------------------------------
// The termui.Console protocol
// ---------------------------------------------------------------------------

// This package and internal/termui both grew the same console protocol —
// erase the input line, print, redraw — but only this one knows whether there
// is an input line on screen. So the ~140 call sites that go through termui
// printed on top of the prompt while the editor's own writes did the right
// thing.
//
// These methods let termui delegate to the editor instead. They are the same
// package-level functions above, exposed as a value termui can hold without
// importing this package (it declares the interface, we satisfy it).
