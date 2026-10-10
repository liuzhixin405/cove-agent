package repl

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/term"
)

// ReadSecret reads one line without showing it: an API key. Each typed
// character is echoed as "•", Backspace removes one, Enter ends the line.
// Nothing goes into the history. Without a raw-mode terminal the line is
// read plainly, after saying so. /api-key used to echo the key into the
// transcript like any other line.
func (lr *LineReader) ReadSecret(prompt string) (string, error) {
	// Output arriving meanwhile (a race notice from ownerPoll) is held
	// until the line is done, or it would be drawn into the masked prompt.
	beginSecretHold()
	defer endSecretHold()
	if shouldUseFallbackReadline() {
		termPrint("（当前终端不支持掩码输入，输入内容会显示）\r\n")
		return lr.plainLine(prompt)
	}
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		// A one-off plain read must not leave the editor in fallback mode:
		// ShowChoices treats a live fallbackReader as "no panel, ever".
		termPrint("（当前终端不支持掩码输入，输入内容会显示）\r\n")
		defer lr.restoreFallbackReaderAfterSecret(lr.fallbackReader)
		return lr.plainLine(prompt)
	}
	defer func() {
		termPrint("\x1b[0m\x1b[?25h")
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}()
	if lr.rawReader == nil {
		lr.rawReader = bufio.NewReaderSize(ownerInput{source: os.Stdin, lr: lr}, rawInputBufferSize)
	}
	return lr.readSecretRaw(prompt)
}

// readSecretRaw is ReadSecret on an already raw lr.rawReader.
func (lr *LineReader) readSecretRaw(prompt string) (string, error) {
	termPrint("\r\x1b[2K" + prompt)
	var buf []rune
	for {
		r, err := readInputRune(lr.rawReader)
		if err != nil {
			termPrint("\r\n")
			return string(buf), err
		}
		switch {
		case r == 3:
			termPrint("\r\n")
			return "", ErrInterrupt
		case r == 4 && len(buf) == 0:
			termPrint("\r\n")
			return "", ErrExit
		case r == '\r' || r == '\n':
			// A pasted CRLF: drop the LF, or the next ReadLine gets an empty line.
			if r == '\r' {
				lr.skipRune('\n')
			}
			termPrint("\r\n")
			return string(buf), nil
		case r == 127 || r == 8:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				termPrint("\b \b")
			}
		case r == 27:
			// Drop an escape sequence (arrow keys) instead of typing it.
			for lr.rawReader.Buffered() > 0 {
				if _, err := readInputRune(lr.rawReader); err != nil {
					break
				}
			}
		case r >= 32:
			buf = append(buf, r)
			termPrint("•")
		}
	}
}

// restoreFallbackReaderAfterSecret puts back the fallback reader the editor
// had before a plain secret read (nil when it was in raw mode), so the one
// read does not switch the editor into fallback mode for good.
func (lr *LineReader) restoreFallbackReaderAfterSecret(prev *bufio.Reader) {
	if prev == nil {
		lr.fallbackReader = nil
	}
}

// plainLine reads one cooked-mode line with prompt, without history.
func (lr *LineReader) plainLine(prompt string) (string, error) {
	if lr.fallbackReader == nil {
		lr.fallbackReader = bufio.NewReader(ownerInput{source: os.Stdin, lr: lr})
	}
	termPrint(prompt)
	line, err := lr.fallbackReader.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}
