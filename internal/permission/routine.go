package permission

import (
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/safety"
)

// Command groups: one "always allow" answer covers a tool's everyday
// subcommands instead of one prefix per subcommand, so the add → commit →
// push cycle, or new → build → test, is one question. Each group names the
// subcommands that are routine and the options that take a command out of
// the group: anything that forces, deletes, publishes, installs system-wide,
// runs another program or opens an editor keeps asking. Read-only commands
// are not in any group; they run unasked.
const (
	// GroupGitRoutine: staging, committing, pushing, pulling and fetching,
	// switching branches, merging and rebasing, stashes and tags. Not in it:
	// forced pushes ("+refspec" included), deleting remote branches or tags,
	// reset, clean, checkout (its path form discards working-tree changes and
	// a branch name cannot be told from a path), restore, stash drop/clear,
	// interactive rebase, a commit or annotated tag without a message.
	GroupGitRoutine = "git"
	// GroupDotnetRoutine: new, sln, add, restore, build, test, run, format,
	// clean, pack. Not in it: publish, tool, nuget, watch, --force.
	GroupDotnetRoutine = "dotnet"
	// GroupNpmRoutine (npm, pnpm, yarn): install, ci, run, test, init,
	// uninstall, build, ls, outdated, audit. Not in it: publish, login,
	// exec/npx, global installs.
	GroupNpmRoutine = "npm"
	// GroupGoRoutine: build, test, vet, run, fmt, mod, get, generate, list,
	// doc. Not in it: install, clean, env -w, tool, work.
	GroupGoRoutine = "go"
	// GroupCargoRoutine: build, test, check, run, fmt, clippy, add, update,
	// doc, bench. Not in it: install, publish, clean, yank.
	GroupCargoRoutine = "cargo"
)

// routineGroup describes one command group.
type routineGroup struct {
	programs    []string        // program names the group applies to
	subcommands map[string]bool // routine subcommands
	// refusedLong lists, per subcommand ("*" = every one), the long options
	// that take a command out of the group; a given option is matched as a
	// prefix of these names (git and .NET accept unambiguous abbreviations).
	refusedLong map[string][]string
	// refusedShort lists the single-letter options that do the same; letters
	// are checked inside grouped flags too ("-fu").
	refusedShort map[string]string
	// split separates the subcommand from the rest; nil means the first
	// word, which must not be an option (firstWordSubcommand).
	split func(args []string) (sub string, rest []string, ok bool)
	// check adds per-group rules on the parsed invocation.
	check func(sub string, positional []string, shortFlags string, hasLong func(string) bool) bool
	// checkArgs adds per-group rules on the words after the subcommand as
	// written, for options whose value decides (npm --location global).
	checkArgs func(sub string, rest []string) bool
	label     string
}

