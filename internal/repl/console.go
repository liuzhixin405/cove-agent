package repl

// Printing while the line editor may be on screen: every write goes
// through here so the input line is erased and redrawn around it, and a
// streamed turn is not broken into.

import (
	"fmt"
	"strings"
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/textmode"
)

var consoleMu sync.Mutex

var activeReader *LineReader

var streamingActive bool

// streamMidLine records that streamed output left the cursor after text on
// its row. Without it, anything that starts a new block during a stream — a
// notice, the permission box's input-line redraw (\r ESC[2K) — landed on that
// row: a notice was glued to the model's unfinished sentence, and the redraw
// erased the partial line outright.
var streamMidLine bool

func PrintSafe(format string, args ...any) {
	PrintAbove(fmt.Sprintf(format, args...))
}

func normalizeOutputNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func printOutputLocked(s string, ensureTrailingNewline bool) {
	termPrint(s)
	if ensureTrailingNewline && !strings.HasSuffix(s, "\r\n") {
		termPrint("\r\n")
	}
}

// secretHeld collects output printed while a secret is being typed
// (ReadSecret), so a background notice does not land in the masked prompt;
// endSecretHold prints it. Guarded by consoleMu.
var (
	secretActive bool
	secretHeld   []string
)

func beginSecretHold() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	secretActive = true
}

func endSecretHold() {
	consoleMu.Lock()
	held := secretHeld
	secretActive, secretHeld = false, nil
	consoleMu.Unlock()
	for _, s := range held {
		PrintAbove(s)
	}
}

func PrintAbove(s string) {
	s = normalizeOutputNewlines(s)
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if secretActive {
		secretHeld = append(secretHeld, s)
		return
	}

	// While a task is streaming the input line is either off screen or pinned
	// to the last row outside the scroll region, so print inline without
	// erasing/redrawing (which would corrupt partial streamed lines that
	// don't end in a newline).
	if streamingActive {
		breakStreamLineLocked()
		printOutputLocked(s, !strings.HasSuffix(s, "\r\n"))
		streamMidLine = false
		return
	}

	if activeReader == nil || !activeReader.reading {
		termPrint(s)
		return
	}

	activeReader.eraseLineLocked()
	printOutputLocked(s, !strings.HasSuffix(s, "\r\n"))
	activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
}

func StreamPrint(s string) {
	s = normalizeOutputNewlines(s)

	consoleMu.Lock()
	defer consoleMu.Unlock()
	// During streaming, print the chunk verbatim. Erasing/redrawing the input
	// line here is what corrupted the "thinking"/answer stream, because a chunk
	// without a trailing newline shares the current terminal line and the next
	// erase (\r\x1b[2K) wiped it.
	if streamingActive {
		termPrint(s)
		streamMidLine = endsMidLine(streamMidLine, s)
		return
	}
	if activeReader != nil && activeReader.reading {
		activeReader.eraseLineLocked()
		termPrint(s)
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
		return
	}
	termPrint(s)
}

func PrintTransientStatus(s string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()

	// The spinner runs during streaming; just overwrite the current line in
	// place without touching any input-line state.
	if streamingActive {
		termPrint("\x1b[0m\x1b[?25h\r\x1b[K" + s)
		streamMidLine = s != ""
		return
	}

	if activeReader != nil && activeReader.reading {
		activeReader.eraseLineLocked()
		termPrint("\x1b[0m\x1b[?25h" + s)
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
		return
	}
	termPrint("\x1b[0m\x1b[?25h\r\x1b[K" + s)
}

// BeginOutput starts a turn's streaming. With an editor on screen and a
// terminal that can say where its cursor is, the input line moves to the
// last row and stays there (see pinned.go); otherwise there is no input line
// on screen until EndOutput, as before.
func BeginOutput() {
	consoleMu.Lock()
	lr := activeReader
	// Erase any idle input line BEFORE marking streaming active (eraseLineLocked
	// is a no-op once streamingActive is set).
	if lr != nil && lr.reading {
		lr.eraseLineLocked()
	}
	streamingActive = true
	streamMidLine = false
	canPin := !pinned && lr != nil && lr.reading && pinEnabled()
	if !canPin {
		termPrint("\n")
		consoleMu.Unlock()
		return
	}
	consoleMu.Unlock()
	// The cursor query needs the key loop to relay the answer, which takes
	// consoleMu, so it runs unlocked. Nothing streams before this returns:
	// the turn that called BeginOutput is waiting on it.
	pinAtCursor(true)
}

