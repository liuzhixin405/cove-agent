package permission

import (
	"regexp"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/safety"
)

// IsShellTool reports whether a tool runs its "command" input through a shell,
// so an "always allow" answer for it is scoped to a command prefix instead of
// the whole tool.
func IsShellTool(name string) bool {
	return strings.EqualFold(name, "bash") || strings.EqualFold(name, "powershell")
}

// CommandPrefixes returns the prefixes an "always allow" answer for command
// should remember: one per distinct simple command in the line, deduplicated
// and in order. A prefix is the executable plus its subcommand for tools whose
// first argument picks what they do (git status, go test, npm run, docker
// compose), and just the executable otherwise (ls, cat). "Allow git" would
// otherwise grant git push and git reset --hard when the user only saw git
// status.
//
// It reports false when no prefix can be remembered safely: a command that
// runs another command (sudo, env, xargs, bash -c ...), a VAR=value or
// variable in front, a subcommand tool without a subcommand, or a line that
// commandCovered would refuse anyway. The rules it returns always cover the
// command they came from.
func CommandPrefixes(command string) ([]string, bool) {
	return CommandPrefixesFor(command, "")
}

// CommandPrefixesFor is CommandPrefixes for a command that runs under the
// given shell: under POSIX shells and PowerShell a fully quoted word may hold
// operator characters (a commit message such as "fix(api): x; y"), under cmd
// (and the zero ShellKind) it may not.
//
// Read-only commands in the line (cd, echo, git log, ...) yield no prefix:
// they run unasked in every mode and a prefix rule treats them as covered
// (see commandCovered), so remembering them only lengthened the prompt. A
// line made only of them offers nothing. Under ShellCmd no command is trusted
// to be read-only, so every one is remembered there.
func CommandPrefixesFor(command string, kind ShellKind) ([]string, bool) {
	cmds, ok := coverableCommands(command, kind)
	if !ok {
		return nil, false
	}
	var out []string
	seen := map[string]bool{}
	for _, words := range cmds {
		if prefixClassifier.readOnlyCommandWords(words, kind) {
			continue
		}
		p, ok := commandPrefix(words)
		if !ok {
			return nil, false
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// prefixClassifier rates the commands of a line for the prefix machinery.
var prefixClassifier = NewClassifier()

// ShellRememberRules returns the allow rules an "always allow" answer for a
// shell tool call should add: one GroupGitRoutine rule when the line holds a
// routine git write (see gitRoutine), and one CommandPrefix rule per other
// command that needed approval, in order of first appearance. Read-only
// commands add nothing. It reports false when some command cannot be
// remembered safely (CommandPrefixesFor lists the reasons) or nothing is
// left to remember; the rules it returns always cover command.
func ShellRememberRules(toolName, command string, kind ShellKind) ([]Rule, bool) {
	cmds, ok := coverableCommands(command, kind)
	if !ok {
		return nil, false
	}
	var rules []Rule
	seen := map[string]bool{}
	for _, words := range cmds {
		if prefixClassifier.readOnlyCommandWords(words, kind) {
			continue
		}
		if id := routineGroupOf(words); id != "" {
			if !seen["group:"+id] {
				seen["group:"+id] = true
				rules = append(rules, Rule{ToolPattern: toolName, CommandGroup: id})
			}
			continue
		}
		p, ok := commandPrefix(words)
		if !ok {
			return nil, false
		}
		if !seen["prefix:"+p] {
			seen["prefix:"+p] = true
			rules = append(rules, Rule{ToolPattern: toolName, CommandPrefix: p})
		}
	}
	if len(rules) == 0 {
		return nil, false
	}
	return rules, true
}

var (
	// subcommandTools take a subcommand as their first argument, and the
	// subcommands differ enough in risk that allowing one must not allow all.
	subcommandTools = map[string]bool{
		"git": true, "gh": true, "glab": true,
		"go": true, "cargo": true, "rustup": true, "dotnet": true,
		"npm": true, "pnpm": true, "yarn": true, "bun": true, "deno": true, "npx": true,
		"pip": true, "pip3": true, "uv": true, "poetry": true, "conda": true,
		"docker": true, "podman": true, "kubectl": true, "helm": true, "terraform": true,
		"mvn": true, "gradle": true, "gradlew": true, "composer": true, "bundle": true, "gem": true,
		"brew": true, "apt": true, "apt-get": true, "dnf": true, "yum": true,
		"winget": true, "choco": true, "scoop": true, "systemctl": true,
		"az": true, "aws": true, "gcloud": true, "flutter": true, "dart": true,
	}
	// commandRunners exist to run some other command, so a prefix made of
	// them would allow anything at all.
	commandRunners = map[string]bool{
		"sudo": true, "doas": true, "su": true, "runas": true,
		"env": true, "nohup": true, "time": true, "command": true, "nice": true,
		"exec": true, "eval": true, "source": true, ".": true, "xargs": true,
		"timeout": true, "watch": true, "chroot": true, "ssh": true, "busybox": true,
		"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
		"cmd": true, "pwsh": true, "powershell": true, "start": true, "call": true,
		"iex": true, "invoke-expression": true, "invoke-command": true, "icm": true,
		"start-process": true,
	}
	subcommandWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._:-]*$`)
)

func commandPrefix(words []string) (string, bool) {
	exe := words[0]
	// "%" is refused like "$": cmd expands %GIT% before it runs anything, so
	// a remembered "%GIT%" prefix would allow whatever the variable names.
	if strings.ContainsAny(exe, " \t$%*?[=") || strings.HasPrefix(exe, "-") {
		return "", false
	}
	name := programName(exe)
	if commandRunners[name] {
		return "", false
	}
	if !subcommandTools[name] {
		return exe, true
	}
	if len(words) < 2 || !subcommandWord.MatchString(words[1]) {
		return "", false
	}
	return exe + " " + words[1], true
}

// programName normalizes an executable word the way the shell resolves it:
// case-insensitive on Windows, directory and .exe dropped.
func programName(exe string) string { return safety.ProgramName(exe) }

// The rules the allow-rule pool applies. coverage is what it vouches for:
// command prefixes (hasWordPrefix), command groups (groupCovers) and,
// implicitly, read-only commands under a shell whose kind is trusted.
//
//   - Each command of a compound line (&& || ; & | newlines, subshells) must
//     match on its own, so "go test ./... | tee out.txt" also needs "tee".
//   - Words are compared as written: "sudo go test", "FOO=1 go test" and
//     "env go test" do not start with "go test".
//   - Command and process substitution ($(...), backticks, <(...), >(...))
//     never matches, even inside quotes where bash and PowerShell still
//     expand it.
//   - Output redirects only match when they discard (/dev/null, NUL, $null);
//     writing a file is not part of running the allowed command.
//   - A word holding an operator character (; & | < > parentheses) only
//     matches when the tokenizer saw the whole word inside quotes and the
//     shell honours those quotes (POSIX sh/bash, PowerShell), and no quote in
//     the line is preceded by a backslash (\" in bash). Under cmd, where a
//     single quote is an ordinary character, such a word never matches.
//   - Here-document and here-string bodies are stdin data, not commands.
//   - Under bash the words are read after its line continuation and
//     backslash quoting (see shellCommands); under the unknown kind a line
//     whose backslashes change its meaning never matches.
//   - A read-only command (cd, echo, git status; see readOnlyCommandWords)
//     counts as covered without a rule, except under cmd.exe: it would run
//     unasked on its own, so chaining it to an allowed command changes
//     nothing.
//
// Anything refused here simply falls back to asking the user again.
type coverage struct {
	prefixes [][]string
	groups   []string
	kind     ShellKind
}

func (c coverage) covers(words []string) bool {
	scoped := words
	if c.kind == ShellPOSIX && len(words) > 1 && words[0] == "time" {
		scoped = words[1:]
		if scoped[0] == "-p" {
			scoped = scoped[1:]
		}
	}
	for _, p := range c.prefixes {
		if hasWordPrefix(words, p) || hasWordPrefix(scoped, p) {
			return true
		}
	}
	for _, g := range c.groups {
		if groupCovers(g, words) || groupCovers(g, scoped) {
			return true
		}
	}
	return prefixClassifier.readOnlyCommandWords(words, c.kind)
}

// lineCovered is commandCovered for a pool of prefixes and groups.
func lineCovered(command string, cov coverage) bool {
	cmds, ok := coverableCommands(command, cov.kind)
	if !ok {
		return false
	}
	for _, words := range cmds {
		if !cov.covers(words) {
			return false
		}
	}
	return true
}

// coverableCommands splits command into the words of each simple command, or
// reports false when the line contains something commandCovered refuses.
func coverableCommands(command string, kind ShellKind) ([][]string, bool) {
	// A line with a lone CR, FF, VT, a Unicode space or separator, a control
	// or a format character is never covered: PowerShell runs "go test
	// ./...<CR>Remove-Item ..." as two commands, and the tokenizer used to see
	// one go test. See safety.HasHostileCharacters.
	if safety.HasHostileCharacters(command) {
		return nil, false
	}
	// A catastrophic line is hard-blocked in every mode, so nothing about it
	// may be remembered or covered. ShellRememberRules used to offer "rm" for
	// "rm -rf /" and "mkfs.ext4" for "mkfs.ext4 /dev/sda1": unreachable while
	// the engine blocks first, but a rule the person saved from such a prompt
	// would name exactly the command the block exists for.
	if _, bad := safety.CatastrophicCommand(command); bad {
		return nil, false
	}
	if hasSubstitution(command) || maybePowerShell(kind) && (hasUnquotedBrace(command) || hasTypographicQuote(command)) {
		return nil, false
	}
	trustQuotes := quotingTrusted(command, kind)
	simple, ok := shellCommands(command, kind)
	if !ok || len(simple) == 0 || kind == ShellPOSIX && literalUNC(command) {
		return nil, false
	}
	out := make([][]string, 0, len(simple))
	for _, c := range simple {
		if len(c.Words) == 0 {
			return nil, false
		}
		for _, r := range c.Redirects {
			if !discardTarget(r) {
				return nil, false
			}
		}
		if !literalWords(c, kind, trustQuotes) {
			return nil, false
		}
		out = append(out, c.Words)
	}
	return out, true
}

// quotingTrusted reports whether a word the tokenizer saw entirely inside
// quotes is a literal argument for the shell command runs under: true for
// POSIX shells and PowerShell, unless a backslash precedes a quote somewhere
// in the line — the tokenizer keeps backslashes literally, so there it and
// bash (\" outside quotes is a literal quote) can disagree about where a
// quoted string ends. cmd.exe, where ' is an ordinary character, never
// qualifies.
func quotingTrusted(command string, kind ShellKind) bool {
	return kind.trustsQuotes() &&
		!strings.Contains(command, `\"`) && !strings.Contains(command, `\'`)
}

// literalWords reports whether every word of c is certainly a plain argument
// rather than shell syntax the tokenizer may have misread: a word holding an
// operator character must be fully quoted under a shell whose quotes are
// trusted, under PowerShell an unquoted $ is refused because $var.Method(...)
// in argument position runs code, and a bash brace expansion ({a,b}, {1..3})
// is refused because it turns one word into several ("git log
// {--output=x,HEAD}" writes a file). The check for a CR in a word was dead
// and is gone: a line holding one outside a CRLF is refused before
// (safety.HasHostileCharacters), and the CR of a CRLF is a blank.
func literalWords(c safety.SimpleCommand, kind ShellKind, trustQuotes bool) bool {
	for i, w := range c.Words {
		if trustQuotes && i < len(c.Quoted) && c.Quoted[i] {
			continue
		}
		if strings.ContainsAny(w, ";&|<>()\n") || braceExpansion(w) {
			return false
		}
		if kind == ShellPowerShell && strings.Contains(w, "$") {
			return false
		}
	}
	return true
}

// braceExpansion reports a {...} in w that bash would expand: one holding a
// comma or "..". The reflog spellings stash@{0}, HEAD@{1}, @{u} and
// main@{yesterday} are literal.
func braceExpansion(w string) bool {
	for {
		open := strings.IndexByte(w, '{')
		if open < 0 {
			return false
		}
		end := strings.IndexByte(w[open:], '}')
		if end < 0 {
			return false
		}
		if inner := w[open+1 : open+end]; strings.ContainsRune(inner, ',') || strings.Contains(inner, "..") {
			return true
		}
		w = w[open+end+1:]
	}
}

func discardTarget(path string) bool {
	switch strings.ToLower(path) {
	case "/dev/null", "nul", "$null":
		return true
	}
	return false
}

func hasWordPrefix(words, prefix []string) bool {
	if len(prefix) == 0 || len(words) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if words[i] != p {
			return false
		}
	}
	return true
}

// anyCommandHasPrefixNormalized is the deny/ask reading of a prefix rule:
// the rule applies as soon as one simple command in the line starts with the
// prefix, including commands inside substitutions. Unlike the allow reading
// (commandCovered, words as written) it looks through spellings that run the
// same program: a directory or .exe on the program name and any letter case
// ("/usr/bin/git", "git.exe", "GIT"), runners and assignments in front
// ("sudo", "env X=1", "command", "nohup", "X=1"; see
// safety.StripCommandRunners), the global options a subcommand tool takes
// before its subcommand ("git -C . push", "kubectl -n prod delete"), and
// bash's backslash reading of the line as well as the literal one ("r\m" is
// rm under bash; see anyReading), and the commands the line runs indirectly
// (bash -c, eval, xargs, find -exec ...; see denyReading). A git alias
// defined on the line matches every rule on git, and a nested command whose
// program is a variable matches every rule.
// Widening a deny or ask rule only ever asks or refuses more often.
func anyCommandHasPrefixNormalized(command string, prefix []string) bool {
	if len(prefix) == 0 {
		return false
	}
	want := normalizeProgram(prefix)
	cmds, opaque := denyReading(command)
	if opaque {
		return true
	}
	for _, c := range cmds {
		if hasWordPrefix(c.Words, prefix) ||
			hasWordPrefix(normalizeProgram(c.Words), want) ||
			hasWordPrefix(normalizeProgram(safety.StripCommandRunners(c.Words)), want) ||
			want[0] == "git" && definesGitAlias(c.Words) {
			return true
		}
	}
	return false
}

// normalizeProgram returns words with the program name normalized
// (programName) and, for a subcommand tool, the global options in front of
// the subcommand dropped. It does not strip runners: a "sudo rm" rule keeps
// meaning sudo rm.
func normalizeProgram(words []string) []string {
	if len(words) == 0 {
		return nil
	}
	name := programName(words[0])
	rest := words[1:]
	if subcommandTools[name] {
		rest = skipGlobalOptions(rest, globalArgOptions[name])
	}
	out := make([]string, 0, 1+len(rest))
	return append(append(out, name), rest...)
}

// skipGlobalOptions drops the leading option words of a subcommand tool's
// arguments; argOpts lists the options whose value is a separate word
// ("-C dir"). "--opt=value" is one word either way. An unknown option taking
// a separate value leaves that value in place, so the match is missed (the
// rule then behaves as before), never widened to another subcommand.
func skipGlobalOptions(args []string, argOpts map[string]bool) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		opt := args[0]
		args = args[1:]
		if opt == "--" {
			break
		}
		if argOpts[opt] && len(args) > 0 {
			args = args[1:]
		}
	}
	return args
}

