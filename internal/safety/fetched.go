package safety

import (
	"regexp"
	"strings"
)

// Downloaded code handed to an interpreter through a substitution. The pipe
// spellings (curl ... | sh, irm ... | iex) are caught by catastrophicPipeline;
// these were not, although they run the download just the same:
//
//	bash <(curl -s URL)          source <(curl URL)      . <(curl URL)
//	sh -c "$(curl -fsSL URL)"    eval "$(curl URL)"      $(curl URL)
//	iex (iwr URL)                iex (irm URL)           python3 <(curl URL)
//
// The tokenizer ends a pipeline at "$(", "<(" and "(" and keeps a double-
// quoted "$(...)" inside its word, so neither side knows the substitution is
// an argument of the interpreter. fetchedCodeRun therefore replaces each
// substitution whose commands fetch (curl, wget, fetch, iwr, irm,
// Invoke-WebRequest, Invoke-RestMethod; base64/xxd decode likewise) with a
// placeholder word and looks where the placeholder ends up.

// psCallOfFetched is PowerShell's call operator (or dot-sourcing) applied to
// a fetched substitution or group.
var psCallOfFetched = regexp.MustCompile(`(?:^|[\s;|(])[&.]\s*(?:` + fetchedValue + `|` + fetchedCode + `)`)

const (
	// fetchedCode stands for $(...), `...` and <(...) whose commands fetch:
	// as a program word or an interpreter's script it runs the download.
	fetchedCode = "__cove_fetched_code__"
	// fetchedValue stands for a bare (...) group whose commands fetch: in
	// PowerShell that is the downloaded text as a value, run by iex; in bash
	// a subshell, whose output is only printed.
	fetchedValue = "__cove_fetched_value__"
)

// fetchedCodeRun reports downloaded or decoded content run as code through
// a command or process substitution (see the file comment). command has been
// normalized by catastrophicCommand; shells that run an inline command (bash
// -c, eval) are unwrapped there and their text comes back through here.
func fetchedCodeRun(command string) (string, bool) {
	if !strings.ContainsAny(command, "(`") {
		return "", false
	}
	marked, inner := markFetchSubstitutions(command)
	for _, s := range inner {
		// A substitution's own text may hand a download to an interpreter:
		// (bash <(curl URL)) or echo $(sh -c "$(curl URL)").
		if why, ok := fetchedCodeRun(s); ok {
			return why, true
		}
	}
	if marked == command {
		return "", false
	}
	// The tokenizer reads "&" as an operator, so the call operator applied
	// to a fetched value never reaches the loop below.
	if psCallOfFetched.MatchString(marked) {
		return "downloaded code run as a command", true
	}
	for _, pipeline := range readings(marked) {
		piped := false
		for _, c := range pipeline {
			w := StripCommandRunners(c.words)
			if len(w) == 0 {
				continue
			}
			if strings.Contains(w[0], fetchedCode) {
				return "downloaded code run as a command", true
			}
			if strings.Contains(w[0], fetchedValue) {
				// "(irm URL) | iex", "(iwr URL).Content | iex": the group's
				// value is the pipeline input of what follows.
				piped = true
				continue
			}
			if (w[0] == "&" || w[0] == ".") && len(w) > 1 && (strings.Contains(w[1], fetchedValue) || strings.Contains(w[1], fetchedCode)) {
				// & ([scriptblock]::Create((irm URL))), . (irm URL)
				return "downloaded code run as a command", true
			}
			name, args := ProgramName(w[0]), w[1:]
			if fetchedScriptArg(name, args) {
				return name + " runs downloaded code", true
			}
			// echo "$(curl URL)" | sh: the download reaches the shell's stdin.
			if piped && interpreters[name] && readsCodeFromStdin(name, args) {
				return "downloaded code piped into " + name, true
			}
			for _, a := range args {
				if strings.Contains(a, fetchedCode) {
					piped = true
				}
			}
		}
	}
	return "", false
}

// fetchedScriptArg reports an interpreter invocation whose code is a
// fetched-substitution placeholder: the file of source and ".", the text of
// eval and iex (a fetched (...) value included), or for a shell or language
// interpreter an option's value or the first operand, which is the script
// (python3 parse.py <(curl URL) only reads the download as data).
func fetchedScriptArg(name string, args []string) bool {
	switch name {
	case "source", ".":
		return len(args) > 0 && strings.Contains(args[0], fetchedCode)
	case "eval", "iex", "invoke-expression":
		for _, a := range args {
			if strings.Contains(a, fetchedCode) || strings.Contains(a, fetchedValue) {
				return true
			}
		}
		return false
	}
	if !interpreters[name] {
		return false
	}
	for _, a := range args {
		if strings.Contains(a, fetchedCode) {
			return true
		}
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			return false // the script operand, not fetched
		}
	}
	return false
}

// markFetchSubstitutions returns command with every fetching $(...), `...`,
// <(...) replaced by fetchedCode and every fetching bare (...) by
// fetchedValue, and the text inside every substitution and group it found.
// Single quotes hide all of them; double quotes hide only <(...) and (...),
// since bash and PowerShell still expand $(...) and backticks there.
func markFetchSubstitutions(command string) (string, []string) {
	var b strings.Builder
	var inner []string
	var quote byte
	for i := 0; i < len(command); {
		ch := command[i]
		next := byte(0)
		if i+1 < len(command) {
			next = command[i+1]
		}
		if quote == '\'' {
			if ch == '\'' {
				quote = 0
			}
			b.WriteByte(ch)
			i++
			continue
		}
		start, placeholder := -1, fetchedCode
		switch {
		case ch == '\'' && quote == 0:
			quote = '\''
		case ch == '"':
			if quote == '"' {
				quote = 0
			} else {
				quote = '"'
			}
		case ch == '$' && next == '(':
			start = i + 2
		case (ch == '<' || ch == '>') && next == '(' && quote == 0:
			start = i + 2
		case ch == '(' && quote == 0:
			start, placeholder = i+1, fetchedValue
		case ch == '`':
			end := strings.IndexByte(command[i+1:], '`')
			body, after := command[i+1:], len(command)
			if end >= 0 {
				body, after = command[i+1:i+1+end], i+end+2
			}
			inner = append(inner, body)
			if fetches(body) {
				b.WriteString(fetchedCode)
			} else {
				b.WriteString(command[i:after])
			}
			i = after
			continue
		}
		if start < 0 {
			b.WriteByte(ch)
			i++
			continue
		}
		end := closingParen(command, start)
		body, after := command[start:], len(command)
		if end >= 0 {
			body, after = command[start:end], end+1
		}
		inner = append(inner, body)
		if ch == '>' || !fetches(body) {
			// >(...) consumes output, it does not produce the code.
			b.WriteString(command[i:after])
		} else {
			b.WriteString(placeholder)
		}
		i = after
	}
	return b.String(), inner
}

// closingParen returns the index of the ")" that closes a "(" whose text
// starts at start, skipping quoted parts, or -1 when it is not closed.
func closingParen(s string, start int) int {
	depth := 1
	var quote byte
	for j := start; j < len(s); j++ {
		c := s[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// fetches reports whether a command line runs a download or decode command
// (remoteSources), substitutions inside it included.
func fetches(line string) bool {
	for _, sc := range SimpleCommands(line) {
		if name, _ := commandWords(sc.Words); remoteSources[name] {
			return true
		}
	}
	return false
}
