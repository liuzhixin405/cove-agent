package safety

import (
	"regexp"
	"strings"
)

// CatastrophicCommand reports whether a shell command would do irreversible
// damage outside the project: wipe the filesystem, the home directory or a
// system directory, write to a raw disk, take the machine down, or run code
// fetched from the network. It returns a short reason when it does.
//
// The command is split into words and simple commands first, so "git add ."
// does not match "dd " and "rm -rf /tmp/x" does not match "rm -rf /". Earlier
// substring lists hard-blocked everyday commands like those while a command
// with an extra space in it slipped past.
func CatastrophicCommand(command string) (string, bool) {
	return catastrophicCommand(command, 0)
}

// maxShellNesting bounds how deep bash -c "sh -c '...'" is unwrapped. Deeper
// nesting has no legitimate use and is treated as catastrophic.
const maxShellNesting = 8

func catastrophicCommand(command string, depth int) (string, bool) {
	if depth > maxShellNesting {
		return "shell nesting too deep", true
	}
	command = ifsPattern.ReplaceAllString(command, " ")
	// $1..$9, $@ and $* are empty in the fresh shell a command line runs in,
	// so "rm$IFS$9-rf$IFS$9/" is rm -rf /; only $IFS used to be normalized.
	command = positionalPattern.ReplaceAllString(command, "")
	// PowerShell accepts en dash, em dash and minus sign as the parameter
	// dash (Remove-Item –Recurse). Normalizing them for every shell only
	// makes the scan stricter.
	command = dashNormalizer.Replace(command)
	if stripped := StripFormatCharacters(command); stripped != command {
		// Zero-width and bidi characters are invisible: "r<ZWSP>m -rf /" is
		// judged as the rm -rf / the person sees, as well as as written.
		if why, ok := catastrophicCommand(stripped, depth+1); ok {
			return why, true
		}
	}
	// The fork-bomb patterns see the line with quoted operator text removed:
	// echo ":(){ :|:& };:" and git commit -m ":(){ :|:& };:" only print or
	// store the text, and were hard-blocked when the raw line was matched. A
	// shell that runs a quoted bomb (bash -c ':(){ :|:& };:', eval '...') is
	// unwrapped below and judges the text again, unquoted.
	if name, ok := forkBomb(dropQuotedOperators(command)); ok {
		return "fork bomb " + name + "()", true
	}
	if why, ok := fetchedCodeRun(command); ok {
		return why, true
	}
	// The line is scanned as written and again as bash would read it (line
	// continuations joined, backslash quoting removed; see parseShellMode).
	// The caller does not say which shell runs it, and the literal reading
	// alone missed "rm \<NL>-rf ~", "r\m -rf /" and "rm -rf /\u\s\r" under
	// bash. The second pass can only add blocks, so a PowerShell or cmd path
	// such as C:\proj\build is still judged by its literal spelling as well.
	// Braces are read the bash way in the second pass as well ("{/etc,/usr}"
	// is one word there, brace-expanded below; the literal reading splits it).
	if psIexOfPipelineInput.MatchString(command) && fetches(command) {
		return "downloaded content run by iex in a script block", true
	}
	for _, pipeline := range readings(command) {
		if why, ok := catastrophicPipeline(pipeline); ok {
			return why, true
		}
		if why, ok := catastrophicXargs(pipeline); ok {
			return why, true
		}
		for i, c := range pipeline {
			if inner, encoded, ok := unwrapShell(c.words); ok {
				if encoded {
					return "encoded command", true
				}
				if why, ok := catastrophicCommand(inner, depth+1); ok {
					return why, true
				}
			}
			// eval and iex/Invoke-Expression run their arguments as a command
			// line. Deny rules already looked into them (NestedCommands), the
			// hard block did not: eval 'rm -rf /' and iex 'rm -rf /' ran in
			// bypass mode.
			if inner, ok := evalText(c.words); ok {
				if why, ok := catastrophicCommand(inner, depth+1); ok {
					return why, true
				}
			}
			// git -c alias.x='!rm -rf ~' x: the alias body runs through the
			// shell. Deny rules read it (NestedCommands); the hard block did
			// not.
			if name, args := commandWords(c.words); name == "git" {
				for _, alias := range GitShellAliases(args) {
					if why, ok := catastrophicCommand(alias, depth+1); ok {
						return why, true
					}
				}
			}
			// The command xargs or find -exec starts, with its arguments
			// written inline: "xargs rm -rf /" and "find . -exec rm -rf / \;"
			// delete the root whatever the input is. Only an xargs target
			// taken from the pipeline (echo / | xargs rm -rf) and find's own
			// start path used to be judged.
			if why, ok := catastrophicStarted(c.words, depth); ok {
				return why, true
			}
			// A here-document is a script when the command reading it is a
			// shell (bash <<EOF) or when it is piped into one (cat <<EOF |
			// sh); only the first used to be scanned.
			if len(c.heredocs) > 0 && (stdinShell(c) || pipesIntoShell(pipeline[i+1:])) {
				for _, h := range c.heredocs {
					if why, ok := catastrophicCommand(h.body, depth+1); ok {
						return why, true
					}
				}
			}
			if why, ok := catastrophicSimple(c); ok {
				return why, true
			}
			if expanded := braceExpandWords(c.words); expanded != nil {
				if why, ok := catastrophicSimple(simpleCmd{words: expanded, redirects: c.redirects}); ok {
					return why, true
				}
			}
		}
	}
	return "", false
}

// readings parses command literally and, when a backslash or a brace makes
// bash read it differently, the bash way as well.
func readings(command string) [][]simpleCmd {
	pipelines := parseShell(command)
	if strings.ContainsAny(command, `\{}`) {
		pipelines = append(pipelines, parseShellMode(command, true)...)
	}
	return pipelines
}

// braceExpandWords returns words with bash's brace expansion applied, or nil
// when no word has a {a,b} group.
func braceExpandWords(words []string) []string {
	var out []string
	changed := false
	for _, w := range words {
		alts := braceExpand(w)
		if len(alts) != 1 || alts[0] != w {
			changed = true
		}
		out = append(out, alts...)
	}
	if !changed {
		return nil
	}
	return out
}

