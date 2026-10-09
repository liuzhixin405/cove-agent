package repl

// Tab completion and the inline command hints.

import (
	"fmt"
	"strings"
	"unicode"
)

type Choice struct {
	Value       string
	Label       string
	Description string
	Preview     string
}

func (lr *LineReader) ShowChoices(title string, choices []Choice) bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if len(choices) == 0 || permInputCh != nil || lr.fallbackReader != nil {
		return false
	}
	lr.choices = append([]Choice(nil), choices...)
	lr.choiceTitle, lr.choiceSearch, lr.choiceIndex, lr.choiceQuery = title, true, 0, ""
	lr.completionBase, lr.completionList, lr.completionIdx = "", nil, -1
	lr.panelFocused = false
	if lr.reading {
		lr.redrawLocked(lr.renderBuf, lr.renderCursor)
	}
	return true
}

func (lr *LineReader) visibleChoicesLocked() []Choice {
	if permInputCh != nil {
		return nil
	}
	if !lr.choiceSearch {
		return lr.choices
	}
	query := strings.ToLower(strings.TrimSpace(string(lr.renderBuf)))
	if query != lr.choiceQuery {
		lr.choiceIndex, lr.choiceQuery = 0, query
	}
	var matches []Choice
	for _, choice := range lr.choices {
		if strings.Contains(strings.ToLower(choice.Label+" "+choice.Description), query) {
			matches = append(matches, choice)
		}
	}
	return matches
}

func choiceText(text string) string {
	return strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return ' '
		}
		return value
	}, text)
}

func (lr *LineReader) displayRowsLocked() []string {
	if permInputCh != nil && len(permPreview) > 0 {
		lines := make([]string, len(permPreview))
		for index, line := range permPreview {
			lines[index] = choiceText(line)
		}
		return lines
	}
	if len(lr.choices) == 0 || permInputCh != nil {
		return lr.panelLines
	}
	choices := lr.visibleChoicesLocked()
	if len(choices) == 0 {
		return padChoiceRows([]string{choiceText(lr.choiceTitle) + " | 无匹配"})
	}
	lr.choiceIndex = max(0, min(lr.choiceIndex, len(choices)-1))
	lines := []string{fmt.Sprintf("%s | %d/%d", choiceText(lr.choiceTitle), lr.choiceIndex+1, len(choices))}
	start := max(0, lr.choiceIndex-1)
	for offset := 0; offset < min(5, len(choices)); offset++ {
		index := start + offset
		if index >= len(choices) {
			lines = append(lines, "")
			continue
		}
		choice := choices[index]
		marker := "  "
		if index == lr.choiceIndex {
			marker = "> "
		}
		line := marker + choiceText(choice.Label)
		if choice.Description != "" {
			line += " | " + choiceText(choice.Description)
		}
		lines = append(lines, line)
	}
	if preview := choices[lr.choiceIndex].Preview; preview != "" {
		lines = append(lines, "预览: "+choiceText(preview))
	}
	return padChoiceRows(lines)
}

// choiceBlockRows is the height of the candidate list while the input is
// pinned: title, five candidates and a preview row.
const choiceBlockRows = 7

// padChoiceRows keeps the candidate block the same height while the input
// line is pinned. The pinned region reserves rows below the input, and a
// change in their number means unpinning, asking the terminal for the cursor
// and pinning again: with the block growing and shrinking as the filter
// narrowed, every keystroke of "/hel" did that round trip (flicker, output
// landing on the input row meanwhile, and a missed cursor answer that
// disabled pinning). Open and close still repin; typing does not.
func padChoiceRows(lines []string) []string {
	if !pinned {
		return lines
	}
	for len(lines) < choiceBlockRows {
		lines = append(lines, "")
	}
	return lines
}

func (lr *LineReader) moveChoice(delta int) bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	choices := lr.visibleChoicesLocked()
	if len(choices) == 0 {
		return len(lr.choices) > 0 && permInputCh == nil
	}
	lr.choiceIndex = max(0, min(lr.choiceIndex+delta, len(choices)-1))
	lr.redrawLocked(lr.renderBuf, lr.renderCursor)
	return true
}

func (lr *LineReader) acceptChoice(buf *[]rune, cursor *int) (bool, bool) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	choices := lr.visibleChoicesLocked()
	if len(choices) == 0 {
		return lr.choiceSearch && permInputCh == nil, false
	}
	choice := choices[max(0, min(lr.choiceIndex, len(choices)-1))]
	search := lr.choiceSearch
	lr.choices, lr.choiceSearch = nil, false
	lr.completionBase, lr.completionList, lr.completionIdx = "", nil, -1
	if !search && (choice.Value == string(*buf) || string(*buf) == "/") {
		// "/" alone is the request for the quick command list; filling in
		// the first candidate made that list unreachable without Esc.
		return false, false
	}
	*buf, *cursor = []rune(choice.Value), len([]rune(choice.Value))
	return true, search
}

