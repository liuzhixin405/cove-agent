package engine

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// signalPattern finds a decision or a finding worth a session note. The
// captured text is group 1. RE2 has no look-around, so what may not come
// right before the match (rejectBefore, checked against the two runes before
// it) or right after its keyword (rejectAfter, the first rune of the
// capture) is checked in code.
type signalPattern struct {
	re           *regexp.Regexp
	rejectBefore string
	rejectAfter  string
}

// zhNegations never precede a real decision or finding: 没/未/不/无 negate
// it ("没有发现问题"), 你/由 hand it to someone else ("由你决定").
const zhNegations = "没未不无你由"

var (
	decisionPatterns = []signalPattern{
		{re: regexp.MustCompile(`(?i)\b(?:use|using|we.ll use|go with|let.s use|switch to|prefer|stick with)\s+(.+?)(?:\.|$)`)},
		{re: regexp.MustCompile(`(?i)(?:I prefer|I like|I want|let.s go with)\s+(.+?)(?:\.|$)`)},
		// Chinese: 改用/采用/决定/换成 anywhere but after a negation.
		{re: regexp.MustCompile(`(?:改用|采用|决定|换成)\s*(.+?)(?:[。！!？?；;\n]|$)`), rejectBefore: zhNegations},
		// A bare 用 only at the start of a clause ("用 tabs 缩进"), not inside
		// words (没有用, 有用) or starting one (用户, 用例, 用法, 用途, 用于,
		// 用来, 用以).
		{re: regexp.MustCompile(`(?:^|[，,。；;！!？?\s])用\s*(.+?)(?:[。！!？?；;\n]|$)`), rejectAfter: "户例法途于来以"},
	}
	discoveryPatterns = []signalPattern{
		{re: regexp.MustCompile(`(?i)\b(?:I found|discovered|the issue is|the reason is|it turns out)\s+(.+?)(?:\.|$)`)},
		{re: regexp.MustCompile(`(?i)(?:fixed by|resolved by|solved by)\s+(.+?)(?:\.|$)`)},
		{re: regexp.MustCompile(`(?:发现|原因是)\s*[:：]?\s*(.+?)(?:[。！!？?；;\n]|$)`), rejectBefore: zhNegations},
	}
)

// signalMinRunes and signalMaxRunes bound a recorded note in characters:
// shorter captures are fragments ("use it."), and bytes used to let a
// Chinese note of 67 characters through as "too long".
const (
	signalMinRunes = 4
	signalMaxRunes = 200
)

func signalNoteOK(text string) bool {
	n := utf8.RuneCountInString(text)
	return n >= signalMinRunes && n < signalMaxRunes
}

// find returns the first acceptable capture of p in s.
func (p signalPattern) find(s string) (string, bool) {
	for _, m := range p.re.FindAllStringSubmatchIndex(s, -1) {
		if p.rejectBefore != "" && strings.ContainsAny(lastRunes(s[:m[0]], 2), p.rejectBefore) {
			continue
		}
		text := strings.TrimSpace(s[m[2]:m[3]])
		if p.rejectAfter != "" && s[m[2]:m[3]] != "" {
			if r, _ := utf8.DecodeRuneInString(s[m[2]:m[3]]); strings.ContainsRune(p.rejectAfter, r) {
				continue
			}
		}
		if signalNoteOK(text) {
			return text, true
		}
	}
	return "", false
}

// lastRunes is the last n runes of s.
func lastRunes(s string, n int) string {
	for i := len(s); i > 0; {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
		if n--; n == 0 {
			return s[i:]
		}
	}
	return s
}

// recordSignals scans user/assistant messages for decisions and
// discoveries, saving them to session notes.
func (e *Engine) recordSignals(userMsg, assistantMsg string) {
	if e.sessionNotes == nil {
		return
	}
	for _, p := range decisionPatterns {
		if text, ok := p.find(userMsg); ok {
			e.sessionNotes.AddDecision(text)
		}
	}
	for _, p := range discoveryPatterns {
		if text, ok := p.find(assistantMsg); ok {
			e.sessionNotes.AddDiscovery(text)
		}
	}
}