// stdinShell reports whether c is a shell that runs its stdin as a script.
func stdinShell(c simpleCmd) bool {
	name, _ := commandWords(c.words)
	return stdinShells[name]
}

// pipesIntoShell reports whether one of the later commands of a pipeline is
// a shell reading its stdin as a script.
func pipesIntoShell(rest []simpleCmd) bool {
	for _, c := range rest {
		if name, args := commandWords(c.words); stdinShells[name] && readsCodeFromStdin(name, args) {
			return true
		}
	}
	return false
}

// stdinShells run a here-document or here-string fed to them as a script.
var stdinShells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"cmd": true, "pwsh": true, "powershell": true,
}

// unwrapShell recognizes a shell started with an inline command — sh/bash/
// zsh/dash/ksh -c "...", cmd /c ..., pwsh/powershell -Command ... — and
// returns that command string so it is scanned like a top-level one. encoded
// is true for PowerShell's -EncodedCommand, whose payload cannot be read.
// Arguments of other programs (git commit -m "rm -rf /") are never unwrapped.
func unwrapShell(words []string) (inner string, encoded, ok bool) {
	name, args := commandWords(words)
	switch name {
	case "sh", "bash", "zsh", "dash", "ksh", "ash":
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "--" || !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+"):
				// The first operand is a script file, not an inline command.
				return "", false, false
			case strings.HasPrefix(a, "--"):
				continue
			case a == "-o" || a == "+o" || a == "-O" || a == "+O":
				i++ // the option name that follows
			case strings.HasPrefix(a, "-") && strings.Contains(a[1:], "c"):
				// -c, -lc, -ec ...: the next non-option word is the command.
				for _, b := range args[i+1:] {
					if !strings.HasPrefix(b, "-") && !strings.HasPrefix(b, "+") {
						return b, false, true
					}
				}
				return "", false, false
			}
		}
	case "cmd":
		for i, a := range args {
			la := strings.ToLower(a)
			if la == "/c" || la == "/k" || la == "/r" {
				return strings.Join(args[i+1:], " "), false, true
			}
		}
	case "su", "runuser", "script":
		// su -c 'rm -rf /', runuser -u x -c '...', script -c '...'
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "-c" || a == "--command":
				if i+1 < len(args) {
					return args[i+1], false, true
				}
			case strings.HasPrefix(a, "--command="):
				return strings.TrimPrefix(a, "--command="), false, true
			}
		}
	case "start-process", "saps":
		// Start-Process rm -ArgumentList '-rf','/': the program and its
		// argument list, as one command line.
		program, argList := "", []string{}
		for i := 0; i < len(args); i++ {
			la := strings.ToLower(args[i])
			switch {
			case la == "-filepath" && i+1 < len(args):
				program = args[i+1]
				i++
			case (la == "-argumentlist" || la == "-args") && i+1 < len(args):
				argList = append(argList, strings.Split(args[i+1], ",")...)
				i++
			case strings.HasPrefix(la, "-"):
			case program == "":
				program = args[i]
			default:
				argList = append(argList, strings.Split(args[i], ",")...)
			}
		}
		if program != "" {
			for j := range argList {
				argList[j] = strings.Trim(strings.TrimSpace(argList[j]), "'\"")
			}
			return strings.Join(append([]string{program}, argList...), " "), false, true
		}
	case "start":
		// cmd's start [/b] [/wait] [/d dir] ["title"] command ...
		for i := 0; i < len(args); i++ {
			la := strings.ToLower(args[i])
			switch {
			case la == "/d" && i+1 < len(args):
				i++
			case strings.HasPrefix(la, "/"):
			case i == 0 && strings.HasPrefix(args[i], "\"") && strings.HasSuffix(args[i], "\""):
				// a quoted window title
			default:
				return strings.Join(args[i:], " "), false, true
			}
		}
	case "pwsh", "powershell":
		for i := 0; i < len(args); i++ {
			la := strings.ToLower(args[i])
			if !strings.HasPrefix(la, "-") && !strings.HasPrefix(la, "/") {
				// The first positional argument is the -Command (Windows
				// PowerShell) or, for pwsh, a script file; read as a command
				// either way, the stricter choice. Positionals used to be
				// skipped, so powershell "Remove-Item -Recurse -Force
				// C:\Windows" was not scanned.
				return strings.Join(args[i:], " "), false, true
			}
			flag := "-" + strings.TrimLeft(la, "-/")
			if j := strings.IndexByte(flag, ':'); j > 0 {
				flag = flag[:j] // -ExecutionPolicy:Bypass
			}
			switch {
			case flag == "-ec" || len(flag) >= 2 && strings.HasPrefix("-encodedcommand", flag):
				// -e, -ec, -enc ... -EncodedCommand: a base64 payload.
				return "", true, true
			case len(flag) >= 2 && strings.HasPrefix("-command", flag):
				return strings.Join(args[i+1:], " "), false, true
			case len(flag) >= 2 && strings.HasPrefix("-file", flag):
				// A script file; the words after it are its arguments.
				return "", false, false
			case powerShellValueParam(flag) && !strings.Contains(la, ":"):
				i++ // the parameter's value, not the command
			}
		}
	}
	return "", false, false
}

// powerShellValueParam reports a powershell.exe / pwsh parameter that takes
// a value (-ExecutionPolicy Bypass, -WindowStyle Hidden), given lowercased
// with one leading "-", by its full name, an abbreviation or an alias.
func powerShellValueParam(flag string) bool {
	switch flag {
	case "-ep", "-ex", "-wd", "-if", "-of", "-v", "-w", "-o", "-cnf":
		return true
	}
	for _, name := range []string{
		"-executionpolicy", "-windowstyle", "-version", "-inputformat", "-outputformat",
		"-configurationname", "-psconsolefile", "-workingdirectory", "-settingsfile",
		"-custompipename", "-configurationfile",
	} {
		if len(flag) >= 4 && strings.HasPrefix(name, flag) {
			return true
		}
	}
	return false
}

// xargsValueFlags are xargs options that take a separate value.
var xargsValueFlags = map[string]bool{
	"-n": true, "-I": true, "-L": true, "-P": true, "-d": true, "-s": true, "-E": true, "-a": true,
	"--max-args": true, "--max-lines": true, "--max-procs": true, "--delimiter": true,
	"--max-chars": true, "--eof": true, "--arg-file": true,
}

