package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

type EditTool struct{ baseTool }

func NewEditTool() Tool {
	return &EditTool{baseTool{def: Def{
		Name: "edit", Aliases: []string{"Edit"},
		Description: "Make exact string replacements in files. Fails if oldString is not found or matches multiple times (use replaceAll for multiple).",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"filePath":{"type":"string","description":"Absolute path to the file to edit"},
				"oldString":{"type":"string","description":"The exact text to replace"},
				"newString":{"type":"string","description":"The text to replace with (must differ from oldString)"},
				"replaceAll":{"type":"boolean","description":"Replace all occurrences"}
			},
			"required":["filePath","oldString","newString"]
		}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "Edit",
	}}}
}

func (t *EditTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	path, _ := input["filePath"].(string)
	oldS, _ := input["oldString"].(string)
	newS, _ := input["newString"].(string)
	all, _ := input["replaceAll"].(bool)

	// Fallback: models sometimes use alternative key names. Accept the same
	// aliases as WriteTool so the two behave identically — the engine's
	// same-file write serialization keys off these aliases too.
	if path == "" {
		for _, alt := range []string{"file_path", "path", "filepath", "file"} {
			if v, ok := input[alt].(string); ok && v != "" {
				path = v
				break
			}
		}
	}

	if path == "" {
		return Result{Data: "Error: filePath required", IsError: true}, nil
	}
	if oldS == newS {
		return Result{Data: "Error: oldString and newString must differ", IsError: true}, nil
	}
	if oldS == "" {
		return Result{Data: "Error: oldString is empty", IsError: true}, nil
	}

	var err error
	path, err = resolvePathInCwd(path, tctx, true)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Data: "Error: file not found: " + path, IsError: true}, nil
		}
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	files := fileTracker(tctx)
	if err := files.Check(path, data); err != nil {
		return Result{Data: "Error: " + err.Error() + ", then retry the edit.", IsError: true}, nil
	}

	content := string(data)
	// A GBK file is edited as text and encoded back, so the whole file stays
	// in one encoding; anything else that is not UTF-8 is refused, because
	// the model's UTF-8 would leave the file in two.
	var legacy *legacyText
	if !utf8.Valid(data) {
		lt, ok := decodeGBK(data)
		if !ok {
			return Result{Data: "Error: " + path + " is not valid UTF-8, and not GBK either (probably another ANSI code page). Editing it would mix encodings in one file; convert it to UTF-8 first (e.g. iconv -f <encoding> -t UTF-8) or change it with a tool that keeps its encoding.", IsError: true}, nil
		}
		legacy, content = &lt, lt.text
	}
	// The model saw the file without its \r (the read tool strips it), so
	// its strings are LF. Match and write them in the file's own line ending.
	if usesCRLF(content) {
		oldS, newS = toCRLF(oldS), toCRLF(newS)
	}
	count := strings.Count(content, oldS)

	var result string
	replacements := 0
	// spots are the byte offsets in result where replaced text starts (at most
	// editSnippetMax of them), inserted is that text; both feed the snippet.
	var spots []int
	inserted := newS
	note := ""

	switch {
	case count == 1, count > 1 && all:
		// Exact match path — unchanged behavior, zero risk to the existing
		// (already working) case.
		positions := matchOffsets(content, oldS, editSnippetMax)
		if all {
			result = strings.ReplaceAll(content, oldS, newS)
			replacements = count
		} else {
			result = strings.Replace(content, oldS, newS, 1)
			replacements = 1
			positions = positions[:1]
		}
		// Every earlier replacement shifts the later ones by the length change.
		for k, p := range positions {
			spots = append(spots, p+k*(len(newS)-len(oldS)))
		}
	case count > 1:
		return Result{Data: fmt.Sprintf("Error: oldString matches %d times (%s). Use replaceAll=true or provide more context.",
			count, describeMatchLines(content, oldS)), IsError: true}, nil
	default:
		// count == 0: mid-tier models frequently produce an oldString that is
		// semantically right but byte-for-byte wrong (extra/missing spaces,
		// tab vs. space indentation, CRLF vs LF, a trailing blank line). Try a
		// whitespace-normalized, line-aware match before giving up outright.
		fo := fuzzyApplyEdit(content, oldS, newS)
		switch {
		case fo.applied:
			result = fo.result
			replacements = 1
			spots = []int{fo.start}
			inserted = fo.inserted
			note = fmt.Sprintf(" (fuzzy whitespace match at lines %d-%d", fo.firstLine, fo.lastLine)
			if fo.reindented {
				note += "; newString re-indented to the file's indentation"
			}
			note += ")"
		case fo.refused:
			return Result{Data: fo.hint, IsError: true}, nil
		default:
			msg := "Error: oldString not found in file."
			if fo.hint != "" {
				msg += " " + fo.hint
			}
			return Result{Data: msg, IsError: true}, nil
		}
	}

	out := []byte(result)
	if legacy != nil {
		if out, err = legacy.encode(result); err != nil {
			return legacy.encodeFailure(path, "newString", err), nil
		}
	}

	if err := ctx.Err(); err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	if err := replaceFile(path, out); err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	files.Record(path, out)

	msg := fmt.Sprintf("Edited %s: %d replacement(s)%s", path, replacements, note)
	if snip := editSnippets(result, spots, len(inserted)); snip != "" {
		msg += "\n" + snip
		if replacements > len(spots) {
			msg += fmt.Sprintf("\n(first %d of %d replacements shown)", len(spots), replacements)
		}
	}
	return Result{Data: msg}, nil
}

