package permission

import (
	"regexp"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/safety"
)

// simpleCommands is the tokenizer the classifier uses (posix selects bash's
// backslash rules, safety.SimpleCommandsPOSIX); a variable so a test can
// count how often a line is split.
var simpleCommands = func(command string, posix bool) []safety.SimpleCommand {
	if posix {
		return safety.SimpleCommandsPOSIX(command)
	}
	return safety.SimpleCommands(command)
}

// shellCommands splits command the way a shell of kind reads it. Under bash
// (ShellPOSIX) backslash-newline continues the line and backslash quoting is
// removed; the literal reading turned "find . -f\<NL>ls out.txt" (bash runs
// find -fls out.txt) into two read-only commands. PowerShell and cmd keep the
// backslash, a path character there. For the unknown kind "" either reading
// may be the real one, so ok is false when the two differ: a line whose
// meaning depends on backslash handling is not vouched for.
func shellCommands(command string, kind ShellKind) (cmds []safety.SimpleCommand, ok bool) {
	switch kind {
	case ShellPOSIX:
		return simpleCommands(command, true), true
	case "":
		literal := simpleCommands(command, false)
		if strings.ContainsRune(command, '\\') && !sameCommands(literal, simpleCommands(command, true)) {
			return literal, false
		}
		return literal, true
	}
	return simpleCommands(command, false), true
}

// anyReading lists the simple commands of command under both the literal and
// the bash reading, for deny and ask rules: widening them to either reading
// only ever refuses or asks more often. The readings differ where a
// backslash or a brace is (bash keeps "stash@{0}" one word).
func anyReading(command string) []safety.SimpleCommand {
	out := safety.SimpleCommands(command)
	if strings.ContainsAny(command, `\{}`) {
		out = append(out, safety.SimpleCommandsPOSIX(command)...)
	}
	return out
}

// denyReading lists the commands deny and ask rules look at: every reading
// of the line (anyReading), the same for the line without its invisible
// format characters ("git<ZWSP> push"), and every command the line runs
// indirectly (safety.NestedCommands: sh -c, eval, xargs, find -exec, a
// heredoc piped into a shell ...). Only the top-level commands used to be
// seen, so "bash -c 'git push'" passed a deny on git push.
//
// opaque reports a command whose program cannot be read from the text
// ($GIT push, bash -c "$CMD", eval "$x", xargs "$tool", %TOOL% under cmd,
// $env:TOOL or ${x} under PowerShell): it may be anything, so every deny and
// ask rule applies to the line. Only nested commands used to count, so with
// the whole tool allowed and a deny on git push, "$GIT push" and "G=git; $G
// push" ran. Such a command is never read-only either (classifyWords refuses
// a program word holding a $).
func denyReading(command string) (cmds []safety.SimpleCommand, opaque bool) {
	variants := []string{command}
	if s := safety.StripFormatCharacters(command); s != command {
		variants = append(variants, s)
	}
	for _, v := range variants {
		top := anyReading(v)
		cmds = append(cmds, top...)
		nested := safety.NestedCommands(v)
		cmds = append(cmds, nested...)
		for _, c := range append(top, nested...) {
			if variableProgram(c.Words) {
				opaque = true
			}
		}
		// "$(which rm) -rf x", "`which rm` -rf x", "& (gcm rm) -rf x": the
		// substitution is the program, but the tokenizer ends a command at
		// "$(", "`" and "(", so what is left reads as a command whose first
		// word is an option. That program cannot be read from the text:
		// every deny and ask rule applies, as for "$RM -rf x".
		if (hasSubstitution(v) || strings.Contains(v, "(")) && substitutedProgram.MatchString(v) {
			opaque = true
		}
	}
	return cmds, opaque
}

// substitutedProgram is a closed substitution or group directly followed by
// a plain word: "$(which rm) -rf x", "`which rm` -rf x", "& (gcm rm) -rf x",
// "$(which git) push". A substitution that is an argument ("python3
// parse.py <(curl URL)", "git commit -m \"$(cat msg)\"", "diff <(a) <(b)")
// is followed by nothing, an operator or another substitution.
var substitutedProgram = regexp.MustCompile("[)`]" + `\s+[A-Za-z_./-]`)