// catastrophicXargs catches `echo ~ | xargs rm -rf`: xargs runs its words as
// a command with the output of the commands before it appended. The output is
// approximated by those commands' arguments; each one is tried as the target.
func catastrophicXargs(p []simpleCmd) (string, bool) {
	for i, c := range p {
		name, args := commandWords(c.words)
		if name != "xargs" || i == 0 {
			continue
		}
		for len(args) > 0 && strings.HasPrefix(args[0], "-") {
			if xargsValueFlags[args[0]] && len(args) > 1 {
				args = args[1:]
			}
			args = args[1:]
		}
		if len(args) == 0 {
			continue
		}
		for _, prev := range p[:i] {
			_, prevArgs := commandWords(prev.words)
			for _, target := range prevArgs {
				if !criticalPath(target) {
					continue
				}
				words := append(append([]string(nil), args...), target)
				if why, ok := catastrophicSimple(simpleCmd{words: words}); ok {
					return "xargs: " + why, true
				}
			}
		}
	}
	return "", false
}

// catastrophicStarted judges the commands xargs and find -exec/-execdir/
// -ok/-okdir start (their words after xargs's options, or up to find's ";"
// or "+") as a command line of their own.
func catastrophicStarted(words []string, depth int) (string, bool) {
	if depth > maxShellNesting {
		return "shell nesting too deep", true
	}
	var started [][]string
	switch name, args := commandWords(words); name {
	case "xargs":
		started = append(started, xargsCommand(args))
	case "find":
		started = findExecCommands(args)
	case "go":
		// go test/run -exec 'cmd' runs cmd around the binary, -toolexec
		// around every tool; cmd/go splits the value on blanks.
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case (a == "-exec" || a == "-toolexec" || a == "--exec" || a == "--toolexec") && i+1 < len(args):
				started = append(started, strings.Fields(args[i+1]))
				i++
			case strings.HasPrefix(a, "-exec=") || strings.HasPrefix(a, "-toolexec=") || strings.HasPrefix(a, "--exec=") || strings.HasPrefix(a, "--toolexec="):
				started = append(started, strings.Fields(a[strings.IndexByte(a, '=')+1:]))
			}
		}
	}
	for _, w := range started {
		if len(w) == 0 {
			continue
		}
		if why, ok := catastrophicSimple(simpleCmd{words: w}); ok {
			return why, true
		}
		if inner, encoded, ok := unwrapShell(w); ok {
			if encoded {
				return "encoded command", true
			}
			if why, ok := catastrophicCommand(inner, depth+1); ok {
				return why, true
			}
		}
		if inner, ok := evalText(w); ok {
			if why, ok := catastrophicCommand(inner, depth+1); ok {
				return why, true
			}
		}
		if why, ok := catastrophicStarted(w, depth+1); ok {
			return why, true
		}
	}
	return "", false
}

// catastrophicFind catches find <critical path> -delete / -exec rm.
func catastrophicFind(args []string) (string, bool) {
	var paths []string
	i := findGlobalOptions(args)
	for ; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" {
			break
		}
		paths = append(paths, a)
	}
	critical := ""
	for _, p := range paths {
		if criticalPath(p) {
			critical = p
			break
		}
	}
	if critical == "" {
		return "", false
	}
	for j := i; j < len(args); j++ {
		switch args[j] {
		case "-delete":
			return "find " + critical + " -delete", true
		case "-exec", "-execdir", "-ok", "-okdir":
			if j+1 < len(args) {
				switch name, _ := commandWords(args[j+1:]); name {
				case "rm", "rmdir", "shred", "unlink", "del", "remove-item":
					return "find " + critical + " " + args[j] + " " + name, true
				}
			}
		}
	}
	return "", false
}

// findGlobalOptions returns the index of the first word after GNU find's
// options that come before the paths: -H, -L, -P, -D debugopts and
// -Olevel (also "-O level"). The paths were taken to start at the first
// word, so "find -P / -delete" had no path and was let through.
func findGlobalOptions(args []string) int {
	i := 0
	for i < len(args) {
		switch a := args[i]; {
		case a == "-H" || a == "-L" || a == "-P":
			i++
		case a == "-D":
			i += 2
		case a == "-O":
			i++
			if i < len(args) && isDigits(args[i]) {
				i++
			}
		case strings.HasPrefix(a, "-O") && isDigits(a[2:]):
			i++
		default:
			return i
		}
	}
	return len(args)
}

var (
	ifsPattern        = regexp.MustCompile(`\$\{?IFS\}?`)
	positionalPattern = regexp.MustCompile(`\$\{?[1-9@*]\}?`)
)

// dashNormalizer maps PowerShell's other dashes (SpecialCharacters.IsDash:
// en dash, em dash, horizontal bar) and the minus sign to "-".
var dashNormalizer = strings.NewReplacer("\u2013", "-", "\u2014", "-", "\u2015", "-", "\u2212", "-")

var (
	funcDef = regexp.MustCompile(`([\w:.]+)\s*\(\)\s*\{([^}]*)\}`)
	// funcKeyword is bash's other spelling, "function f { ... }" (the
	// parentheses optional), which funcDef did not see.
	funcKeyword = regexp.MustCompile(`\bfunction\s+([\w:.-]+)\s*(?:\(\s*\))?\s*\{([^}]*)\}`)
)

// dropQuotedOperators returns command with every quoted part that holds a
// shell operator character removed, quotes included, so text such as the
// message in git commit -m ":(){ :|:& };:" is not read as code. A quoted part
// without operators keeps its text and loses its quotes, so a quoted name
// ("f"|"f") still reads as the name. An unterminated quote runs to the end.
func dropQuotedOperators(command string) string {
	if !strings.ContainsAny(command, `'"`) {
		return command
	}
	var b strings.Builder
	for i := 0; i < len(command); {
		q := command[i]
		if q != '\'' && q != '"' {
			b.WriteByte(q)
			i++
			continue
		}
		var inner string
		if end := strings.IndexByte(command[i+1:], q); end < 0 {
			inner, i = command[i+1:], len(command)
		} else {
			inner, i = command[i+1:i+1+end], i+end+2
		}
		if !strings.ContainsAny(inner, "(){}|&;<>") {
			b.WriteString(inner)
		}
	}
	return b.String()
}