// editSnippetMax caps how many replaced regions the success message shows
// (replaceAll can touch hundreds) and how many match lines an error lists.
const editSnippetMax = 3

// editSnippetContext is how many unchanged lines are shown around a change.
const editSnippetContext = 2

// matchOffsets returns the byte offsets of the first max non-overlapping
// occurrences of sub in s — the same ones strings.Replace/ReplaceAll hit.
func matchOffsets(s, sub string, maxCount int) []int {
	var out []int
	for from := 0; len(out) < maxCount; {
		i := strings.Index(s[from:], sub)
		if i < 0 {
			break
		}
		out = append(out, from+i)
		from += i + len(sub)
	}
	return out
}

// describeMatchLines names the lines where oldString occurs, so the model can
// add context that tells them apart: "lines 1, 3, 5".
func describeMatchLines(content, oldS string) string {
	const maxListed = 20
	offsets := matchOffsets(content, oldS, maxListed+1)
	parts := make([]string, 0, len(offsets))
	line, last := 1, 0
	for i, off := range offsets {
		if i == maxListed {
			parts = append(parts, "...")
			break
		}
		line += strings.Count(content[last:off], "\n")
		last = off
		parts = append(parts, fmt.Sprintf("%d", line))
	}
	return "lines " + strings.Join(parts, ", ")
}

// editSnippets renders each changed region of result with editSnippetContext
// lines around it, numbered like the read tool's output, so the model can see
// the edit landed where it meant without reading the file again.
func editSnippets(result string, spots []int, insertedLen int) string {
	if len(spots) == 0 {
		return ""
	}
	lines := strings.Split(result, "\n")
	// A trailing newline yields a final empty element that is not a line.
	if n := len(lines); n > 1 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	var sb strings.Builder
	for i, start := range spots {
		if start < 0 || start > len(result) {
			continue
		}
		end := start + insertedLen
		if end > len(result) {
			end = len(result)
		}
		first := strings.Count(result[:start], "\n")
		last := first + strings.Count(result[start:end], "\n")
		if end > start && result[end-1] == '\n' && last > first {
			last-- // the inserted text's final newline ends its last line
		}
		from := first - editSnippetContext
		if from < 0 {
			from = 0
		}
		to := last + editSnippetContext
		if to > len(lines)-1 {
			to = len(lines) - 1
		}
		if i > 0 {
			sb.WriteString("---\n")
		}
		for ln := from; ln <= to; ln++ {
			fmt.Fprintf(&sb, "%d: %s\n", ln+1, strings.TrimRight(lines[ln], "\r"))
		}
	}
	return truncateForHint(strings.TrimRight(sb.String(), "\n"), 4000)
}