// variableProgram reports a simple command whose program word, after the
// runners in front (sudo, env, VAR=value), is built from a variable or a
// substitution: $X, ${X}, $env:X, rm${IFS}-rf, %X%, a backtick. A lone "%"
// is PowerShell's ForEach-Object alias, not a cmd variable, and a statement
// that is only a member access ($_.Name in a ForEach-Object block) is a
// PowerShell expression: both used to count only when nested, and must not
// make every deny rule refuse "ls | % { $_.Name }".
func variableProgram(words []string) bool {
	w := safety.StripCommandRunners(words)
	if len(w) == 0 {
		return false
	}
	p := w[0]
	switch {
	case strings.ContainsRune(p, '`') || strings.Count(p, "%") >= 2:
		return true
	case !strings.Contains(p, "$"):
		return false
	case len(w) == 1 && psMemberAccess.MatchString(p):
		return false
	}
	return true
}

// psMemberAccess is a PowerShell property read such as $_.Name or
// $item.FullName.Length.
var psMemberAccess = regexp.MustCompile(`^\$[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$`)

// definesGitAlias reports a git invocation among words that defines an alias
// (git -c alias.p=push p, git config alias.p push): the alias runs a
// subcommand, or a shell command, that no rule on git's subcommands sees, so
// every deny and ask rule on git applies.
func definesGitAlias(words []string) bool {
	w := safety.StripCommandRunners(words)
	return len(w) > 0 && programName(w[0]) == "git" && safety.GitDefinesAlias(w[1:])
}

// literalUNC reports a UNC path (\\host\share) in the literal reading of
// command. Bash's reading turns \\evil\x into \evilx, but the line may not
// reach bash as typed (Git Bash users write UNC paths this way), so a word
// that names another host in either reading is refused.
func literalUNC(command string) bool {
	if !strings.Contains(command, `\\`) {
		return false
	}
	for _, sc := range safety.SimpleCommands(command) {
		for _, w := range sc.Words {
			if isUNCPath(w) {
				return true
			}
		}
	}
	return false
}

func sameCommands(a, b []safety.SimpleCommand) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameStrings(a[i].Words, b[i].Words) || !sameStrings(a[i].Redirects, b[i].Redirects) {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type CmdCategory int

const (
	CatUnknown   CmdCategory = iota
	CatSafe                  // 只读，永远安全
	CatGit                   // git 操作，需区分读/写
	CatBuild                 // 构建/测试，需看具体命令
	CatInstall               // 包管理器安装
	CatDangerous             // rm -rf, fork bomb, etc.
)

func (c CmdCategory) String() string {
	switch c {
	case CatSafe:
		return "CatSafe"
	case CatGit:
		return "CatGit"
	case CatBuild:
		return "CatBuild"
	case CatInstall:
		return "CatInstall"
	case CatDangerous:
		return "CatDangerous"
	default:
		return "CatUnknown"
	}
}

// riskRank orders categories from least to most risky, so a compound line
// takes the category of its riskiest command.
func riskRank(c CmdCategory) int {
	switch c {
	case CatSafe:
		return 0
	case CatBuild:
		return 1
	case CatGit:
		return 2
	case CatInstall:
		return 3
	case CatDangerous:
		return 5
	default: // CatUnknown
		return 4
	}
}

type Classifier struct{}

func NewClassifier() *Classifier { return &Classifier{} }

// Classify rates a single simple command. A line holding more than one
// command, a redirect or a substitution is CatUnknown here; ClassifyLine is
// the entry point that looks at every command of a compound line.
func (c *Classifier) Classify(cmd string) CmdCategory {
	hostile := safety.HasHostileCharacters(cmd)
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return CatSafe
	}
	if c.isDangerous(cmd) {
		return CatDangerous
	}
	if hostile || c.hasShellControlOperator(cmd) {
		return CatUnknown
	}
	simple, ok := shellCommands(cmd, "")
	if !ok || len(simple) == 0 {
		return CatUnknown
	}
	return c.classifyWords(simple[0].Words)
}

// ClassifyLine rates a whole command line: every simple command the shell
// would start is classified on its own tokenized words and the riskiest
// category wins. Command or process substitution anywhere in the line, ${...}
// expansion, and any output redirect to a real file make the line CatUnknown.
// Quotes are not trusted (cmd.exe rules); ClassifyLineFor takes the shell.
func (c *Classifier) ClassifyLine(command string) CmdCategory {
	return c.ClassifyLineFor(command, "")
}

