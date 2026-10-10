package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// -p prints the answer alone on stdout; the engine's own lines (a tool the
// run could not approve, compaction, a stall) go to stderr. A script
// reading the answer used to get them mixed in, or not at all.
func TestE2E_PrintModeKeepsStdoutForTheAnswer(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"mkdir p_out"}`}}},
		fakeStep{Content: "final answer"},
	)
	e2eHome(t, model)
	resetE2EGlobals()
	t.Cleanup(resetE2EGlobals)
	app, err := bootstrapApp(false, "", false)
	if err != nil {
		t.Fatal(err)
	}
	app.eng.SetAutoExtract(false)

	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code := runPrintModeSession(app.eng, "", "建一个目录", false, nil, app.cfg, app.mcpPool)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	stdout, _ := io.ReadAll(outR)
	stderr, _ := io.ReadAll(errR)

	if code != 0 {
		t.Fatalf("exit code %d; stderr:\n%s", code, stderr)
	}
	if got := strings.TrimSpace(string(stdout)); got != "final answer" {
		t.Fatalf("stdout = %q, want the answer alone; stderr:\n%s", got, stderr)
	}
	if !strings.Contains(string(stderr), "mkdir p_out") {
		t.Fatalf("stderr lacks the engine's tool line:\n%s", stderr)
	}
	if strings.Contains(string(stdout), "⚙") || strings.Contains(string(stdout), "mkdir") {
		t.Fatalf("engine lines leaked into stdout: %q", stdout)
	}
}