var routineGroups = map[string]*routineGroup{
	GroupGitRoutine: {
		programs: []string{"git"},
		subcommands: map[string]bool{
			"init": true, "add": true, "commit": true, "push": true, "pull": true, "fetch": true,
			"switch": true, "merge": true, "rebase": true,
			"stash": true, "branch": true, "tag": true, "cherry-pick": true, "mv": true,
		},
		refusedLong: map[string][]string{
			"*":      {"--interactive", "--patch", "--edit", "--force", "--receive-pack", "--upload-pack", "--exec", "--edit-todo"},
			"push":   {"--force-with-lease", "--force-if-includes", "--delete", "--mirror", "--prune"},
			"tag":    {"--delete"},
			"switch": {"--discard-changes"},
		},
		refusedShort: map[string]string{
			"*": "ipe", "push": "d", "branch": "DMC", "switch": "C", "tag": "d", "rebase": "x",
			"commit": "c", // -c <commit> reuses a message and opens the editor
		},
		split: gitSubcommand,
		check: gitRoutineCheck,
		label: "git 常规操作（add/commit/push/pull/switch 等，不含 --force、reset、clean、checkout）",
	},
	GroupDotnetRoutine: {
		programs: []string{"dotnet"},
		subcommands: map[string]bool{
			"new": true, "sln": true, "add": true, "remove": true, "restore": true, "build": true,
			"test": true, "run": true, "format": true, "clean": true, "pack": true, "list": true,
		},
		refusedLong:  map[string][]string{"*": {"--force"}},
		refusedShort: map[string]string{},
		label:        "dotnet 常规操作（new/sln/add/restore/build/test/run 等，不含 publish、tool、--force）",
	},
	GroupNpmRoutine: {
		programs: []string{"npm", "pnpm", "yarn"},
		subcommands: map[string]bool{
			"install": true, "i": true, "ci": true, "run": true, "test": true, "init": true,
			"uninstall": true, "remove": true, "build": true, "ls": true, "list": true, "outdated": true, "audit": true,
		},
		// --prefix installs into another tree ("npm install --prefix
		// /usr/local"); --location global|user is npm 9's spelling of -g
		// (npmCheckArgs).
		refusedLong:  map[string][]string{"*": {"--force", "--global", "--prefix"}},
		refusedShort: map[string]string{"*": "g"},
		checkArgs:    npmCheckArgs,
		label:        "npm/pnpm/yarn 常规操作（install/run/test/build 等，不含 publish、全局安装、exec）",
	},
	GroupGoRoutine: {
		programs: []string{"go"},
		subcommands: map[string]bool{
			"build": true, "test": true, "vet": true, "run": true, "fmt": true, "mod": true,
			"get": true, "generate": true, "list": true, "doc": true, "version": true,
		},
		refusedLong:  map[string][]string{},
		refusedShort: map[string]string{},
		checkArgs:    func(_ string, rest []string) bool { return !goRunsOtherProgram(rest) },
		label:        "go 常规操作（build/test/vet/run/mod/get 等，不含 install、clean、env -w、-exec）",
	},
	GroupCargoRoutine: {
		programs: []string{"cargo"},
		subcommands: map[string]bool{
			"build": true, "test": true, "check": true, "run": true, "fmt": true, "clippy": true,
			"add": true, "update": true, "doc": true, "bench": true, "tree": true,
		},
		refusedLong:  map[string][]string{"*": {"--force"}},
		refusedShort: map[string]string{},
		label:        "cargo 常规操作（build/test/check/run/fmt/add 等，不含 install、publish、clean）",
	},
}

// KnownGroup reports whether name is a command group a rule may refer to.
func KnownGroup(name string) bool { _, ok := routineGroups[name]; return ok }

// GroupLabel names a group for the person, e.g. in the prompt's 记住范围.
func GroupLabel(name string) string {
	if g := routineGroups[name]; g != nil {
		return g.label
	}
	return name + " 常规操作"
}

// groupForProgram returns the group whose programs include exe (spelled
// plainly), or "".
func groupForProgram(exe string) string {
	name := programName(exe)
	for id, g := range routineGroups {
		for _, p := range g.programs {
			if p == name {
				return id
			}
		}
	}
	return ""
}

// minLongAbbrev is the shortest long-option spelling that counts as an
// abbreviation of a refused option: "--f" is too short to be anyone's intent,
// "--forc" is --force.
const minLongAbbrev = len("--xx")

// longRefused reports whether the long option name (without "=value") is
// one of, or an abbreviation of, the group's refused options for sub or for
// every subcommand, or a longer name that starts with one. Only the
// abbreviation direction used to be checked, so "git switch --force-create"
// (and its abbreviation "--force-c") passed as routine although it resets an
// existing branch: every --force-something, --delete-something ... is now
// taken out of the group as well.
func (g *routineGroup) longRefused(sub, name string) bool {
	if len(name) < minLongAbbrev {
		return false
	}
	for _, list := range [][]string{g.refusedLong["*"], g.refusedLong[sub]} {
		for _, r := range list {
			if strings.HasPrefix(r, name) || strings.HasPrefix(name, r) {
				return true
			}
		}
	}
	return false
}