// ClassifyLineFor is ClassifyLine for a line run by a shell of the given
// kind. A command with a word the tokenizer may have misread (an operator
// character outside trusted quotes, an unquoted $ under PowerShell; see
// literalWords) is CatUnknown.
func (c *Classifier) ClassifyLineFor(command string, kind ShellKind) CmdCategory {
	cat, _ := c.classifyLine(command, kind)
	return cat
}

// classifyLine is ClassifyLineFor that also returns the simple commands it
// tokenized (nil when it decided before tokenizing), so IsReadOnlyLineFor
// can look at them without splitting the line a second time.
func (c *Classifier) classifyLine(command string, kind ShellKind) (CmdCategory, []safety.SimpleCommand) {
	hostile := safety.HasHostileCharacters(command) // before trimming: a trailing lone CR counts
	command = strings.TrimSpace(command)
	if command == "" {
		return CatUnknown, nil
	}
	if c.isDangerous(command) {
		return CatDangerous, nil
	}
	// A lone CR, FF, VT, a Unicode space or separator, a control or an
	// invisible format character: PowerShell splits commands or words where
	// the tokenizer may not, and the person may not see it. Such a line is
	// never read-only or auto-approved ("ls<CR>Remove-Item ..." was a
	// read-only ls).
	if hostile {
		return CatUnknown, nil
	}
	if hasSubstitution(command) || maybePowerShell(kind) && (hasUnquotedBrace(command) || hasTypographicQuote(command)) {
		return CatUnknown, nil
	}
	simple, ok := shellCommands(command, kind)
	if !ok || len(simple) == 0 || kind == ShellPOSIX && literalUNC(command) {
		return CatUnknown, nil
	}
	trustQuotes := quotingTrusted(command, kind)
	worst := CatSafe
	for _, sc := range simple {
		cat := CatUnknown
		if len(sc.Words) > 0 && !writesFile(sc.Redirects) && literalWords(sc, kind, trustQuotes) &&
			(!maybePowerShell(kind) || !powerShellIterator(sc.Words[0])) {
			cat = c.classifyWords(sc.Words)
			if maybePowerShell(kind) && powerShellFetch(sc.Words[0]) {
				// Under PowerShell curl/wget are Invoke-WebRequest and only its
				// allowlist applies; under the unknown kind both must pass.
				// pwsh 6+ dropped the curl/wget aliases, so under it "wget URL"
				// is GNU wget writing a file; the 5.1-alias reading no longer
				// upgrades an unknown curl/wget line to safe.
				if !powerShellFetchReadOnly(sc.Words[1:]) {
					cat = CatUnknown
				}
			}
		}
		if riskRank(cat) > riskRank(worst) {
			worst = cat
		}
	}
	return worst, simple
}

// IsReadOnlyLine reports whether every command in the line is read-only, so
// default mode may run it without asking. Network fetches (curl/wget) are
// CatSafe for the classifier but still egress, so they are excluded here and
// keep asking in default mode.
func (c *Classifier) IsReadOnlyLine(command string) bool {
	return c.IsReadOnlyLineFor(command, "")
}

// IsReadOnlyLineFor is IsReadOnlyLine for a line run by a shell of kind.
// Under ShellCmd (the bash tool fell back to cmd.exe) it is always false: the
// tokenizer does not model cmd's ^ escapes, %VAR% expansion or its quoting,
// so no line is trusted to be read-only there and every command asks. The
// zero ShellKind keeps the classification (strict quoting) for callers that
// only want to know what a line does.
func (c *Classifier) IsReadOnlyLineFor(command string, kind ShellKind) bool {
	if kind == ShellCmd {
		return false
	}
	cat, simple := c.classifyLine(command, kind)
	if cat != CatSafe {
		return false
	}
	for _, sc := range simple {
		if len(sc.Words) > 0 {
			switch programName(sc.Words[0]) {
			case "curl", "wget":
				return false
			}
		}
	}
	return true
}

