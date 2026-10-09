package permission

import "testing"

// Review fix round 3: regression tests built from the reviewer's probes.

// A deny or ask group rule used to reuse the allow-side routine test, so it
// only fired on routine invocations: "ask for git" plus an allow prefix
// "git push" asked for "git push origin main" and let "git push --force"
// through. The riskier variant must be caught at least as strongly.
func TestGroupDenyAskCatchNonRoutineVariants(t *testing.T) {
	risky := []string{"git push --force origin main", "git push -f origin main", "git reset --hard", "git -c core.pager=evil log", "sudo git clean -fdx"}
	for _, cmd := range risky {
		m := NewManager(Default)
		m.SetShellKind(ShellPOSIX)
		m.AddRule(DAsk, Rule{ToolPattern: "bash", CommandGroup: GroupGitRoutine})
		m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "git"})
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("ask group + allow prefix: %q = %v, want ask", cmd, d)
		}
		md := NewManager(Default)
		md.SetShellKind(ShellPOSIX)
		md.AddRule(DDeny, Rule{ToolPattern: "bash", CommandGroup: GroupGitRoutine})
		if d, _ := md.Check("bash", map[string]any{"command": cmd}, DAsk); d != DDeny {
			t.Errorf("deny group: %q = %v, want deny", cmd, d)
		}
	}
	md := NewManager(Default)
	md.AddRule(DDeny, Rule{ToolPattern: "bash", CommandGroup: GroupNpmRoutine})
	for _, cmd := range []string{"npm publish", "npm install -g x", "pnpm dlx evil"} {
		if d, _ := md.Check("bash", map[string]any{"command": cmd}, DAsk); d != DDeny {
			t.Errorf("deny npm group: %q = %v, want deny", cmd, d)
		}
	}
	// Read-only invocations are less risky than the routine writes and stay
	// outside the group, as before.
	for _, cmd := range []string{"git status", "git log --oneline -5", "npm view react", "ls"} {
		if d, _ := md.Check("bash", map[string]any{"command": cmd}, DAllow); d != DAllow {
			t.Errorf("deny npm group: %q = %v, want allow", cmd, d)
		}
	}
}

// longRefused only checked whether the typed option abbreviates a refused
// name; "--force-create" (a name that starts with the refused --force) was
// routine and remembered as the git group.
func TestGroupRefusesLongerSpellingsOfRefusedOptions(t *testing.T) {
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandGroup: GroupGitRoutine})
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandGroup: GroupNpmRoutine})
	for _, cmd := range []string{
		"git switch --force-create main origin/main",
		"git switch --force-c main origin/main",
		"git branch --force-anything x",
		"npm install --location=global somepkg",
		"npm install --location global somepkg",
		"npm install --loc=global somepkg",
		"npm install --location=user somepkg",
		"npm install --prefix /usr/local somepkg",
		"npm install --global-style somepkg",
	} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("%q = %v, want ask", cmd, d)
		}
		rules, _ := ShellRememberRules("bash", cmd, ShellPOSIX)
		for _, r := range rules {
			if r.CommandGroup != "" {
				t.Errorf("ShellRememberRules(%q) offered group %q", cmd, r.CommandGroup)
			}
		}
	}
	for _, cmd := range []string{"git switch -c feat/x", "git push origin main", "npm install", "npm install --location=project x", "npm install --save-dev x"} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAllow {
			t.Errorf("%q = %v, want allow", cmd, d)
		}
	}
}