// npmCheckArgs refuses npm 9's --location global|user, the successor of
// -g/--global, in every spelling: "--location=global", "--location global"
// and nopt's abbreviations ("--loc=global"). Only --global and -g used to be
// refused, so "npm install --location=global pkg" was a routine install.
// Only the project location stays in the group.
func npmCheckArgs(_ string, rest []string) bool {
	for i, a := range rest {
		if a == "--" {
			break
		}
		name := optionName(a)
		if len(name) < minLongAbbrev || !strings.HasPrefix("--location", name) {
			continue
		}
		value := optionValue(a)
		if !strings.Contains(a, "=") {
			if i+1 >= len(rest) {
				return false
			}
			value = rest[i+1]
		}
		if value != "project" {
			return false
		}
	}
	return true
}

// firstWordSubcommand is the default split: the subcommand must be the first
// word. It used to be the first word that is not an option, so a global
// option's value became the subcommand — "npm --prefix test publish" read as
// npm test, "go -C build env -w X=y" as go build — and the group ran a
// publish or an env -w unasked. Which global options take a value differs per
// tool and version, so any option in front of the subcommand takes the
// invocation out of the group (it asks) instead of being guessed at.
func firstWordSubcommand(args []string) (string, []string, bool) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, false
	}
	return args[0], args[1:], true
}

// routine reports whether an invocation of one of the group's programs (its
// arguments, without the program) is a routine write.
func (g *routineGroup) routine(args []string) bool {
	split := g.split
	if split == nil {
		split = firstWordSubcommand
	}
	sub, rest, ok := split(args)
	if !ok || !g.subcommands[sub] {
		return false
	}
	refusedShort := g.refusedShort["*"] + g.refusedShort[sub]
	var positional []string
	var shortFlags string
	var longNames []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			positional = append(positional, rest[i+1:]...)
			i = len(rest)
		case strings.HasPrefix(a, "--"):
			name := optionName(a)
			if g.longRefused(sub, name) {
				return false
			}
			longNames = append(longNames, a)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.ContainsAny(a[1:], refusedShort) {
				return false
			}
			shortFlags += a[1:]
		default:
			positional = append(positional, a)
		}
	}
	hasLong := func(want string) bool {
		for _, a := range longNames {
			n := optionName(a)
			if len(n) >= minLongAbbrev && strings.HasPrefix(want, n) {
				return true
			}
		}
		return false
	}
	if g.check != nil && !g.check(sub, positional, shortFlags, hasLongWithValues(longNames, hasLong)) {
		return false
	}
	if g.checkArgs != nil && !g.checkArgs(sub, rest) {
		return false
	}
	return true
}

// hasLongWithValues wraps hasLong so a check can also ask about "--opt=value"
// forms through the same function ("--rebase=interactive").
func hasLongWithValues(longNames []string, hasLong func(string) bool) func(string) bool {
	return func(want string) bool {
		if strings.Contains(want, "=") {
			wantName, wantValue := optionName(want), optionValue(want)
			for _, a := range longNames {
				n := optionName(a)
				if len(n) >= minLongAbbrev && strings.HasPrefix(wantName, n) && optionValue(a) == wantValue {
					return true
				}
			}
			return false
		}
		return hasLong(want)
	}
}

// gitRoutineCheck holds git's rules beyond the option tables.
func gitRoutineCheck(sub string, positional []string, shortFlags string, hasLong func(string) bool) bool {
	if sub != "add" && strings.ContainsRune(shortFlags, 'f') {
		// git add -f only includes ignored files; every other -f forces
		// something over what is there.
		return false
	}
	switch sub {
	case "push":
		for _, p := range positional {
			// "git push origin :branch" deletes the remote branch; a "+"
			// refspec is a forced push.
			if strings.HasPrefix(p, ":") || strings.HasPrefix(p, "+") {
				return false
			}
		}
	case "pull":
		if hasLong("--rebase=interactive") || hasLong("--rebase=i") {
			return false
		}
	case "stash":
		if len(positional) > 0 && (positional[0] == "drop" || positional[0] == "clear") {
			return false
		}
	case "commit":
		// Without a message source git opens the editor, which hangs a
		// non-interactive shell: -m/-F/-C, --message/--file/--reuse-message,
		// --no-edit (amend) or --allow-empty-message.
		if !strings.ContainsAny(shortFlags, "mFC") && !hasLong("--message") && !hasLong("--file") &&
			!hasLong("--reuse-message") && !hasLong("--no-edit") && !hasLong("--allow-empty-message") {
			return false
		}
	case "tag":
		// An annotated or signed tag without a message opens the editor.
		if (strings.ContainsAny(shortFlags, "asu") || hasLong("--annotate") || hasLong("--sign") || hasLong("--local-user")) &&
			!strings.ContainsAny(shortFlags, "mF") && !hasLong("--message") && !hasLong("--file") {
			return false
		}
	}
	return true
}