// readOnlyCommandWords is IsReadOnlyLineFor for one simple command given as
// its words: rated CatSafe, not a network fetch, not a PowerShell iterator.
// Under ShellCmd nothing qualifies, for the reason IsReadOnlyLineFor gives.
// Callers have already checked the command's redirects and quoting
// (coverableCommands), which classifyLine does for a whole line.
func (c *Classifier) readOnlyCommandWords(words []string, kind ShellKind) bool {
	if kind == ShellCmd || len(words) == 0 {
		return false
	}
	if maybePowerShell(kind) && powerShellIterator(words[0]) {
		return false
	}
	switch programName(words[0]) {
	case "curl", "wget":
		return false
	}
	return c.classifyWords(words) == CatSafe
}

// AutoApproveLine is the auto-mode test: every command in the line is
// read-only or a build/test command.
func (c *Classifier) AutoApproveLine(command string) bool {
	return c.AutoApproveLineFor(command, "")
}

// AutoApproveLineFor is AutoApproveLine for a line run by a shell of kind.
// Like IsReadOnlyLineFor it is always false under ShellCmd: auto mode does
// not run anything unasked through a cmd.exe fallback.
func (c *Classifier) AutoApproveLineFor(command string, kind ShellKind) bool {
	if kind == ShellCmd {
		return false
	}
	cat, simple := c.classifyLine(command, kind)
	if cat != CatSafe && cat != CatBuild {
		return false
	}
	// Network fetches are egress and keep asking in auto mode too, as the
	// manual says. A GET used to run unasked, and a URL or header carries
	// whatever the shell expands into it: curl "https://x/?k=$API_KEY".
	for _, sc := range simple {
		if len(sc.Words) > 0 && networkFetchProgram(programName(sc.Words[0])) {
			return false
		}
	}
	return true
}

// networkFetchProgram is a program that sends a request out: curl, wget and
// PowerShell's Invoke-WebRequest / Invoke-RestMethod with their aliases.
func networkFetchProgram(name string) bool {
	switch strings.ToLower(name) {
	case "curl", "wget", "iwr", "irm", "invoke-webrequest", "invoke-restmethod":
		return true
	}
	return false
}

func hasSubstitution(command string) bool {
	for _, op := range []string{"`", "$(", "<(", ">(", "${"} {
		if strings.Contains(command, op) {
			return true
		}
	}
	return false
}

// maybePowerShell reports whether a line of this kind may be run by
// PowerShell: PowerShell itself, or an unknown shell (treated strictly).
func maybePowerShell(kind ShellKind) bool {
	return kind == ShellPowerShell || kind == ""
}

// powerShellIterator names the PowerShell cmdlets and aliases that run a
// scriptblock or member for every pipeline item (where { rm $_ }, % Delete).
func powerShellIterator(word string) bool {
	switch programName(word) {
	case "where", "where-object", "?", "foreach", "foreach-object", "%":
		return true
	}
	return false
}

// hasTypographicQuote reports U+2018–U+201F (‘ ’ ‚ ‛ “ ” „ ‟). PowerShell
// treats them as ' and ", the tokenizer does not, so under PowerShell any
// line containing one is never trusted: quote-dependent checks (scriptblock
// braces, ; | & inside quotes) would be reading a different line.
func hasTypographicQuote(command string) bool {
	for _, r := range command {
		if r >= '\u2018' && r <= '\u201f' {
			return true
		}
	}
	return false
}

// hasUnquotedBrace reports a { or } outside '...' and "..." — a PowerShell
// scriptblock, which runs code wherever it is passed.
func hasUnquotedBrace(command string) bool {
	var q rune
	for _, r := range command {
		switch {
		case q != 0:
			if r == q {
				q = 0
			}
		case r == '\'' || r == '"':
			q = r
		case r == '{' || r == '}':
			return true
		}
	}
	return false
}

func writesFile(redirects []string) bool {
	for _, r := range redirects {
		if !discardTarget(r) {
			return true
		}
	}
	return false
}

// isDangerous is the hard-block test the engine applies in every permission
// mode, so it only covers damage outside the project (see
// safety.CatastrophicCommand). Everything else that writes or deletes is
// CatUnknown and goes through the normal approval prompt.
func (c *Classifier) isDangerous(cmd string) bool {
	_, ok := safety.CatastrophicCommand(cmd)
	return ok
}

