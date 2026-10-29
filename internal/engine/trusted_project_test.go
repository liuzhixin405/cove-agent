package engine

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/permission"
)

// Trusting a project is a one-time approval for routine work in it: writes
// inside the project root and recognised read-only/build commands stop asking
// even in default mode, while installs, git publishing, unknown commands and
// writes outside the project still go through the normal prompt.
func TestTrustedProjectSkipsRoutineApprovals(t *testing.T) {
	isolateHome(t)
	bash := &permShellTool{name: "bash"}
	w := &mockTool{name: "write", result: "ok"}
	eng, p := newPermEngine(t, permission.Default, bash, w)
	writeFile := func(path string) {
		t.Helper()
		eng.executeTool(context.Background(), api.ToolCall{ID: "w-" + path, Name: "write", Input: map[string]any{"filePath": path}})
	}

	// Untrusted project: an in-project write asks, as it always has.
	writeFile("a.go")
	if p.count() != 1 {
		t.Fatalf("untrusted project write prompted %d times, want 1: %v", p.count(), p.asked)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.TrustProjectDir(cwd); err != nil {
		t.Fatalf("trust project dir: %v", err)
	}

	// Trusted: the same writes and the routine lines run unasked.
	writeFile("a.go")
	writeFile("sub/b.go")
	for _, cmd := range []string{"git status", "go build ./...", "go test ./..."} {
		if out := runShell(t, eng, "bash", cmd); strings.HasPrefix(out, "Error") {
			t.Fatalf("%q: %s", cmd, out)
		}
	}
	if p.count() != 1 {
		t.Fatalf("trusted project prompted for routine work: %v", p.asked)
	}

	// Installs, git publishing and unknown commands still ask.
	for _, cmd := range []string{"npm install x", "git commit -m x", "go run ."} {
		runShell(t, eng, "bash", cmd)
	}
	// Writes outside the project root still ask.
	writeFile(t.TempDir() + "/x.go")
	writeFile("../escape.go")
	if p.count() != 6 {
		t.Fatalf("trusted project prompted %d times for special work, want 6: %v", p.count(), p.asked)
	}

	// A deny rule still wins over the project trust.
	eng.AddPermissionRule(permission.DDeny, permission.Rule{ToolPattern: "bash", CommandPrefix: "git status"})
	if out := runShell(t, eng, "bash", "git status"); !strings.Contains(out, "denied") {
		t.Fatalf("deny rule ignored in trusted project: %s", out)
	}
}