func (t *EditTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Asked("edit modifies file content")
}

// ──── Fuzzy fallback for near-miss oldString values ────
//
// This only runs after an exact strings.Count match has already failed
// (count == 0), so it can never change behavior for the common case where
// the model got the text exactly right. It has two tiers:
//
//  1. Normalized, unambiguous match: whitespace-insensitive comparison finds
//     exactly one candidate location → safe to auto-apply, since collapsing
//     whitespace runs and trimming line edges essentially never changes the
//     *meaning* of the located block, only how confidently we could match it
//     byte-for-byte.
//  2. Everything else: never auto-applied. If a reasonably similar block is
//     found (or several equally-normalized candidates, which would be
//     genuinely ambiguous), the real file content and line numbers are
//     reported back so the model can retry with exact text instead of
//     guessing blindly again.
//
// Large-file guard: bails out with no hint (falls back to the plain "not
// found" message) above fuzzyMaxLines, so this can't become a perf cliff on
// huge generated/vendored files.
const fuzzyMaxLines = 20000

// fuzzyOutcome is what the fuzzy fallback decided.
type fuzzyOutcome struct {
	result  string // the new content, when applied
	applied bool
	// refused: a unique block matched, but newString cannot be applied
	// safely; hint is then the complete error message.
	refused bool
	hint    string
	// start is the byte offset in result where inserted (newString after
	// re-indentation) begins; firstLine/lastLine are the matched range in
	// the original, 1-based.
	start               int
	inserted            string
	firstLine, lastLine int
	reindented          bool
}