func (c *Classifier) hasShellControlOperator(cmd string) bool {
	// "${" is included alongside the classic "$(" / backtick substitution
	// markers: brace parameter expansion can also be used to construct or
	// hide command content that a keyword scan wouldn't recognize (beyond
	// the specific $IFS case already escalated to CatDangerous above), so
	// it's treated the same as other substitution syntax — forced to
	// CatUnknown for manual review rather than silently classified.
	operators := []string{"&&", "||", ";", "|", "`", "$(", "${", ">", "<"}
	for _, op := range operators {
		if strings.Contains(cmd, op) {
			return true
		}
	}
	return false
}

// isUNCPath reports whether a word (or the value of a --flag=value word)
// names a UNC / SMB path such as \\host\share or //host/share. Opening one
// contacts another host, and on Windows sends the user's credentials to it,
// so a "read" of it is not a local read. A leading separator pair followed by
// a space or another separator (a "// TODO" grep pattern) is not a host name.
//
// Only the word start and the text after the first '=' used to be checked,
// so PowerShell's other ways of starting a path inside a word ran unasked:
// "-Path:\\host\share" (parameter:value), "FileSystem::\\host\share" and
// "Microsoft.PowerShell.Core\FileSystem::\\host\share" (provider-qualified),
// ".,\\host\share" (an array argument). A path may now begin at the word
// start or after any '=', ',' or '(', after "::", after a ':' that does not
// end a drive letter ("C:\\Users" in bash's doubled spelling is local), and
// "//host" after a -Parameter: (a URL's "https://" is not one).
func isUNCPath(word string) bool {
	check := func(w string) bool {
		w = strings.Trim(w, `"'`)
		if len(w) < 3 || !strings.HasPrefix(w, `\\`) && !strings.HasPrefix(w, "//") {
			return false
		}
		switch w[2] {
		case '/', '\\', ' ', '\t':
			return false
		}
		return true
	}
	if check(word) {
		return true
	}
	for i := 0; i < len(word); i++ {
		rest := word[i+1:]
		switch word[i] {
		case '=', ',', '(':
			if check(rest) {
				return true
			}
		case ':':
			switch {
			case i > 0 && word[i-1] == ':':
				if check(rest) {
					return true
				}
			case strings.HasPrefix(strings.TrimLeft(rest, `"'`), `\\`):
				if !driveLetterBefore(word, i) && check(rest) {
					return true
				}
			case strings.HasPrefix(word, "-") && check(rest):
				return true
			}
		}
	}
	return false
}

// driveLetterBefore reports whether the ':' at word[i] ends a drive letter:
// a single ASCII letter at the word start or after a separator ("C:",
// "-Path:C:", "a,D:").
func driveLetterBefore(word string, i int) bool {
	if i == 0 {
		return false
	}
	c := word[i-1] | 0x20
	if c < 'a' || c > 'z' {
		return false
	}
	if i == 1 {
		return true
	}
	switch word[i-2] {
	case ':', '=', ',', '(', '"', '\'':
		return true
	}
	return false
}