// forkBomb finds a function that pipes into itself, e.g. :(){ :|:& };:
func forkBomb(command string) (string, bool) {
	for _, re := range []*regexp.Regexp{funcDef, funcKeyword} {
		for _, m := range re.FindAllStringSubmatch(command, -1) {
			body := strings.Join(strings.Fields(m[2]), "")
			if strings.Contains(body, m[1]+"|"+m[1]) {
				return m[1], true
			}
		}
	}
	return "", false
}

// simpleCmd is one command of a pipeline: its words and output redirect targets.
type simpleCmd struct {
	words     []string
	quoted    []bool // quoted[i]: every character of words[i] came from inside quotes
	redirects []string
	// heredocs are the bodies of << / <<- here-documents and <<< here-strings
	// fed to this command's stdin. They are data, not commands of this line.
	heredocs []*heredoc
}

// parseShell splits a command line into pipelines of simple commands. It knows
// quotes, ; && || & | and newlines, redirects, here-documents, subshells and
// command substitution — enough to find each command's name and arguments.
// Backslash is kept literally: the same string may be a PowerShell or cmd
// command, where it is a path separator.
func parseShell(s string) [][]simpleCmd { return parseShellMode(s, false) }

// parseShellMode is parseShell with posix selecting bash's backslash rules:
// outside quotes a backslash-newline is a line continuation (removed, the
// command goes on) and a backslash quotes the next character; inside double
// quotes it only escapes $ ` " \ and newline; inside single quotes it is
// literal. The literal reading ended the command at "\<NL>" and kept "r\m" as
// written, so a command meant a different thing to bash than to the checks.
// A character quoted by a backslash counts as unquoted for Quoted, so a caller
// that trusts quoted words still refuses an operator character in it.
func parseShellMode(s string, posix bool) [][]simpleCmd {
	var (
		pipelines [][]simpleCmd
		pipeline  []simpleCmd
		cur       simpleCmd
		word      strings.Builder
		inWord    bool
		hadQuote  bool // the current word contains a quoted part
		allQuoted = true
		quote     rune
		redirect  bool // the next word is an output redirect target
		skipWord  bool // the next word is an input redirect source
		hereWord  bool // the next word is a here-string (<<<)
		pending   []*heredoc
		// parenDepth and lineComment keep << from being read as a heredoc
		// where bash would not start one — (( x<<2 )) arithmetic, a subshell,
		// or after a # comment — since skipping the "body" would hide
		// commands bash runs.
		parenDepth  int
		lineComment bool
	)
	endWord := func() {
		if !inWord {
			return
		}
		w := word.String()
		switch {
		case redirect:
			cur.redirects = append(cur.redirects, w)
			redirect = false
		case hereWord:
			cur.heredocs = append(cur.heredocs, &heredoc{body: w})
			hereWord = false
		case skipWord:
			skipWord = false
		default:
			cur.words = append(cur.words, w)
			cur.quoted = append(cur.quoted, hadQuote && allQuoted)
		}
		word.Reset()
		inWord = false
		hadQuote = false
		allQuoted = true
	}
	writeUnquoted := func(ch rune) {
		word.WriteRune(ch)
		inWord = true
		allQuoted = false
	}
	endCmd := func() {
		endWord()
		if len(cur.words) > 0 || len(cur.redirects) > 0 || len(cur.heredocs) > 0 {
			pipeline = append(pipeline, cur)
		}
		cur = simpleCmd{}
	}
	endPipeline := func() {
		endCmd()
		if len(pipeline) > 0 {
			pipelines = append(pipelines, pipeline)
		}
		pipeline = nil
	}

	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		ch := rs[i]
		next := rune(0)
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		if posix && ch == '\\' && quote != '\'' {
			switch {
			case quote == '"':
				switch next {
				case '$', '`', '"', '\\':
					word.WriteRune(next)
					i++
				case '\n':
					i++
				default:
					word.WriteRune(ch)
				}
			case next == '\n':
				i++ // line continuation: neither character is part of the line
			case i+1 >= len(rs):
				writeUnquoted(ch) // a trailing backslash is literal
			default:
				writeUnquoted(next)
				i++
			}
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
			continue
		}
		switch {
		case ch == '\'' || ch == '"':
			quote = ch
			inWord = true
			hadQuote = true
		case isStatementBreak(ch, next):
			// PowerShell ends a statement at a lone CR, NEL, U+2028 and
			// U+2029. The lone CR used to be read as a blank, so "ls<CR>rm
			// -rf x" was one ls with arguments here and two commands there.
			// Splitting is the stricter reading for bash too, which keeps
			// these characters inside a word.
			endPipeline()
			lineComment = false
		case ch != '\n' && isBlank(ch):
			// FF, VT, NBSP and the other Unicode spaces separate arguments
			// for PowerShell; only space, tab and CR (before LF) used to.
			endWord()
		case ch == '\n':
			endPipeline()
			lineComment = false
			if len(pending) > 0 {
				i = readHeredocBodies(rs, i, pending)
				pending = nil
			}
		case ch == '(':
			parenDepth++
			endPipeline()
		case ch == ')':
			if parenDepth > 0 {
				parenDepth--
			}
			endPipeline()
		case ch == ';' || ch == '`':
			endPipeline()
		case ch == '{' || ch == '}':
			if ch == '{' && inWord && strings.HasSuffix(word.String(), "$") {
				// ${VAR}: keep the whole expansion in the word.
				for ; i < len(rs) && rs[i] != '}'; i++ {
					word.WriteRune(rs[i])
				}
				writeUnquoted('}')
				continue
			}
			if posix && !braceIsGroup(ch, inWord, next) {
				// Under bash a brace is a command group only as a word of its
				// own ("{ cmd; }"); inside a word it is literal or brace
				// expansion (stash@{0}, HEAD@{1}, @{u}, {a,b}). Splitting there
				// made "git diff HEAD@{1}" three unknown commands. The literal
				// reading keeps splitting: a PowerShell scriptblock glued to a
				// word (ForEach-Object{ ... }) must stay visible.
				writeUnquoted(ch)
				continue
			}
			endPipeline()
		case ch == '$' && next == '(':
			parenDepth++
			endPipeline()
			i++
		case ch == '|':
			if next == '|' {
				endPipeline()
				i++
			} else {
				endCmd()
			}
		case ch == '&':
			if next == '&' {
				i++
			}
			endPipeline()
		case ch == '>':
			// A file descriptor number directly before > belongs to the redirect.
			if inWord && isDigits(word.String()) {
				word.Reset()
				inWord = false
			}
			endWord()
			if next == '>' || next == '|' {
				i++ // >> appends, >| overrides noclobber: both write a file
			}
			if i+1 < len(rs) && rs[i+1] == '&' {
				i++
				// >&WORD copies or closes a descriptor only when WORD is all
				// digits, exactly "-", or digits followed by "-" (2>&1, >&-,
				// >&1-). Any other WORD (>&1b, >&file) is a file bash writes.
				j := i + 1
				for j < len(rs) && !strings.ContainsRune(" \t\r\n;|&<>()'\"`", rs[j]) {
					j++
				}
				// bash removes quotes before judging WORD, so a quote glued to
				// it (>&1'b', 2>&1"b") makes it a file name; treat any glued
				// quote or backtick as a file (conservative for >&1'').
				glued := j < len(rs) && (rs[j] == '\'' || rs[j] == '"' || rs[j] == '`')
				if w := string(rs[i+1 : j]); !glued && isDescriptorWord(w) {
					i = j - 1
					continue
				}
			}
			redirect = true
		case ch == '<':
			// \< is a literal < for bash, so \<<EOF is no heredoc. In posix
			// mode the escape was already consumed above (and \\<<EOF is a
			// real heredoc after a literal backslash).
			escaped := !posix && i > 0 && rs[i-1] == '\\'
			if inWord && isDigits(word.String()) {
				word.Reset()
				inWord = false
			}
			endWord()
			switch {
			case next == '<' && i+2 < len(rs) && rs[i+2] == '<':
				// <<< here-string: the next word is stdin data.
				i += 2
				hereWord = true
			case next == '<' && (escaped || parenDepth > 0 || lineComment):
				// Not a heredoc bash would start: keep reading the next lines
				// as commands (the safe direction) and treat << as input.
				i++
				skipWord = true
			case next == '<':
				// << or <<- here-document: the body starts on the next line.
				i++
				dash := false
				if i+1 < len(rs) && rs[i+1] == '-' {
					dash = true
					i++
				}
				delim, last := readHeredocDelimiter(rs, i+1)
				i = last
				if delim != "" {
					h := &heredoc{delim: delim, dash: dash}
					cur.heredocs = append(cur.heredocs, h)
					pending = append(pending, h)
				}
			default:
				skipWord = true
			}
		default:
			if ch == '#' && !inWord {
				lineComment = true
			}
			writeUnquoted(ch)
		}
	}
	endPipeline()
	return pipelines
}

