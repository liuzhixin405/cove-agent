package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/mcp"
)

func registeredNames(goos string) map[string]bool {
	names := map[string]bool{}
	for _, t := range registerToolsFor(mcp.NewPool(), goos).All() {
		names[t.Def().Name] = true
	}
	return names
}

// lsp had no runner wired in and cron never fired: both only cost prompt
// tokens and invited calls that could not work.
func TestRegistryOmitsDeadTools(t *testing.T) {
	for _, goos := range []string{"windows", "linux", "darwin"} {
		names := registeredNames(goos)
		for _, dead := range []string{"lsp", "cron"} {
			if names[dead] {
				t.Errorf("%s: %q is still registered", goos, dead)
			}
		}
		for _, core := range []string{"bash", "read", "write", "edit", "grep", "glob", "todowrite", "task", "agent"} {
			if !names[core] {
				t.Errorf("%s: core tool %q missing", goos, core)
			}
		}
	}
}

func TestRegistryPowerShellOnlyOnWindows(t *testing.T) {
	if !registeredNames("windows")["powershell"] {
		t.Error("powershell missing on windows")
	}
	for _, goos := range []string{"linux", "darwin"} {
		if registeredNames(goos)["powershell"] {
			t.Errorf("powershell registered on %s, where it can only fail", goos)
		}
	}
}

func toolSchema(t *testing.T, name string) string {
	t.Helper()
	tl, ok := registerToolsFor(mcp.NewPool(), "windows").Find(name)
	if !ok {
		t.Fatalf("tool %q not registered", name)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, tl.Def().InputSchema); err != nil {
		t.Fatalf("%s schema is not valid JSON: %v", name, err)
	}
	return buf.String()
}

// Closed value sets are enums, so the model picks a value the tool handles.
func TestToolSchemasUseEnums(t *testing.T) {
	cases := map[string][]string{
		"todowrite":   {`"enum":["pending","in_progress","completed","cancelled"]`, `"enum":["high","medium","low"]`},
		"task_update": {`"enum":["pending","running","completed","failed","cancelled"]`},
		"agent":       {`"enum":["general","explore","plan","review","test","verify"]`},
	}
	for name, wants := range cases {
		schema := toolSchema(t, name)
		for _, want := range wants {
			if !strings.Contains(schema, want) {
				t.Errorf("%s schema lacks %s: %s", name, want, schema)
			}
		}
	}
}

// task only records a to-do item; the old "runs independently" wording made
// models wait for results that never came.
func TestTaskDescriptionSaysItDoesNotRun(t *testing.T) {
	tl, _ := registerToolsFor(mcp.NewPool(), "windows").Find("task")
	if d := tl.Def().Description; !strings.Contains(d, "不会执行") {
		t.Fatalf("task description = %q, want it to say the task is not executed", d)
	}
}