// classifyWords rates one simple command from its words exactly as written.
// Anything run through a wrapper (env, sudo, xargs, time, nohup), started via
// a path, or preceded by VAR=value is CatUnknown: the classifier only vouches
// for commands it can name.
func (c *Classifier) classifyWords(words []string) CmdCategory {
	if len(words) == 0 {
		return CatUnknown
	}
	exe := words[0]
	if strings.ContainsAny(exe, `/\$=*?[`) || strings.HasPrefix(exe, "-") {
		return CatUnknown
	}
	name := programName(exe)
	args := words[1:]
	for _, a := range args {
		if isUNCPath(a) {
			return CatUnknown
		}
	}
	switch name {
	case "git":
		return c.classifyGit(args)
	case "ls", "dir", "pwd", "echo", "cat", "head", "tail", "wc", "du", "df", "printenv",
		"which", "where", "whoami", "uname", "uptime", "id", "groups", "grep", "egrep", "fgrep",
		"locate", "stat", "type", "realpath", "basename", "dirname", "true",
		"get-childitem", "gci", "get-content", "gc", "get-location", "gl", "select-string", "sls",
		"test-path", "get-item", "gi", "resolve-path", "get-command", "gcm",
		// Changing directory alters nothing by itself; the commands run
		// there are rated on their own. A UNC target is refused above.
		"cd", "pushd", "popd", "set-location", "sl":
		return CatSafe
	case "command":
		// "command -v gh" / "command -V gh" only looks a name up; plain
		// "command x" runs x.
		if len(args) >= 2 && (args[0] == "-v" || args[0] == "-V") {
			return CatSafe
		}
		return CatUnknown
	case "date":
		return classifyDate(args)
	case "hostname":
		if len(args) == 0 {
			return CatSafe
		}
		return CatUnknown
	case "ag":
		// --pager runs a program. ag parses with getopt_long, which takes any
		// unambiguous prefix, so "--pag=x" is --pager too; only the exact
		// spelling used to be refused. Every prefix down to "--p" asks (the
		// shorter ones are ambiguous and ag rejects them anyway).
		for _, a := range args {
			if a == "--" {
				break
			}
			if name := optionName(a); len(name) >= len("--p") && strings.HasPrefix("--pager", name) {
				return CatUnknown
			}
		}
		return CatSafe
	case "rg":
		// --pre and --hostname-bin run a program (the latter was missed).
		// ripgrep's parser (lexopt; clap before 14) takes no abbreviations,
		// so the exact names are enough.
		for _, a := range args {
			if a == "--" {
				break
			}
			switch optionName(a) {
			case "--pre", "--pre-glob", "--hostname-bin":
				return CatUnknown
			}
		}
		return CatSafe
	case "tree":
		// -o writes a file, -H emits HTML, -R writes 00Tree.html per directory;
		// short options combine (-aR, -ao out.txt, -fH .).
		for _, a := range args {
			if strings.HasPrefix(a, "--output") {
				return CatUnknown
			}
			if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], "oHR") {
				return CatUnknown
			}
		}
		return CatSafe
	case "file":
		// -C/--compile writes a .mgc file. Only the exact spellings used to
		// be refused; GNU file takes getopt_long abbreviations ("--comp")
		// and grouped short options ("-zC").
		for _, a := range args {
			if a == "--" {
				break
			}
			if name := optionName(a); strings.HasPrefix(a, "--") && len(name) >= len("--c") && strings.HasPrefix("--compile", name) {
				return CatUnknown
			}
			if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a[1:], 'C') {
				return CatUnknown
			}
		}
		return CatSafe
	case "find":
		return classifyFind(args)
	case "go", "cargo", "rustc", "javac", "tsc", "make", "cmake", "ninja", "bazel", "meson":
		return c.classifyBuild(name, args)
	case "dotnet":
		return classifyDotnet(args)
	case "pytest", "gofmt":
		return classifyTestTool(name, args)
	case "npm", "yarn", "pnpm", "pip", "pip3", "gem", "composer", "nuget", "apt", "apt-get",
		"yum", "dnf", "brew", "choco", "winget", "pacman", "zypper", "snap", "flatpak":
		return c.classifyPackageManager(name, args)
	case "docker", "podman", "nerdctl":
		return c.classifyDocker(args)
	case "curl", "wget":
		return c.classifyNetworkingWords(words)
	default:
		return CatUnknown
	}
}

// classifyDate allows date only when it cannot set the clock. The old check
// matched "-s" and "--set..." as whole words, missing the abbreviation
// "--s"/"--se" (GNU getopt accepts unambiguous prefixes) and -s grouped with
// other letters ("-us"). Now any short group holding s, any prefix of --set,
// and any operand other than a +FORMAT (GNU date MMDDhhmm[[CC]YY] sets the
// clock) is CatUnknown.
func classifyDate(args []string) CmdCategory {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a, "=")
			if len(name) >= len("--s") && strings.HasPrefix("--set", name) {
				return CatUnknown
			}
			if !hasValue && (name == "--date" || name == "--file" || name == "--reference") {
				i++ // the value
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// Letters are read as getopt does: u R j n are flags, d f r v
			// take the rest of the group or the next word as their value,
			// I takes an optional glued value. Anything else (s included)
			// is CatUnknown.
		letters:
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'u', 'R', 'j', 'n':
				case 'I':
					break letters
				case 'd', 'f', 'r', 'v':
					if j == len(a)-1 {
						i++ // the value is the next word
					}
					break letters
				default:
					return CatUnknown
				}
			}
		case !strings.HasPrefix(a, "+"):
			return CatUnknown
		}
	}
	return CatSafe
}

