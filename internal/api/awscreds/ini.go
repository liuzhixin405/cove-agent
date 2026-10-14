package awscreds

import (
	"bufio"
	"strings"
)

// parseINI reads the subset of INI the AWS CLI writes: "[section]" lines,
// "key = value" lines, blank lines and comments starting with # or ;.
// Section and key names are lower-cased and trimmed; values are trimmed
// (including a trailing \r from CRLF files). Keys before the first section
// are dropped. Multi-line values and inline comments are not supported.
func parseINI(content string) map[string]map[string]string {
	// PowerShell 5.1 (Out-File, Set-Content -Encoding utf8) and older
	// Notepad start the file with a UTF-8 BOM, which TrimSpace keeps.
	content = strings.TrimPrefix(content, string(rune(0xFEFF)))
	sections := map[string]map[string]string{}
	var current map[string]string
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' && strings.HasSuffix(line, "]") {
			name := strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			current = map[string]string{}
			sections[name] = current
			continue
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		current[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return sections
}