// optionName is a long option without its "=value" part.
func optionName(a string) string {
	if i := strings.IndexByte(a, '='); i > 0 {
		return a[:i]
	}
	return a
}

// optionValue is the "=value" part of a long option, or "".
func optionValue(a string) string {
	if i := strings.IndexByte(a, '='); i > 0 {
		return a[i+1:]
	}
	return ""
}

// plainProgram reports whether exe is spelled plainly: no path, no
// variable, no runner in front (the allow reading, like an allow prefix).
func plainProgram(exe string) bool {
	return !strings.ContainsAny(exe, ` /\$*?[=`) && !strings.HasPrefix(exe, "-")
}

// groupCovers is the allow reading of a group rule for one simple command
// given as its words as written.
func groupCovers(group string, words []string) bool {
	g := routineGroups[group]
	if g == nil || len(words) == 0 || !plainProgram(words[0]) || groupForProgram(words[0]) != group {
		return false
	}
	return g.routine(words[1:])
}

// routineGroupOf returns the group that covers words as a routine write, or
// "" (the program is in no group, or the invocation is not routine).
func routineGroupOf(words []string) string {
	if len(words) == 0 || !plainProgram(words[0]) {
		return ""
	}
	if id := groupForProgram(words[0]); id != "" && routineGroups[id].routine(words[1:]) {
		return id
	}
	return ""
}

// groupMatchesAny is the deny/ask reading of a group rule: some command of
// the line, seen through runners, paths, letter case and bash's backslash
// reading (anyReading), runs one of the group's programs in a way that is not
// read-only. Global options in front of the subcommand ("npm --prefix sub
// install", "git -c x push") never make an invocation read-only, so they
// only widen the rule.
//
// Every invocation of the group's programs that is not read-only matches,
// routine or not. The deny/ask reading used to be the routine test alone, so it only
// fired on the routine writes: with "ask for git" and an allow prefix "git
// push", "git push origin main" asked and "git push --force" ran unasked,
// and "deny git" only asked for "git reset --hard". What the group leaves
// out as too risky must be caught at least as strongly as what it covers;
// only the read-only commands (git status, npm view), less risky than the
// routine writes, stay outside.
//
// The commands a line runs indirectly count too (see denyReading): "bash -c
// 'git push'", "xargs git push" and "find -exec git push" used to pass a
// deny on the git group. A nested command whose program is a variable
// matches every group.
func groupMatchesAny(group, command string) bool {
	g := routineGroups[group]
	if g == nil {
		return false
	}
	cmds, opaque := denyReading(command)
	if opaque {
		return true
	}
	for _, c := range cmds {
		for _, words := range [][]string{c.Words, safety.StripCommandRunners(c.Words)} {
			if len(words) == 0 || groupForProgram(words[0]) != group {
				continue
			}
			args := words[1:]
			name := programName(words[0])
			// Rated as spelled plainly, so "/usr/bin/git status" is the
			// read-only git status and "git.exe push -f" a forced push. The
			// read-only test comes first: the listing forms of routine
			// subcommands (git branch -a, git tag, git stash list) passed the
			// routine test, so an ask rule on the git group asked for them in
			// auto mode, against "all non-read-only usages" above.
			if groupClassifier.classifyWords(append([]string{name}, args...)) == CatSafe {
				continue
			}
			return true
		}
	}
	return false
}

// groupClassifier rates invocations for groupMatchesAny.
var groupClassifier = NewClassifier()