// classifyFind allows find only without its actions that delete, run
// commands or write files.
func classifyFind(args []string) CmdCategory {
	for _, a := range args {
		switch {
		case a == "-delete", a == "-exec", a == "-execdir", a == "-ok", a == "-okdir",
			strings.HasPrefix(a, "-fprint"), a == "-fls":
			return CatUnknown
		}
	}
	return CatSafe
}

func hasAny(args []string, want ...string) bool {
	for _, a := range args {
		for _, w := range want {
			if a == w {
				return true
			}
		}
	}
	return false
}

// onlyFlagsFrom reports whether every arg is one of allowed.
func onlyFlagsFrom(args []string, allowed ...string) bool {
	for _, a := range args {
		if !hasAny([]string{a}, allowed...) {
			return false
		}
	}
	return true
}

// classifyNetworking rates curl/wget invocations.
//
// The default is CatUnknown (ask the user), not CatSafe. The old default
// auto-approved anything it did not specifically recognize, which covered the
// two cases that matter most: `curl -X POST -d @secrets.json <url>` exfiltrates
// data, and `wget <url>` (no -O) writes a file into the working directory —
// both ran without a prompt. Only genuinely read-only fetches are auto-approved.
func (c *Classifier) classifyNetworking(cmd string) CmdCategory {
	return c.classifyNetworkingWords(strings.Fields(cmd))
}

func (c *Classifier) classifyNetworkingWords(fields []string) CmdCategory {
	// An allowlist, not a list of known-bad options: the old denylist rated
	// every option it had not heard of as a read, so "curl --json @~/.ssh/id_rsa
	// URL" uploaded a key, "curl -XDELETE URL" (method glued to -X) deleted, and
	// -D, --trace, -c and --stderr wrote files — all CatSafe, all run unasked
	// in auto mode. Now every option must be a known read-only one, spelled
	// exactly (curl's unambiguous abbreviations fail closed), and the method
	// must be GET or HEAD.
	//
	// Short flags are matched CASE-SENSITIVELY on tokenized arguments: for
	// curl -F is a form upload while -f is --fail, -T an upload while -t is
	// unrelated.
	if len(fields) == 0 {
		return CatUnknown
	}
	spec := curlReadOnly
	wget := programName(fields[0]) == "wget"
	if wget {
		spec = wgetReadOnly
	}
	args := fields[1:]
	// wget writes a file by default, so it is only a read with an explicit
	// stdout target (-O -).
	toStdout := false
	value := func(opt, v string) bool {
		switch opt {
		case "X", "--request":
			m := strings.ToUpper(v)
			return m == "GET" || m == "HEAD"
		case "o", "O", "--output", "--output-document":
			// Only stdout: any other target writes a file.
			if v != "-" {
				return false
			}
			toStdout = true
			return true
		case "H", "--header":
			// "-H @file" reads headers from a file and sends them.
			return !strings.HasPrefix(v, "@")
		}
		return true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--" || a == "-":
			return CatUnknown
		case strings.HasPrefix(a, "--"):
			name, v, hasValue := strings.Cut(a, "=")
			switch {
			case spec.longFlags[name] && !hasValue:
			case spec.longValues[name]:
				if !hasValue {
					if i+1 >= len(args) {
						return CatUnknown
					}
					i++
					v = args[i]
				}
				if !value(name, v) {
					return CatUnknown
				}
			default:
				return CatUnknown
			}
		case strings.HasPrefix(a, "-"):
			group := a[1:]
			for j := 0; j < len(group); j++ {
				letter := group[j : j+1]
				if strings.Contains(spec.shortFlags, letter) {
					continue
				}
				if !strings.Contains(spec.shortValues, letter) {
					return CatUnknown
				}
				// A value-taking letter ends the group: the rest of it, or the
				// next word, is its value (-XGET, -m5, -qO-, -X GET).
				v := group[j+1:]
				if v == "" {
					if i+1 >= len(args) {
						return CatUnknown
					}
					i++
					v = args[i]
				}
				if !value(letter, v) {
					return CatUnknown
				}
				break
			}
		}
	}
	if wget && !toStdout {
		return CatUnknown
	}
	return CatSafe
}

// netReadOnly lists the options of a fetch tool that keep it a read: flags
// without a value and options taking one, short letters and long names. A
// value is checked further in classifyNetworkingWords (method, output target,
// header file).
type netReadOnly struct {
	shortFlags, shortValues string
	longFlags, longValues   map[string]bool
}