// SimpleCommand is one command the shell would start: its words exactly as
// written (quotes removed, nothing like sudo or VAR=value stripped) and the
// files its output is redirected to. Quoted[i] reports whether Words[i] was
// written entirely inside quotes ("a;b", 'x && y'), so an operator character
// in it is an argument for a shell that honours those quotes. Here-document
// and here-string bodies are stdin data and appear in neither list.
type SimpleCommand struct {
	Words     []string
	Quoted    []bool
	Redirects []string
}

// SimpleCommands lists every simple command in a command line, in order, with
// the same tokenizer CatastrophicCommand uses. Pipelines, && ; & and newlines
// are flattened, and the bodies of $(...), backticks and subshells come out as
// commands of their own, so a caller that vets each entry also vets what a
// substitution would run.
func SimpleCommands(command string) []SimpleCommand {
	return simpleCommandsMode(command, false)
}

// SimpleCommandsPOSIX is SimpleCommands for a line bash runs: backslash-newline
// continues the line and backslash quoting is removed from the words, as bash
// does before it starts the command (see parseShellMode). "find . -f\<NL>ls x"
// is one command, find -fls x, not a find and an ls.
func SimpleCommandsPOSIX(command string) []SimpleCommand {
	return simpleCommandsMode(command, true)
}

func simpleCommandsMode(command string, posix bool) []SimpleCommand {
	var out []SimpleCommand
	for _, pipeline := range parseShellMode(command, posix) {
		for _, c := range pipeline {
			if len(c.words) == 0 && len(c.redirects) == 0 {
				continue // only here-document data
			}
			out = append(out, SimpleCommand{Words: c.words, Quoted: c.quoted, Redirects: c.redirects})
		}
	}
	return out
}

// isDescriptorWord reports whether w, the word after >&, names a descriptor
// to copy or close: all digits, "-", or digits followed by "-".
func isDescriptorWord(w string) bool {
	if w == "-" {
		return true
	}
	return isDigits(strings.TrimSuffix(w, "-"))
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// commandWords drops what runs in front of the real command — sudo, env,
// VAR=value assignments — and returns the lowercased command name and its args.
func commandWords(words []string) (string, []string) {
	words = StripCommandRunners(words)
	if len(words) == 0 {
		return "", nil
	}
	return ProgramName(words[0]), words[1:]
}

// ProgramName normalizes an executable word the way the shell resolves it:
// lowercased (Windows is case-insensitive), directory and .exe dropped, so
// "/usr/bin/git", `C:\Git\git.exe` and "GIT" all name "git".
func ProgramName(exe string) string {
	name := strings.ToLower(exe)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".exe")
}