func fuzzyApplyEdit(content, oldS, newS string) fuzzyOutcome {
	oldLines := strings.Split(oldS, "\n")
	includeTrailingNL := false
	if len(oldLines) > 1 && oldLines[len(oldLines)-1] == "" && strings.HasSuffix(oldS, "\n") {
		oldLines = oldLines[:len(oldLines)-1]
		includeTrailingNL = true
	}
	n := len(oldLines)
	if n == 0 {
		return fuzzyOutcome{}
	}

	normOld := make([]string, n)
	for i, l := range oldLines {
		normOld[i] = normalizeLine(l)
	}

	lines, widths := splitLinesWithWidths(content)
	if len(lines) == 0 || len(lines) > fuzzyMaxLines || n > len(lines) {
		return fuzzyOutcome{}
	}

	var matchStarts []int
	for start := 0; start+n <= len(lines); start++ {
		match := true
		for k := 0; k < n; k++ {
			if normalizeLine(lines[start+k]) != normOld[k] {
				match = false
				break
			}
		}
		if match {
			matchStarts = append(matchStarts, start)
		}
	}

	if len(matchStarts) == 1 {
		s := matchStarts[0]
		byteStart := 0
		for i := 0; i < s; i++ {
			byteStart += widths[i]
		}
		regionWidth := 0
		for i := s; i < s+n-1; i++ {
			regionWidth += widths[i]
		}
		if includeTrailingNL {
			regionWidth += widths[s+n-1]
		} else {
			// Stop before the last line's "\r" when it ends a CRLF line:
			// lines keep it, and replacing it with a newString that lacks it
			// left one LF line among CRLF ones.
			last := lines[s+n-1]
			if widths[s+n-1] > len(last) {
				last = strings.TrimSuffix(last, "\r")
			}
			regionWidth += len(last)
		}
		byteEnd := byteStart + regionWidth
		if byteStart < 0 || byteEnd > len(content) || byteStart > byteEnd {
			return fuzzyOutcome{} // defensive: never slice out of range
		}
		// The block was found despite whitespace differences, which usually
		// means the model's indentation (tabs, width) is not the file's.
		// newString is written in the file's indentation, or refused when
		// that cannot be worked out: mixed indentation would compile in Go
		// but break Python/YAML and every diff.
		text, ok, changed := reindentForFile(lines[s:s+n], oldLines, newS, fileIndentUnit(lines))
		if !ok {
			return fuzzyOutcome{refused: true, hint: fmt.Sprintf(
				"Error: oldString matched lines %d-%d only after whitespace normalization, and the indentation there does not map consistently onto oldString's (mixed tabs/spaces or uneven levels), so a multi-line newString cannot be re-indented safely. The file was not changed; read lines %d-%d again and retry with their exact text.",
				s+1, s+n, s+1, s+n)}
		}
		return fuzzyOutcome{
			result:     content[:byteStart] + text + content[byteEnd:],
			applied:    true,
			start:      byteStart,
			inserted:   text,
			firstLine:  s + 1,
			lastLine:   s + n,
			reindented: changed,
		}
	}

	if len(matchStarts) > 1 {
		return fuzzyOutcome{hint: fmt.Sprintf(
			"Found %d whitespace-normalized matches (ambiguous) at lines %s. Add more surrounding context to oldString to disambiguate.",
			len(matchStarts), formatLineRanges(matchStarts, n))}
	}

	// No exact normalized match either. Look for the single most similar
	// window purely to help the model retry — never applied automatically.
	bestStart, bestScore := -1, 0.0
	for start := 0; start+n <= len(lines); start++ {
		score := windowSimilarity(lines[start:start+n], normOld)
		if score > bestScore {
			bestScore = score
			bestStart = start
		}
	}
	var hint string
	if bestStart >= 0 && bestScore >= 0.6 {
		snippet := strings.Join(lines[bestStart:bestStart+n], "\n")
		hint = fmt.Sprintf(
			"Closest candidate (%.0f%% similar) is at lines %d-%d:\n---\n%s\n---\nCompare carefully against oldString (spacing, quotes, identifiers) and retry with the exact text.",
			bestScore*100, bestStart+1, bestStart+n, truncateForHint(snippet, 800))
	}
	return fuzzyOutcome{hint: hint}
}

