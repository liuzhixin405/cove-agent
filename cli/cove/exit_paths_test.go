package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/cost"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

// Only the TUI's /exit and Ctrl+D paths recorded the session's cost, so after
// headless and -p runs /cost's 24h/7d figures stayed empty. finishSession is
// what every exit path now calls; it must not print, because the headless and
// -p paths keep stdout for answers.
func TestFinishSessionRecordsCostQuietly(t *testing.T) {
	eng := newTestEngine(t)
	eng.LoadMessages([]api.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}})
	out := captureOut(t)

	finishSession(eng, nil)

	if out.Len() != 0 {
		t.Fatalf("finishSession printed %q", out.String())
	}
	h := cost.NewCostHistory()
	if len(h.Records) != 1 || h.Records[0].SessionID != eng.SessionID() {
		t.Fatalf("cost history = %+v, want one record for %s", h.Records, eng.SessionID())
	}
	home, _ := os.UserHomeDir()
	sessions, _ := session.ListSessionFiles(filepath.Join(home, ".cove", "sessions"))
	if len(sessions) != 1 || sessions[0] != eng.SessionID()+".jsonl" {
		t.Fatalf("session files = %v, want the one saved session", sessions)
	}
}

func TestFinishSessionSkipsEmptySession(t *testing.T) {
	eng := newTestEngine(t)
	finishSession(eng, nil)
	if h := cost.NewCostHistory(); len(h.Records) != 0 {
		t.Fatalf("an empty session was recorded: %+v", h.Records)
	}
}

// -p refuses slash commands, but only the prompt the user typed can be one: a
// piped log that starts with "/usr/bin/foo: error" is content.
func TestRunPrintModeSlashCheckIgnoresPipedContent(t *testing.T) {
	cases := []struct {
		arg  string
		want bool
	}{
		{"/commit", true},
		{"  /diff", true},
		{"", false},
		{"解释", false},
	}
	for _, c := range cases {
		if got := printPromptIsSlashCommand(c.arg); got != c.want {
			t.Errorf("printPromptIsSlashCommand(%q) = %v, want %v", c.arg, got, c.want)
		}
	}
}

// -p without an API key sent the request anyway and surfaced whatever the
// provider or network said (401, connection refused) instead of the setup
// guidance headless and the TUI print.
func TestPrintModeKeyCheck(t *testing.T) {
	if !runNeedsAPIKey("") {
		t.Fatal("no key, live run: must be refused")
	}
	if runNeedsAPIKey("placeholder") {
		t.Fatal("a key is set: must run")
	}
}

type fakeDisconnector struct{ calls int }

func (f *fakeDisconnector) DisconnectAll() { f.calls++ }

// MCP servers were never disconnected on a normal exit; stdio servers were
// left to die with the process instead of shutting down cleanly.
func TestFinishSessionDisconnectsMCP(t *testing.T) {
	eng := newTestEngine(t)
	pool := &fakeDisconnector{}
	finishSession(eng, pool)
	if pool.calls != 1 {
		t.Fatalf("DisconnectAll called %d times, want 1", pool.calls)
	}
}

func TestAutoSaveSessionStillAnnouncesSave(t *testing.T) {
	eng := newTestEngine(t)
	eng.LoadMessages([]api.Message{{Role: "user", Content: "hi"}})
	out := captureOut(t)
	autoSaveSession(eng)
	if !strings.Contains(out.String(), "会话已自动保存") {
		t.Fatalf("autoSaveSession output = %q", out.String())
	}
}