// runnerArgOptions lists, per command runner, the options that take their
// value as a separate word, so StripCommandRunners can step over it.
var runnerArgOptions = map[string]map[string]bool{
	"sudo":    {"-u": true, "-g": true, "-C": true, "-h": true, "-p": true, "-r": true, "-t": true, "-U": true, "-D": true},
	"doas":    {"-u": true, "-C": true},
	"env":     {"-u": true, "-C": true, "-S": true},
	"time":    {"-f": true, "-o": true},
	"nice":    {"-n": true},
	"exec":    {"-a": true},
	"timeout": {"-s": true, "-k": true},
	"nohup":   {},
	"command": {},
	"busybox": {},
	// More programs that only run what follows them: the hard block and
	// deny rules saw none of "setsid rm -rf /", "stdbuf -o0 rm -rf /",
	// "ionice -c3 rm -rf /", "strace rm -rf /", "wsl rm -rf /".
	"setsid":      {},
	"stdbuf":      {"-i": true, "-o": true, "-e": true},
	"ionice":      {"-c": true, "-n": true, "-p": true},
	"strace":      {"-o": true, "-e": true, "-p": true, "-s": true, "-E": true},
	"ltrace":      {"-o": true, "-e": true, "-p": true},
	"systemd-run": {"-p": true, "-u": true, "--unit": true, "--property": true},
	"unshare":     {},
	"nsenter":     {"-t": true},
	"fakeroot":    {},
	"caffeinate":  {"-t": true},
	"flock":       {"-w": true, "-E": true},
	"wsl":         {"-d": true, "-u": true, "--distribution": true, "--user": true, "--cd": true},
	"parallel":    {"-j": true},
}

// StripCommandRunners drops what runs in front of the real command — sudo,
// doas, env, nohup, time, command, nice, exec, busybox, timeout (with their
// own options, and timeout's duration) and VAR=value assignments — and
// returns the remaining words, the program as written first. Runner names are
// compared with ProgramName, so "/usr/bin/sudo" counts too.
func StripCommandRunners(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		name := ProgramName(w)
		if argOpts, ok := runnerArgOptions[name]; ok {
			words = words[1:]
			split := false
			for len(words) > 0 && strings.HasPrefix(words[0], "-") && words[0] != "-" {
				opt := words[0]
				words = words[1:]
				if opt == "--" {
					break
				}
				// env -S 'rm -rf /' (--split-string): the value IS the
				// command line, split on blanks; it used to be dropped as an
				// option value, so the hard block and deny rules saw an
				// empty command.
				if name == "env" {
					value, ok := "", false
					switch {
					case opt == "-S" || opt == "--split-string":
						if len(words) > 0 {
							value, ok, words = words[0], true, words[1:]
						}
					case strings.HasPrefix(opt, "--split-string="):
						value, ok = strings.TrimPrefix(opt, "--split-string="), true
					case strings.HasPrefix(opt, "-S") && len(opt) > 2:
						value, ok = opt[2:], true
					}
					if ok {
						words = append(strings.Fields(value), words...)
						split = true
						break
					}
				}
				if argOpts[opt] && len(words) > 0 {
					words = words[1:]
				}
			}
			if split {
				continue // the split words may start with VAR=value or another runner
			}
			if name == "timeout" && len(words) > 0 {
				words = words[1:] // the duration
			}
			if name == "flock" && len(words) > 0 && !strings.HasPrefix(words[0], "-") {
				words = words[1:] // the lock file (or descriptor)
			}
			continue
		}
		if strings.Contains(w, "=") && !strings.HasPrefix(w, "=") && !strings.HasPrefix(w, "-") {
			words = words[1:]
			continue
		}
		return words
	}
	return nil
}

var (
	driveRoot = regexp.MustCompile(`^[a-z]:$`)
	// gitBashDrive is a drive as Git Bash, MSYS, WSL and Cygwin spell it:
	// /c, /mnt/c, /cygdrive/c, optionally followed by a path.
	gitBashDrive = regexp.MustCompile(`^/(?:mnt/|cygdrive/)?([a-z])(/.*)?$`)
	multiSlash   = regexp.MustCompile(`/{2,}`)
	// systemDirs are compared lowercased, with forward slashes.
	systemDirs = map[string]bool{
		"/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true,
		"/lib": true, "/lib64": true, "/opt": true, "/proc": true, "/root": true,
		"/sbin": true, "/sys": true, "/usr": true, "/var": true,
		"/system": true, "/users": true, "/library": true, "/applications": true,
		"c:/windows": true, "c:/program files": true, "c:/program files (x86)": true,
		"c:/users": true, "c:/programdata": true,
	}
	homeRefs = map[string]bool{
		"~": true, "$home": true, "${home}": true, "%userprofile%": true,
		"$env:userprofile": true, "%systemroot%": true, "$env:systemroot": true,
		"%windir%": true, "$env:windir": true,
		// The other deterministic spellings of the home directory, the
		// system drive and the system directories (cmd, PowerShell and the
		// Windows variables Git Bash exposes). "${env:X}" is normalized to
		// "$env:x" before the lookup.
		"$userprofile": true, "${userprofile}": true,
		"%systemdrive%": true, "$env:systemdrive": true, "$systemdrive": true, "${systemdrive}": true,
		"%homedrive%": true, "$env:homedrive": true, "$homedrive": true,
		"%homedrive%%homepath%": true, "$env:homedrive$env:homepath": true, "$homedrive$homepath": true,
		"%programfiles%": true, "%programfiles(x86)%": true, "%programw6432%": true,
		"$env:programfiles": true, "$env:programfiles(x86)": true, "$env:programw6432": true,
		"$programfiles": true, "$programdata": true,
		"%programdata%": true, "$env:programdata": true, "%allusersprofile%": true, "$env:allusersprofile": true,
		"%appdata%": true, "$env:appdata": true, "$appdata": true,
		"%localappdata%": true, "$env:localappdata": true, "$localappdata": true,
		"%public%": true, "$env:public": true,
		"/home/$user": true, "/home/${user}": true, "/home/$logname": true, "/home/$username": true,
		"/users/$user": true, "/users/${user}": true,
	}
)

// dotGlobSuffixes name a directory's dot entries: "~/.*", "~/.[!.]*" and
// "~/.??*" (after the trailing "*" is trimmed) delete .ssh, .gnupg, .aws and
// everything else in the home directory; rm skips only "." and "..".
var dotGlobSuffixes = []string{"/.[!.]", "/.??", "/.", `\.[!.]`, `\.??`, `\.`}