// reindentForFile rewrites newS's leading indentation from oldString's style
// into the style of the matched file lines. ok=false means the mapping is
// ambiguous and newS spans several lines, so the edit must be refused;
// changed reports whether newS was altered.
//
// Indentation is compared in levels: a tab is one level, and so is a run of
// the text's space unit: for the file block see blockUnit (its own unit,
// or the whole file's fileUnitHint when a single-depth block says nothing);
// for oldString/newString it is spaceUnit of their lines. The file block must use one
// character for indentation, and every non-blank line must sit the same
// number of levels deeper (or shallower) in the file than in oldString —
// models often drop the block's common leading indentation.
func reindentForFile(fileLines, oldLines []string, newS string, fileUnitHint int) (text string, ok, changed bool) {
	var fileInd, oldInd []string
	identical := true
	for i := range oldLines {
		if isBlankLine(oldLines[i]) || isBlankLine(fileLines[i]) {
			continue
		}
		f, o := leadingWS(fileLines[i]), leadingWS(oldLines[i])
		fileInd, oldInd = append(fileInd, f), append(oldInd, o)
		if f != o {
			identical = false
		}
	}
	if identical {
		// Only inner whitespace differed; newS is already in the file's style.
		return newS, true, false
	}
	multiLine := strings.Contains(strings.TrimRight(newS, "\r\n"), "\n")
	fail := func() (string, bool, bool) {
		if multiLine {
			return "", false, false
		}
		// A single line cannot end up with mixed indentation within itself;
		// keep the old behaviour of writing it as given.
		return newS, true, false
	}

	newLines := strings.Split(newS, "\n")
	var newInd []string
	for _, l := range newLines {
		if !isBlankLine(l) {
			newInd = append(newInd, leadingWS(l))
		}
	}

	// The file block's indentation character.
	tabs, spaces := false, false
	for _, ws := range fileInd {
		t := strings.Count(ws, "\t")
		if t > 0 {
			tabs = true
		}
		if t < len(ws) {
			spaces = true
		}
	}
	if tabs && spaces {
		return fail()
	}
	modelUnit := spaceUnit(append(append([]string(nil), oldInd...), newInd...))
	fileUnit := blockUnit(fileInd, oldInd, modelUnit, fileUnitHint)
	unitStr := "\t"
	switch {
	case spaces:
		unitStr = strings.Repeat(" ", fileUnit)
	case !tabs:
		// The block is not indented at all; deeper newString lines follow
		// the model's own style.
		if modelUnit > 0 {
			unitStr = strings.Repeat(" ", modelUnit)
		}
	}

	delta, haveDelta := 0, false
	for i := range fileInd {
		fl, ok1 := indentLevel(fileInd[i], fileUnit)
		ol, ok2 := indentLevel(oldInd[i], modelUnit)
		if !ok1 || !ok2 {
			return fail()
		}
		if !haveDelta {
			delta, haveDelta = fl-ol, true
		} else if fl-ol != delta {
			return fail()
		}
	}

	for i, l := range newLines {
		if isBlankLine(l) {
			continue
		}
		ws := leadingWS(l)
		lvl, ok := indentLevel(ws, modelUnit)
		if !ok || lvl+delta < 0 {
			return fail()
		}
		newLines[i] = strings.Repeat(unitStr, lvl+delta) + l[len(ws):]
	}
	text = strings.Join(newLines, "\n")
	return text, true, text != newS
}

func isBlankLine(s string) bool { return strings.TrimSpace(s) == "" }

// leadingWS returns the run of spaces and tabs s starts with.
func leadingWS(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[:i]
}

// indentLevel converts leading whitespace to a level: one per tab, one per
// unit spaces. ok=false when the spaces are not a whole number of units.
func indentLevel(ws string, unit int) (int, bool) {
	tabs := strings.Count(ws, "\t")
	spaces := len(ws) - tabs
	if spaces == 0 {
		return tabs, true
	}
	if unit <= 0 || spaces%unit != 0 {
		return 0, false
	}
	return tabs + spaces/unit, true
}

// spaceUnit guesses how many spaces make one indentation level: the greatest
// common divisor of the space counts, or — when every line has the same
// count, which says nothing about the unit — 4 or 2 when they divide it.
// 0 means no line is indented with spaces.
func spaceUnit(indents []string) int {
	g := 0
	distinct := map[int]bool{}
	for _, ws := range indents {
		n := len(ws) - strings.Count(ws, "\t")
		if n > 0 {
			g = gcd(g, n)
			distinct[n] = true
		}
	}
	if len(distinct) == 1 {
		switch {
		case g%4 == 0:
			return 4
		case g%2 == 0:
			return 2
		}
	}
	return g
}

// fileIndentUnit infers a file's space indentation unit from all of its
// lines: the most common positive step between consecutive non-blank lines
// indented with spaces only (ties go to the smaller step). 0 when there is no
// such step (tab-indented or flat text).
func fileIndentUnit(lines []string) int {
	counts := map[int]int{}
	prev := -1
	for _, l := range lines {
		if isBlankLine(l) {
			continue
		}
		ws := leadingWS(l)
		if strings.Contains(ws, "\t") {
			prev = -1
			continue
		}
		if prev >= 0 && len(ws) > prev {
			counts[len(ws)-prev]++
		}
		prev = len(ws)
	}
	best, bestN := 0, 0
	for step, c := range counts {
		if c > bestN || (c == bestN && step < best) {
			best, bestN = step, c
		}
	}
	return best
}

