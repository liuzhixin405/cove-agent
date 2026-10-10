package repl

// The relay of typed lines to a waiting prompt (permission, question,
// limit): which lines answer it and which are the next instruction.

import (
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/liuzhixin405/cove-agent/internal/termui"
)

var permInputCh chan<- string

// permAccepts says which typed lines answer the waiting prompt (nil: any
// non-empty line) and permHint is what to show for a line that does not.
var permAccepts func(string) bool

var permHint string

// permAllowEmpty: an empty line answers the waiting prompt (a "reason, or
// Enter to skip" line). Guarded by consoleMu.
var permAllowEmpty bool

var permTitle string
var permPreview []string

// SetPromptInput registers the channel a waiting prompt reads its answer
// from, with the test for what counts as an answer and the hint to show for
// a line that does not. Only answers are relayed: the person may be typing
// the next instruction when a prompt appears, and that line used to be
// swallowed as a refusal.
func SetPromptInput(ch chan<- string, accepts func(string) bool, hint string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	permInputCh = ch
	permAccepts = accepts
	permHint = hint
	permAllowEmpty = false
	permArmedToken = 0
	permTitle, permPreview = "", nil
	permKeys, permOptions, permOptionIdx = "", nil, -1
}

// PromptInputState is what TakePromptInputFor found for a typed line.
type PromptInputState int

const (
	// PromptNone: no prompt is waiting; the line is ordinary input.
	PromptNone PromptInputState = iota
	// PromptAnswer: the line answers the waiting prompt; send it on the
	// returned channel, which is now unregistered.
	PromptAnswer
	// PromptNotAnswer: a prompt is waiting but the line is not an answer to
	// it; the prompt keeps waiting and the line is the caller's to handle.
	PromptNotAnswer
)

// TakePromptInputFor decides what a typed line is for the prompt that may be
// waiting; hint is the prompt's hint when the line is not an answer.
func TakePromptInputFor(line string) (ch chan<- string, state PromptInputState, hint string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if permInputCh == nil {
		return nil, PromptNone, ""
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" && !permAllowEmpty {
		return nil, PromptNotAnswer, permHint
	}
	if trimmed != "" && permAccepts != nil && !permAccepts(trimmed) {
		return nil, PromptNotAnswer, permHint
	}
	ch = permInputCh
	clearPromptLocked()
	return ch, PromptAnswer, ""
}

func ClearPermInputCh() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	clearPromptLocked()
}

// TakePermInputCh takes the waiting prompt's channel unconditionally (Ctrl+C
// answers every prompt); TakePromptInputFor is the typed-line path.
func TakePermInputCh() chan<- string {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	ch := permInputCh
	clearPromptLocked()
	return ch
}

func clearPromptLocked() {
	permInputCh, permAccepts, permHint = nil, nil, ""
	permAllowEmpty = false
	permArmedToken = 0
	permKeys, permOptions, permOptionIdx = "", nil, -1
	permTitle, permPreview = "", nil
}

// permKeys are the keys that answer the waiting prompt on their own, pressed
// on an empty input line ("yapn"); permOptions are the answers Up/Down cycle
// through on the input line (a question's options). Guarded by consoleMu.
var (
	permKeys      string
	permOptions   []string
	permOptionIdx = -1
)

// promptKeyAnswers reports whether r, typed on an empty line, answers the
// waiting prompt by itself.
func promptKeyAnswers(r rune) bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if permInputCh == nil || permKeys == "" || r > 127 {
		return false
	}
	return strings.ContainsRune(permKeys, unicode.ToLower(r))
}

// cycleOption moves the input line to the previous (-1) or next (1) option
// of the waiting prompt; false when the prompt has none or the line holds
// other text (Up/Down then walk the history as usual).
func (lr *LineReader) cycleOption(buf *[]rune, cursor *int, dir int) bool {
	consoleMu.Lock()
	opts := permOptions
	idx := permOptionIdx
	ok := permInputCh != nil && len(opts) > 0
	if ok && len(*buf) > 0 && (idx < 0 || string(*buf) != opts[idx]) {
		ok = false
	}
	if ok {
		switch {
		case idx < 0 && dir > 0:
			idx = 0
		case idx < 0:
			idx = len(opts) - 1
		default:
			idx = (idx + dir + len(opts)) % len(opts)
		}
		permOptionIdx = idx
	}
	consoleMu.Unlock()
	if !ok {
		return false
	}
	*buf = []rune(opts[idx])
	*cursor = len(*buf)
	lr.redraw(*buf, *cursor)
	return true
}