// curlReadOnly: output shaping, redirects, timeouts, retries, protocol
// versions, headers. Not in it, among others: -d/--data*/--json/-F/-T/
// --upload-file/--url-query (send data), -o/-O/-D/-c/--trace*/--stderr/
// --remote-name* (write files; -o - is stdout), -K/--config (options from a
// file), -b/-u/-x (cookies, credentials, proxy), -w (%output{} writes a file),
// -k/--insecure.
var curlReadOnly = netReadOnly{
	shortFlags:  "sSfLIiv46gGNq0123#jl",
	shortValues: "XoHAemr",
	longFlags: map[string]bool{
		"--silent": true, "--show-error": true, "--fail": true, "--fail-with-body": true,
		"--location": true, "--head": true, "--include": true, "--verbose": true,
		"--compressed": true, "--globoff": true, "--get": true, "--no-buffer": true,
		"--no-progress-meter": true, "--progress-bar": true, "--ipv4": true, "--ipv6": true,
		"--http1.0": true, "--http1.1": true, "--http2": true, "--http2-prior-knowledge": true,
		"--http3": true, "--tlsv1": true, "--tlsv1.0": true, "--tlsv1.1": true, "--tlsv1.2": true,
		"--tlsv1.3": true, "--list-only": true,
	},
	longValues: map[string]bool{
		"--request": true, "--output": true, "--header": true, "--user-agent": true,
		"--referer": true, "--max-time": true, "--connect-timeout": true, "--retry": true,
		"--retry-delay": true, "--retry-max-time": true, "--range": true, "--max-redirs": true,
		"--url": true,
	},
}

// wgetReadOnly: only a fetch printed to stdout (-O -), quiet or verbose.
var wgetReadOnly = netReadOnly{
	shortFlags:  "qvS",
	shortValues: "OUT",
	longFlags: map[string]bool{
		"--quiet": true, "--verbose": true, "--server-response": true, "--no-verbose": true,
	},
	longValues: map[string]bool{
		"--output-document": true, "--user-agent": true, "--timeout": true, "--tries": true,
		"--header": true,
	},
}

// powerShellFetch names the programs a PowerShell line may run as
// Invoke-WebRequest or Invoke-RestMethod; see powerShellFetchReadOnly.
func powerShellFetch(word string) bool {
	switch programName(word) {
	case "curl", "wget", "iwr", "irm", "invoke-webrequest", "invoke-restmethod":
		return true
	}
	return false
}

// isCurlOrWget reports the two fetch aliases that may be rated CatSafe; iwr,
// irm and the cmdlets always ask.
func isCurlOrWget(word string) bool {
	switch programName(word) {
	case "curl", "wget":
		return true
	}
	return false
}

// powerShellFetchReadOnly is the test for curl and wget under PowerShell,
// where Windows PowerShell 5.1 aliases both to Invoke-WebRequest: -Method,
// -InFile, -Body and -OutFile mean nothing to curl's rules. Only one URL,
// given bare or with -Uri, and -UseBasicParsing make it a GET that prints the
// response; everything else (curl options included, which Invoke-WebRequest
// would read as its own parameters) asks.
func powerShellFetchReadOnly(args []string) bool {
	urls := 0
	for i := 0; i < len(args); i++ {
		switch a := strings.ToLower(args[i]); {
		case a == "-usebasicparsing":
		case a == "-uri":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return false
			}
			i++
			urls++
		case strings.HasPrefix(a, "-"):
			return false
		default:
			urls++
		}
	}
	return urls == 1
}

func (c *Classifier) ShouldAutoApprove(cmd string) bool {
	cat := c.Classify(cmd)
	return cat == CatSafe || cat == CatBuild
}

func (c *Classifier) Explain(cmd string) string {
	cat := c.Classify(cmd)
	switch cat {
	case CatSafe:
		return "safe: read-only operation"
	case CatGit:
		return "git: may modify repository"
	case CatBuild:
		return "build/test: runs code, may write artifacts"
	case CatInstall:
		return "install: installs packages, may modify system"
	case CatDangerous:
		return "DANGEROUS: may damage system"
	default:
		return "unknown: manual review recommended"
	}
}