func (lr *LineReader) dismissChoices(buf *[]rune, cursor *int) bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if len(lr.choices) == 0 {
		return false
	}
	if lr.choiceSearch {
		*buf, *cursor = nil, 0
	}
	lr.choices, lr.choiceSearch = nil, false
	lr.completionBase, lr.completionList, lr.completionIdx = "", nil, -1
	return true
}

func (lr *LineReader) showInlineSuggestions(suggestions []string, offset int) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	// Candidates are whole lines; the hint shows only the word being
	// completed ("@internal/" rather than the sentence before it).
	line := string(lr.renderBuf)
	lead := line[:strings.LastIndexAny(line, " \t")+1]
	lr.choices = nil
	for _, suggestion := range suggestions {
		value, description, _ := strings.Cut(suggestion, "\t")
		lr.choices = append(lr.choices, Choice{Value: value, Label: strings.TrimPrefix(value, lead), Description: description})
	}
	lr.choiceTitle, lr.choiceIndex, lr.choiceSearch = "候选", 0, false
	lr.redrawLocked(lr.renderBuf, lr.renderCursor)
}

func (lr *LineReader) showCommandCountHint(count int, offset int) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	hint := fmt.Sprintf("\x1b[90m  (按 Tab 查看 %d 个命令)\x1b[0m", count)
	lr.activeHint = hint
	lr.redrawLocked(lr.renderBuf, lr.renderCursor)
}

func (lr *LineReader) complete(buf *[]rune, cursor *int) {
	if lr.advanceCompletionCycle(buf, cursor) {
		return
	}
	if lr.completer == nil {
		return
	}
	line := string(*buf)
	suggestions := lr.completer(line)
	if len(suggestions) == 0 {
		lr.resetCompletionCycle()
		return
	}
	texts := completionTexts(suggestions)
	// The cursor indexes the rune buffer, so it is the completion's rune
	// count. It used to be its byte length: "请看 @internal/" (16 bytes, 12
	// runes) put the cursor past the end and the redraw panicked.
	if len(suggestions) == 1 {
		*buf = []rune(texts[0])
		*cursor = len(*buf)
		lr.resetCompletionCycle()
		lr.redraw(*buf, *cursor)
		return
	}
	common := commonPrefix(texts)
	if len(common) > len(line) {
		*buf = []rune(common)
		*cursor = len(*buf)
		lr.completionBase, lr.completionList, lr.completionIdx = common, texts, -1
		lr.redraw(*buf, *cursor)
		return
	}
	lr.completionBase, lr.completionList, lr.completionIdx = line, texts, -1
	lr.showInlineSuggestions(suggestions, lr.promptWidth+*cursor)
}

func (lr *LineReader) advanceCompletionCycle(buf *[]rune, cursor *int) bool {
	next, idx, ok := completionCycleNext(string(*buf), lr.completionBase, lr.completionList, lr.completionIdx)
	if !ok {
		lr.resetCompletionCycle()
		return false
	}
	lr.completionIdx = idx
	consoleMu.Lock()
	lr.choiceIndex = idx
	consoleMu.Unlock()
	*buf = []rune(next)
	*cursor = len(*buf) // runes, not bytes (see complete)
	lr.redraw(*buf, *cursor)
	return true
}

func completionCycleNext(line, base string, list []string, idx int) (string, int, bool) {
	if len(list) == 0 {
		return "", idx, false
	}
	if line != base {
		current := ""
		if idx >= 0 && idx < len(list) {
			current = list[idx]
		}
		if line != current {
			return "", idx, false
		}
	}
	nextIdx := (idx + 1) % len(list)
	return list[nextIdx], nextIdx, true
}

func (lr *LineReader) resetCompletionCycle() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	lr.completionBase, lr.completionList, lr.completionIdx = "", nil, -1
	if !lr.choiceSearch {
		lr.choices = nil
	}
}

func completionTexts(ss []string) []string {
	res := make([]string, len(ss))
	for i, s := range ss {
		text := s
		if idx := strings.IndexByte(s, '\t'); idx >= 0 {
			text = s[:idx]
		}
		res[i] = text
	}
	return res
}

// commonPrefix is the longest prefix of whole characters the candidates
// share. It used to trim a byte at a time, so "@档案" and "@案卷" (档 and 案
// share their first two bytes) left half a character, shown as U+FFFD.
func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := []rune(ss[0])
	for _, s := range ss[1:] {
		n := 0
		for _, r := range s {
			if n >= len(p) || p[n] != r {
				break
			}
			n++
		}
		p = p[:n]
	}
	return string(p)
}
