package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AppendFileBytes is the size one memory file may grow to by appending; past
// it Append rolls over to name-2.md, name-3.md, ... instead of clipping.
const AppendFileBytes = 10240

// BaseContent returns the content name currently has: the primary
// directory's file, else (fromLower) a lower-priority directory's (the global
// one under a per-project store). ok is false when no directory has it.
func (s *Store) BaseContent(name string) (content string, fromLower, ok bool) {
	for i, d := range s.dirs {
		data, err := os.ReadFile(filepath.Join(d, name))
		if err == nil {
			return string(data), i > 0, true
		}
	}
	return "", false, false
}

// Append adds content to the memory name and returns the file actually
// written. The base is the primary directory's copy or, when only a
// lower-priority (global) directory has the name, that copy: the project copy
// then shadows the global one without losing what it said. Past
// AppendFileBytes (or MaxIndexLines) the content goes to the first
// name-N.md roll-over file with room. Writes go through Save.
func (s *Store) Append(name, content string) (string, error) {
	return s.append(name, content, nil)
}

func (s *Store) append(name, content string, source *ProvenanceSource) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i <= 100; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s-%d%s", base, i, ext)
		}
		existing, _, ok := s.BaseContent(cand)
		if !ok || existing == "" {
			return cand, s.save(cand, content, source, "")
		}
		if endsWithLines(existing, content) {
			// Already the file's last lines: appending again only duplicates.
			return cand, nil
		}
		combined := existing + "\n" + content
		if len(combined) <= AppendFileBytes && strings.Count(combined, "\n")+1 <= MaxIndexLines {
			return cand, s.save(cand, combined, source, cand)
		}
	}
	cand := fmt.Sprintf("%s-%d%s", base, 100, ext)
	return cand, s.save(cand, content, source, "")
}

// Dirs are the store's directories, primary (written) first.
func (s *Store) Dirs() []string { return append([]string(nil), s.dirs...) }

// endsWithLines reports whether base's last lines equal content's lines
// (compared trimmed, blank lines ignored).
func endsWithLines(base, content string) bool {
	want := nonBlankLines(content)
	have := nonBlankLines(base)
	if len(want) == 0 || len(want) > len(have) {
		return len(want) == 0
	}
	tail := have[len(have)-len(want):]
	for i := range want {
		if tail[i] != want[i] {
			return false
		}
	}
	return true
}

func nonBlankLines(s string) []string {
	var out []string
	// TrimSpace also drops a CR of CRLF line endings.
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
