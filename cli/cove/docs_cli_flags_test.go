package main

import (
	"bytes"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/termui"
)

// manualFlags returns the flags in the first column of USER_MANUAL's 启动参数
// table.
func manualFlags(t *testing.T) map[string]bool {
	t.Helper()
	manual := readTextFile(t, filepath.Join(repoRootFromThisFile(t), "docs", "USER_MANUAL.md"))
	start := strings.Index(manual, "### 启动参数")
	if start < 0 {
		t.Fatal("USER_MANUAL.md has no 启动参数 section")
	}
	section := manual[start:]
	if end := strings.Index(section[len("### 启动参数"):], "\n#"); end >= 0 {
		section = section[:len("### 启动参数")+end]
	}
	flagRE := regexp.MustCompile(`--?[a-z][a-z-]*`)
	flags := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		cells := strings.Split(strings.TrimSpace(line), "|")
		if len(cells) < 3 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "`-") {
			continue
		}
		for _, f := range flagRE.FindAllString(cells[1], -1) {
			flags[f] = true
		}
	}
	return flags
}

// The 启动参数 table listed -r as working when it did nothing, and left out
// --no-tui and --profile. Keep the table, the parser and
// --help in step.
func TestManualFlagTableMatchesParser(t *testing.T) {
	documented := manualFlags(t)
	var missing, unknown []string
	for f := range cliFlags {
		if !documented[f] {
			missing = append(missing, f)
		}
	}
	for f := range documented {
		if !cliFlags[f] {
			unknown = append(unknown, f)
		}
	}
	sort.Strings(missing)
	sort.Strings(unknown)
	if len(missing) > 0 {
		t.Errorf("USER_MANUAL.md 启动参数 table lacks: %s", strings.Join(missing, ", "))
	}
	if len(unknown) > 0 {
		t.Errorf("USER_MANUAL.md 启动参数 table documents flags cove does not parse: %s", strings.Join(unknown, ", "))
	}
}

func TestCLIHelpListsEveryFlag(t *testing.T) {
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	t.Cleanup(func() { termui.SetWriter(nil) })
	printCLIHelp()
	help := buf.String()
	var missing []string
	for f := range cliFlags {
		if !regexp.MustCompile(`(^|[\s,])` + regexp.QuoteMeta(f) + `($|[\s,\[<])`).MatchString(help) {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("cove --help does not mention: %s", strings.Join(missing, ", "))
	}
}