// AskSpec is a prompt for AskWith.
type AskSpec struct {
	Text    string
	Title   string
	Preview []string
	Accepts func(string) bool // nil: any non-empty line
	Hint    string
	Timeout time.Duration
	// Keys answer the prompt on their own on an empty line (no Enter).
	Keys string
	// Options are offered on the input line with Up/Down; Enter sends the
	// one shown.
	Options  []string
	External func() (<-chan ExternalAnswer, func())
	// AllowEmpty makes an empty line an answer ("") instead of a repeat of
	// the hint: for "type a reason, or press Enter to skip".
	AllowEmpty bool
}

type ExternalAnswer struct {
	Answer   string
	Validate func() bool
}

// AskWith is Ask with one-key answers and selectable options.
func AskWith(s AskSpec) (answer string, ok bool) {
	askMu.Lock()
	defer askMu.Unlock()
	ch := make(chan string, 1)
	SetPromptInput(ch, s.Accepts, s.Hint)
	consoleMu.Lock()
	permKeys, permOptions, permOptionIdx = s.Keys, s.Options, -1
	permAllowEmpty = s.AllowEmpty
	permTitle = s.Title
	permPreview = append([]string(nil), s.Preview[:min(len(s.Preview), panelMaxRows)]...)
	consoleMu.Unlock()
	var external <-chan ExternalAnswer
	if s.External != nil {
		var cleanup func()
		external, cleanup = s.External()
		if cleanup != nil {
			defer cleanup()
		}
	}
	BeginPromptInput()
	termui.PrintAbove(s.Text)
	timer := time.NewTimer(s.Timeout)
	defer timer.Stop()
	select {
	case answer = <-ch:
		ok = true
	case result, open := <-external:
		answer = result.Answer
		select {
		case answer = <-ch:
			ok = true
		default:
			ok = open && (result.Validate == nil || result.Validate())
			select {
			case answer = <-ch:
				ok = true
			default:
			}
		}
	case <-timer.C:
	}
	ClearPermInputCh()
	EndPromptInput()
	return answer, ok
}

// askMu serialises prompts: one prompt owns the answer relay at a time. The
// permission, question and limit prompts each registered the relay on their
// own, and only the first and last were serialised (by the engine's prompt
// lock): a question and a permission prompt from parallel tool calls
// registered over each other, and the second one's timeout unregistered
// the first.
var askMu sync.Mutex

// Ask shows text above the input line and waits up to timeout for a typed
// line accepts takes as an answer (nil: any non-empty line); hint is shown
// for a line that is not one. ok is false on timeout. Either way nothing is
// registered afterwards, so a later line is an ordinary one.
func Ask(text string, accepts func(string) bool, hint string, timeout time.Duration) (answer string, ok bool) {
	return AskWith(AskSpec{Text: text, Accepts: accepts, Hint: hint, Timeout: timeout})
}

// ArmPrompt registers a prompt whose answer the front end reads from the
// next typed line itself: a slash command runs on the input loop, so it
// cannot block in AskWith waiting for that loop to deliver the answer. The
// keys answer on their own (no Enter), the panel shows title and preview,
// and the status row says the prompt is waiting. DisarmPrompt ends it; the
// front end must call it when it takes the line.
//
// It refuses (ok false) while another prompt is waiting: overwriting a
// permission prompt's registration would send its answer to the wrong
// place and leave the tool waiting for its timeout. The token identifies
// this arming; DisarmPrompt with a stale token clears nothing.
func ArmPrompt(title string, preview []string, hint, keys string) (token uint64, ok bool) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if permInputCh != nil {
		return 0, false
	}
	// The channel is never read: the line goes to the front end instead.
	permInputCh = make(chan string, 1)
	permAccepts, permHint, permAllowEmpty = nil, hint, false
	permKeys, permOptions, permOptionIdx = keys, nil, -1
	permTitle = title
	permPreview = append([]string(nil), preview[:min(len(preview), panelMaxRows)]...)
	armedSeq++
	permArmedToken = armedSeq
	if activeReader != nil && activeReader.reading {
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
	}
	return permArmedToken, true
}

// permArmedToken identifies the prompt ArmPrompt registered (0: none, or a
// prompt AskWith registered since). Guarded by consoleMu.
var (
	permArmedToken uint64
	armedSeq       uint64
)

// DisarmPrompt ends the prompt ArmPrompt registered under token, and only
// that one: a permission prompt registered since stays.
func DisarmPrompt(token uint64) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if token == 0 || permArmedToken != token {
		return
	}
	clearPromptLocked()
}

// promptWaiting reports whether any prompt is registered on the relay.
func promptWaiting() bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	return permInputCh != nil
}

// PromptKeyAnswers reports whether r, typed on an empty line, answers the
// waiting prompt by itself (for front-end tests).
func PromptKeyAnswers(r rune) bool { return promptKeyAnswers(r) }