// blockUnit picks the space unit of the matched file block. A block with two
// or more space depths carries its own unit (spaceUnit), which the file-wide
// hint must not override. A single-depth block says nothing about the unit
// (spaceUnit only guesses 4 or 2), so there the whole file's unit is used —
// but only when it maps the block onto exactly the depth, in levels, that
// oldString has; otherwise the block's own guess stays.
func blockUnit(fileInd, oldInd []string, modelUnit, hint int) int {
	own := spaceUnit(fileInd)
	depths := map[int]bool{}
	for _, ws := range fileInd {
		if n := len(ws) - strings.Count(ws, "\t"); n > 0 {
			depths[n] = true
		}
	}
	if len(depths) != 1 || !fitsUnit(fileInd, hint) || len(oldInd) == 0 {
		return own
	}
	// Compare the first indented line: a line at column 0 is level 0 in every
	// unit, so comparing it would accept any hint.
	for i, ws := range fileInd {
		if ws == "" {
			continue
		}
		if i >= len(oldInd) {
			return own
		}
		fl, ok1 := indentLevel(ws, hint)
		ol, ok2 := indentLevel(oldInd[i], modelUnit)
		if ok1 && ok2 && fl == ol {
			return hint
		}
		return own
	}
	return own
}

// fitsUnit reports whether every indentation's space count is a whole number
// of unit (unit > 0).
func fitsUnit(indents []string, unit int) bool {
	if unit <= 0 {
		return false
	}
	for _, ws := range indents {
		if (len(ws)-strings.Count(ws, "\t"))%unit != 0 {
			return false
		}
	}
	return true
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// normalizeLine collapses internal whitespace runs to a single space and
// trims leading/trailing whitespace (this also absorbs a trailing "\r" on
// CRLF files, since strings.Fields treats it as whitespace).
func normalizeLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// splitLinesWithWidths splits content into lines without their "\n" (a CRLF
// line keeps its "\r"), plus the exact original byte width of each line *including*
// its terminator (or, for a final unterminated line, just its own length).
// Keeping widths lets callers reconstruct precise byte offsets for a
// replacement without re-scanning the original content.
func splitLinesWithWidths(content string) (lines []string, widths []int) {
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			lines = append(lines, content[start:i])
			widths = append(widths, i-start+1)
			start = i + 1
		}
	}
	if start < len(content) || len(content) == 0 {
		lines = append(lines, content[start:])
		widths = append(widths, len(content)-start)
	}
	return
}

// windowSimilarity scores how close a candidate block of raw file lines is
// to the (already-normalized) oldString lines, using per-line word-set
// Jaccard similarity averaged across the window. This is intentionally
// cheap (no character-level edit distance) so it stays fast even when
// scanning every window of a several-thousand-line file.
func windowSimilarity(windowLines []string, normOld []string) float64 {
	if len(windowLines) != len(normOld) || len(windowLines) == 0 {
		return 0
	}
	var total float64
	for i := range windowLines {
		total += jaccard(strings.Fields(windowLines[i]), strings.Fields(normOld[i]))
	}
	return total / float64(len(windowLines))
}

func jaccard(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	setA := make(map[string]bool, len(a))
	for _, w := range a {
		setA[w] = true
	}
	setB := make(map[string]bool, len(b))
	for _, w := range b {
		setB[w] = true
	}
	inter := 0
	for w := range setA {
		if setB[w] {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 1
	}
	return float64(inter) / float64(union)
}

func formatLineRanges(starts []int, n int) string {
	parts := make([]string, 0, len(starts))
	for _, s := range starts {
		if n <= 1 {
			parts = append(parts, fmt.Sprintf("%d", s+1))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", s+1, s+n))
		}
	}
	return strings.Join(parts, ", ")
}

func truncateForHint(s string, maxBytes int) string {
	// maxBytes is a byte budget, clipped on a rune boundary: the hint shows
	// file content back to the model, which is frequently non-ASCII here.
	return textutil.ClipBytes(s, maxBytes, "\n... (truncated)")
}