// criticalPath reports whether deleting (or chmod-ing) target recursively
// would destroy the system or the user's home rather than a project directory.
func criticalPath(target string) bool {
	// PowerShell treats typographic quotes (U+2018–U+201F) as quotes; the
	// tokenizer keeps them, so strip them before judging the target.
	t := strings.ToLower(strings.TrimSpace(strings.Trim(target, "‘’‚‛“”„‟")))
	if strings.Contains(t, ",") {
		// A PowerShell array argument (".,C:\Windows") names each element.
		for _, part := range strings.Split(t, ",") {
			if criticalPath(part) {
				return true
			}
		}
		return false
	}
	// Globs and separators at the end name the same directory's contents:
	// "/**", "/*/", "/etc/**", "~/**". Only one "*" and then the slashes used
	// to be stripped, so those were judged as some other path.
	base := strings.TrimRight(t, `*/\`)
	for changed := true; changed; {
		changed = false
		for _, suffix := range dotGlobSuffixes {
			if strings.HasSuffix(base, suffix) && len(base) > len(suffix) {
				base = strings.TrimRight(base[:len(base)-len(suffix)], `*/\`)
				changed = true
			}
		}
		// "~/.." is the parent of the home directory (/home): deleting it
		// takes the home with it.
		for _, suffix := range []string{"/..", `\..`} {
			if strings.HasSuffix(base, suffix) && len(base) > len(suffix) {
				base = strings.TrimRight(base[:len(base)-len(suffix)], `*/\`)
				changed = true
			}
		}
	}
	base = strings.ReplaceAll(base, "${env:", "$env:")
	if strings.HasPrefix(base, "$env:") && strings.HasSuffix(base, "}") && !strings.Contains(base, "{") {
		base = strings.TrimSuffix(base, "}")
	}
	if base == "" {
		// "/", "/**", `\` are the root; "", "*", "**", "*/" the working
		// directory (destructive, but project-scoped: a warning).
		return strings.HasPrefix(t, "/") || strings.HasPrefix(t, `\`)
	}
	if base == "." {
		return false // ".", "./", "./*", "./**/": the working directory
	}
	p := multiSlash.ReplaceAllString(strings.ReplaceAll(base, `\`, "/"), "/")
	if m := gitBashDrive.FindStringSubmatch(p); m != nil {
		// /c/Windows is C:\Windows for Git Bash; forward-slash Windows
		// paths (C:/Users) were not known either.
		p = m[1] + ":" + m[2]
	}
	return driveRoot.MatchString(p) || homeRefs[p] || systemDirs[p]
}

func catastrophicSimple(c simpleCmd) (string, bool) {
	for _, r := range c.redirects {
		if rawDisk(r) {
			return "write to raw disk " + r, true
		}
	}
	name, args := commandWords(c.words)
	switch name {
	case "rm", "rmdir", "remove-item", "ri", "del", "erase", "rd":
		recursive := false
		for _, a := range args {
			la := strings.ToLower(a)
			if la == "--no-preserve-root" {
				return "rm --no-preserve-root", true
			}
			// PowerShell's -Name:value switch form (-Recurse:$true) names the
			// same switch; judged by name alone, so -Recurse:$false is treated
			// as recursive too (stricter).
			if strings.HasPrefix(la, "-") && !strings.HasPrefix(la, "--") {
				if i := strings.IndexByte(la, ':'); i > 0 {
					la = la[:i]
				}
			}
			recursive = recursive || recursiveFlag(la)
		}
		if !recursive {
			return "", false
		}
		// Only cmd's del/erase/rd/rmdir take /s /q /p /f /a switches. Any
		// "/x" used to be skipped as one, so "rm -rf /c" (Git Bash's C:\)
		// was not judged at all.
		cmdBuiltin := name == "del" || name == "erase" || name == "rd" || name == "rmdir"
		for i, a := range args {
			if strings.HasPrefix(a, "-") || cmdBuiltin && cmdSwitch.MatchString(strings.ToLower(a)) {
				continue
			}
			if criticalPath(a) {
				return "recursive delete of " + a, true
			}
			// An unquoted path with a space ("c:/program files/") arrives as
			// two words; the pair is judged as one path as well.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				if joined := a + " " + args[i+1]; criticalPath(joined) {
					return "recursive delete of " + joined, true
				}
			}
		}
	case "chmod", "chown", "chgrp":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && criticalPath(a) {
				return name + " on " + a, true
			}
		}
	case "mkfs", "mke2fs", "fdisk", "sfdisk", "parted", "wipefs", "format-volume", "clear-disk", "initialize-disk", "diskpart":
		return name + " rewrites a disk", true
	case "format":
		for _, a := range args {
			if driveArg.MatchString(strings.ToLower(a)) {
				return "format " + a, true
			}
		}
	case "dd":
		for _, a := range args {
			if strings.HasPrefix(strings.ToLower(a), "of=") && rawDisk(a[len("of="):]) {
				return "dd to raw disk " + a, true
			}
		}
	case "shred", "tee":
		// shred overwrites every file it is given, tee writes to them: of a
		// block device, that destroys the disk. Only dd and redirects were
		// known.
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && rawDisk(a) {
				return name + " to raw disk " + a, true
			}
		}
	case "cp", "mv", "install", "copy-item", "move-item", "copy", "move":
		// The destination is the last operand (or -t DIR / --target-directory).
		var operands []string
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "-t" && i+1 < len(args):
				if rawDisk(args[i+1]) {
					return name + " onto raw disk " + args[i+1], true
				}
				i++
			case strings.HasPrefix(a, "--target-directory="):
				if rawDisk(strings.TrimPrefix(a, "--target-directory=")) {
					return name + " onto raw disk " + a, true
				}
			case !strings.HasPrefix(a, "-"):
				operands = append(operands, a)
			}
		}
		if n := len(operands); n >= 2 && rawDisk(operands[n-1]) {
			return name + " onto raw disk " + operands[n-1], true
		}
	case "shutdown", "reboot", "halt", "poweroff", "stop-computer", "restart-computer", "telinit":
		return name + " takes the machine down", true
	case "init":
		if len(args) > 0 && (args[0] == "0" || args[0] == "6") {
			return name + " " + args[0] + " takes the machine down", true
		}
	case "systemctl":
		for _, a := range args {
			switch strings.ToLower(a) {
			case "poweroff", "reboot", "halt", "kexec", "suspend", "hibernate":
				return name + " " + a + " takes the machine down", true
			}
		}
	case "find":
		return catastrophicFind(args)
	}
	if strings.HasPrefix(name, "mkfs.") {
		return name + " rewrites a disk", true
	}
	return "", false
}

// recursiveFlag recognizes -r/-R/--recursive, combined short flags (-rf, -fR),
// PowerShell's -Recurse and any prefix of it, and cmd's /s.
func recursiveFlag(la string) bool {
	switch {
	case la == "--recursive" || la == "/s":
		return true
	case strings.HasPrefix(la, "--"):
		return false
	case len(la) >= 2 && strings.HasPrefix("-recurse", la):
		return true
	case shortFlags.MatchString(la):
		return strings.Contains(la, "r")
	}
	return false
}

var (
	// Any number of letters: the group used to be capped at four, so "rm
	// -rfvvv /" was not recursive to the scan.
	shortFlags = regexp.MustCompile(`^-[rfivd]+$`)
	cmdSwitch  = regexp.MustCompile(`^/[sqpfa](:.*)?$`)
	driveArg   = regexp.MustCompile(`^[a-z]:$`)
)

func rawDisk(path string) bool {
	p := strings.ToLower(path)
	for _, prefix := range []string{"/dev/sd", "/dev/hd", "/dev/nvme", "/dev/vd", "/dev/xvd", "/dev/disk", "/dev/mmcblk", `\\.\physicaldrive`,
		"/dev/mapper/", "/dev/dm-", "/dev/md", "/dev/loop", "/dev/zd"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	// \\.\C: is the volume device of drive C.
	if strings.HasPrefix(p, `\\.\`) && len(p) >= 6 && p[5] == ':' {
		return true
	}
	return false
}

var (
	remoteSources = map[string]bool{
		"curl": true, "wget": true, "fetch": true, "iwr": true, "irm": true,
		"invoke-webrequest": true, "invoke-restmethod": true,
		"base64": true, "xxd": true,
	}
	interpreters = map[string]bool{
		"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
		"python": true, "python3": true, "perl": true, "ruby": true, "node": true, "php": true,
		"pwsh": true, "powershell": true, "iex": true, "invoke-expression": true,
		"source": true, ".": true,
	}
	// psIexOfPipelineInput is iex applied to the pipeline variable inside a
	// script block: "iwr URL | % { iex $_.Content }". The block splits the
	// pipeline for the tokenizer, so the source and the sink never meet.
	psIexOfPipelineInput = regexp.MustCompile(`(?i)\b(?:iex|invoke-expression)\s+\$_`)
)

// catastrophicPipeline catches downloaded or decoded text being run as code:
// curl ... | sh, irm ... | iex, base64 -d | bash. An interpreter that is given
// a script or a module (python3 -m json.tool, python3 parse.py) is only
// reading data and is left alone.
func catastrophicPipeline(p []simpleCmd) (string, bool) {
	source := ""
	for _, c := range p {
		name, args := commandWords(c.words)
		if source != "" && interpreters[name] && readsCodeFromStdin(name, args) {
			return source + " piped into " + name, true
		}
		if remoteSources[name] {
			source = name
		}
	}
	return "", false
}

// posixShells are the sh-family interpreters readsCodeFromStdin reads the
// options of.
var posixShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true, "fish": true}

// codeOptions lists, per interpreter, the options whose value is the program
// text (so stdin is data). Letters are case-sensitive: python -E only ignores
// the environment, perl -E runs code.
var codeOptions = map[string]string{
	"python": "cm", "python3": "cm", "perl": "eE", "ruby": "e", "node": "ep", "php": "r",
}

// stdinScript reports a script operand that is stdin itself.
func stdinScript(a string) bool {
	switch a {
	case "-", "/dev/stdin", "/dev/fd/0", "/proc/self/fd/0":
		return true
	}
	return false
}

// readsCodeFromStdin reports whether interpreter name, given args, runs the
// program text it reads on stdin. Any -c/-m/-e style option, or a script
// file operand, used to mean "reads data" and every other word a flag; so
// "bash -s -- install" (-s: commands from stdin, the rest positional
// parameters), "bash /dev/stdin", "bash -" and "bash -e" (errexit, not code)
// were let through while they run the piped download.
func readsCodeFromStdin(name string, args []string) bool {
	switch {
	case name == "source" || name == ".":
		// source /dev/stdin, . - : the current shell runs what is piped in.
		return len(args) > 0 && stdinScript(args[0])
	case posixShells[name]:
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case stdinScript(a):
				return true
			case a == "--":
				return i+1 >= len(args) || stdinScript(args[i+1])
			case a == "-o" || a == "+o" || a == "-O" || a == "+O":
				i++ // the option name
			case a == "--rcfile" || a == "--init-file":
				i++
			case strings.HasPrefix(a, "--"):
			case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+"):
				switch {
				case strings.ContainsRune(a[1:], 'c'):
					return false // -c: the command is an argument
				case strings.ContainsRune(a[1:], 's'):
					return true // -s: commands from stdin, the rest are parameters
				}
			default:
				return false // a script file
			}
		}
		return true
	case name == "pwsh" || name == "powershell":
		for i := 0; i < len(args); i++ {
			la := strings.ToLower(args[i])
			if !strings.HasPrefix(la, "-") && !strings.HasPrefix(la, "/") {
				return false // the command (or a script) is an argument
			}
			flag := "-" + strings.TrimLeft(la, "-/")
			switch {
			case len(flag) >= 2 && (strings.HasPrefix("-command", flag) || strings.HasPrefix("-file", flag)):
				// -Command - and -File - read stdin.
				return i+1 < len(args) && args[i+1] == "-"
			case powerShellValueParam(flag):
				i++
			}
		}
		return true
	case name == "iex" || name == "invoke-expression":
		// iex runs its pipeline input unless it is given the string itself.
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				return false
			}
		}
		return true
	}
	code := codeOptions[name]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case stdinScript(a):
			return true
		case a == "--":
			return i+1 >= len(args) || stdinScript(args[i+1])
		case strings.HasPrefix(a, "--"):
			if a == "--eval" || a == "--print" || strings.HasPrefix(a, "--eval=") {
				return false
			}
		case strings.HasPrefix(a, "-"):
			if strings.ContainsAny(a[1:], code) {
				return false
			}
			if (name == "python" || name == "python3") && (a == "-W" || a == "-X") {
				i++ // the option's value
			}
		default:
			return false // a script file
		}
	}
	return true
}
