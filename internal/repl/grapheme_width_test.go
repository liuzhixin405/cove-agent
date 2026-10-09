package repl

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// Emoji presentation sequences: a text-default base plus U+FE0F (VS16), or a
// keycap. Summed rune by rune they measure 1 (base 1, selector 0) but the
// terminal draws them 2 wide, so a row full of them overflowed, soft-wrapped
// and left a ghost row on the next redraw.
var vs16Sequences = []string{"⚠️", "❤️", "✔️", "1️⃣", "👨‍👩‍👧"}

func TestInputDisplayWindowMeasuresEmojiSequences(t *testing.T) {
	for _, seq := range vs16Sequences {
		buf := []rune(strings.Repeat(seq, 30))
		for _, cursor := range []int{0, len(buf) / 2, len(buf)} {
			disp, _, used, _ := inputDisplayWindow(buf, cursor, 20)
			if w := textutil.Width(string(disp)); w > 20 || w != used {
				t.Errorf("%q cursor %d: window %q is %d columns (reported %d), budget 20", seq, cursor, string(disp), w, used)
			}
		}
	}
}

func TestLayoutInputMeasuresEmojiSequences(t *testing.T) {
	for _, seq := range vs16Sequences {
		buf := []rune(strings.Repeat(seq, 30))
		rows, _, _ := layoutInput("> ", 2, buf, len(buf), 20)
		for i, r := range rows {
			if w := textutil.Width(string(r.text)); w > 20-1-2 {
				t.Errorf("%q row %d is %d columns, budget %d: %q", seq, i, w, 20-1-2, string(r.text))
			}
		}
	}
}

func TestPinnedInputLineMeasuresEmojiSequences(t *testing.T) {
	for _, seq := range vs16Sequences {
		buf := []rune(strings.Repeat(seq, 30))
		for _, cursor := range []int{0, len(buf)} {
			line := pinnedInputLine("> ", 2, buf, cursor, 20, "")
			// The caret's reverse-video codes must not split a sequence
			// either, so measure the text with them stripped.
			plain := strings.NewReplacer("\x1b[7m", "", "\x1b[27m", "", "\x1b[0m", "").Replace(line)
			if w := textutil.Width(plain); w >= 20 {
				t.Errorf("%q cursor %d: pinned row is %d columns in a 20-column window: %q", seq, cursor, w, line)
			}
		}
	}
}

func TestNeedsMultiRowMeasuresEmojiSequences(t *testing.T) {
	lr := &LineReader{promptWidth: 2}
	// 10 sequences are 20 columns: more than the 17 a 20-column row leaves.
	if !lr.needsMultiRow([]rune(strings.Repeat("⚠️", 10)), 20, 24) {
		t.Fatal("a 20-column emoji line was judged to fit a 17-column row")
	}
}