// globalArgOptions lists, per subcommand tool, the global options before the
// subcommand whose value is a separate word.
var globalArgOptions = map[string]map[string]bool{
	"git": {"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
		"--super-prefix": true, "--config-env": true},
	"docker": {"-H": true, "--host": true, "--context": true, "-c": true, "--config": true,
		"-l": true, "--log-level": true, "--tlscacert": true, "--tlscert": true, "--tlskey": true},
	"podman": {"--connection": true, "-c": true, "--url": true, "--root": true, "--runroot": true,
		"--log-level": true, "--identity": true},
	"kubectl": {"-n": true, "--namespace": true, "--context": true, "--kubeconfig": true,
		"-s": true, "--server": true, "--cluster": true, "--user": true, "--token": true,
		"--as": true, "--as-group": true, "-v": true, "--request-timeout": true,
		"--certificate-authority": true, "--client-certificate": true, "--client-key": true},
	"helm":      {"-n": true, "--namespace": true, "--kube-context": true, "--kubeconfig": true},
	"go":        {"-C": true},
	"cargo":     {"-C": true, "--config": true, "-Z": true},
	"npm":       {"--prefix": true, "-w": true, "--workspace": true},
	"pnpm":      {"-C": true, "--dir": true, "--filter": true, "-F": true},
	"yarn":      {"--cwd": true},
	"gh":        {"-R": true, "--repo": true},
	"systemctl": {"-H": true, "--host": true, "-M": true, "--machine": true},
	"aws":       {"--profile": true, "--region": true, "--output": true, "--endpoint-url": true},
	"az":        {"--subscription": true, "--output": true, "-o": true},
	"gcloud":    {"--project": true, "--account": true, "--configuration": true},
}

func inputCommand(input map[string]any) string {
	s, _ := input["command"].(string)
	return s
}