// BeginPromptInput temporarily suspends streaming-output suppression so an
// interactive prompt (e.g. a permission y/n/a question) can draw and echo the
// input line normally, in the flow of the output. The engine is blocked
// awaiting the answer, so no streaming output is produced meanwhile. A pinned
// input row is released first. Pair with EndPromptInput.
func BeginPromptInput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	breakStreamLineLocked()
	unpinLocked()
	streamingActive = false
	if activeReader != nil && activeReader.reading {
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
	}
}

// EndPromptInput restores streaming-output suppression after an interactive
// prompt has been answered, pinning the input row again when it can.
func EndPromptInput() {
	consoleMu.Lock()
	lr := activeReader
	if lr != nil && lr.reading {
		lr.eraseLineLocked()
	}
	streamingActive = true
	canPin := !pinned && lr != nil && lr.reading && pinEnabled()
	consoleMu.Unlock()
	if canPin {
		pinAtCursor(false)
	}
}

func EndOutput() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	unpinLocked()
	streamingActive = false
	streamMidLine = false
	printOutputLocked("\r\n", false)
	if activeReader != nil && activeReader.reading {
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
	}
}

// breakStreamLineLocked moves to a fresh row if the stream left text on the
// current one. Callers hold consoleMu.
func breakStreamLineLocked() {
	if streamingActive && streamMidLine {
		termPrint("\r\n")
		streamMidLine = false
	}
}

// endsMidLine reports whether the cursor is after text on its row once s has
// been printed, given whether it was before. Escape sequences are not text,
// and a bare \r leaves the row's text in place.
func endsMidLine(was bool, s string) bool {
	mid := was
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case 0x1b:
			// Skip a CSI sequence, the only kind that reaches this point:
			// untrusted text is sanitised before it is printed.
			if i+1 < len(s) && s[i+1] == '[' {
				i += 2
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
			}
		case '\n':
			mid = false
		case '\r':
		default:
			mid = true
		}
	}
	return mid
}

// clearScreenSeq erases the screen and the scrollback and homes the cursor.
const clearScreenSeq = "\x1b[0m\x1b[H\x1b[2J\x1b[3J"

// ClearScreen erases the screen and scrollback. While a turn streams it does
// nothing and reports false: the pinned input row and its scroll region, and
// the half-printed line the stream is writing to, would be wiped with it.
func ClearScreen() bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if streamingActive {
		return false
	}
	termPrint(clearScreenSeq)
	if activeReader != nil && activeReader.reading {
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
	}
	return true
}

// PrintAbove implements termui.Console.
func (lr *LineReader) PrintAbove(s string) { PrintAbove(s) }

// StreamPrint implements termui.Console.
func (lr *LineReader) StreamPrint(s string) { StreamPrint(s) }

// Transient implements termui.Console.
func (lr *LineReader) Transient(s string) { PrintTransientStatus(s) }

// BeginOutput implements termui.Console.
func (lr *LineReader) BeginOutput() { BeginOutput() }

// EndOutput implements termui.Console.
func (lr *LineReader) EndOutput() { EndOutput() }

// noColor reports whether output must carry no colour (the NO_COLOR setting).
//
// The environment is read at first use rather than at init: a test binary
// pins it in TestMain, and an init-time read would already have happened by
// then, so the byte-exact render tests would depend on the ambient NO_COLOR.
func noColor() bool {
	noColorOnce.Do(func() { noColorValue = textmode.NoColor() })
	return noColorValue
}

var (
	noColorOnce  sync.Once
	noColorValue bool
)

// resetNoColor makes the next decision read the environment again. Tests only.
func resetNoColor() {
	noColorOnce = sync.Once{}
	noColorValue = false
}

// termPrint is how this package writes to the terminal: fmt.Print, without
// colour under NO_COLOR (cursor control stays).
func termPrint(a ...any) {
	s := fmt.Sprint(a...)
	if noColor() {
		s = textmode.StripSGR(s)
	}
	fmt.Print(s)
}