// Read-only spellings that run a program, write a file or contact another
// host.
func TestReadOnlyRefusesProgramRunningAndWritingSpellings(t *testing.T) {
	c := NewClassifier()
	for _, tc := range []struct {
		cmd  string
		kind ShellKind
	}{
		{"ag --pag=x foo", ShellPOSIX},
		{"ag --pa=x foo", ShellPOSIX},
		{"ag --pager x foo", ShellPOSIX},
		{"rg --hostname-bin=x foo", ShellPOSIX},
		{"rg --hostname-bin x foo", ShellPOSIX},
		{`gci -Path:\\host\share`, ShellPowerShell},
		{`gci -Path:'\\host\share'`, ShellPowerShell},
		{`gci FileSystem::\\host\share`, ShellPowerShell},
		{`gci Microsoft.PowerShell.Core\FileSystem::\\host\share`, ShellPowerShell},
		{`gci .,\\host\share`, ShellPowerShell},
		{`gci -Path://host/share`, ShellPowerShell},
		{`gci \\host\share`, ShellPowerShell},
		{"yarn --version", ShellPOSIX},
		{"yarn info react", ShellPOSIX},
		{"yarn why react", ShellPOSIX},
		{"docker compose config -o out.yml", ShellPOSIX},
		{"docker compose config --output=out.yml", ShellPOSIX},
		{"docker compose config -qo out.yml", ShellPOSIX},
		{"file --comp -m x", ShellPOSIX},
		{"file --c -m x", ShellPOSIX},
		{"file -zC -m x", ShellPOSIX},
	} {
		if c.IsReadOnlyLineFor(tc.cmd, tc.kind) || c.AutoApproveLineFor(tc.cmd, tc.kind) {
			t.Errorf("[%s] %q auto-allowed (cat %v)", tc.kind, tc.cmd, c.ClassifyLineFor(tc.cmd, tc.kind))
		}
	}
	for _, tc := range []struct {
		cmd  string
		kind ShellKind
	}{
		{"ag foo", ShellPOSIX},
		{"ag --path-to-ignore x foo", ShellPOSIX},
		{"rg -n foo", ShellPOSIX},
		{"rg --hidden foo", ShellPOSIX},
		{`gci C:\Users`, ShellPowerShell},
		{`gci -Path:C:\Users`, ShellPowerShell},
		{`cat C:\\Users\\x`, ShellPOSIX},
		{`gci FileSystem::C:\Users`, ShellPowerShell},
		{"curl https://example.com", ShellPowerShell},
		{"docker compose config", ShellPOSIX},
		{"docker compose config --services", ShellPOSIX},
		{"file -m x y", ShellPOSIX},
		{"file -b y", ShellPOSIX},
		{"npm view react", ShellPOSIX},
		{"npm --version", ShellPOSIX},
		{"pnpm list", ShellPOSIX},
		{"git -C sub/dir status", ShellPOSIX},
	} {
		if tc.cmd == "curl https://example.com" {
			// A fetch is egress: it asks in auto mode too (fourth round).
			if c.AutoApproveLineFor(tc.cmd, tc.kind) {
				t.Errorf("[%s] %q auto-approved, want asked", tc.kind, tc.cmd)
			}
			continue
		}
		if !c.IsReadOnlyLineFor(tc.cmd, tc.kind) {
			t.Errorf("[%s] %q not read-only (cat %v)", tc.kind, tc.cmd, c.ClassifyLineFor(tc.cmd, tc.kind))
		}
	}
}

// npm/pnpm/yarn test and build scripts were CatInstall (asked every time in
// auto mode) while go test and make test were auto-approved.
func TestPackageScriptsTestBuildAreBuildCategory(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{
		"npm test", "npm t", "npm run test", "npm run build", "npm run lint", "npm run-script build",
		"pnpm test", "pnpm build", "pnpm run build", "yarn test", "yarn build", "yarn run lint",
		"npm test -- --coverage", "npm run build -- --prod",
	} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got != CatBuild {
			t.Errorf("%q = %v, want CatBuild", cmd, got)
		}
		if !c.AutoApproveLineFor(cmd, ShellPOSIX) {
			t.Errorf("%q not auto-approved", cmd)
		}
		if c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("%q read-only", cmd)
		}
	}
	for _, cmd := range []string{
		"npm install", "npm i x", "npm ci", "npm publish", "npm exec x", "pnpm dlx x", "pnpm add x", "yarn add x",
		"npm run deploy", "npm run build --script-shell=evil", "npm test --global", "npm --prefix x test",
		"yarn dlx x", "npm run", "npm start",
	} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got == CatBuild || got == CatSafe {
			t.Errorf("%q = %v, want not build/safe", cmd, got)
		}
	}
}
