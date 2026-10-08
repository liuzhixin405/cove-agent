package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

func TestListMetadataCountsUserTurnsAndToolMessages(t *testing.T) {
	messages := []struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		Synthetic bool   `json:"synthetic,omitempty"`
	}{
		{Role: "user", Content: "请修改代码"},
		{Role: "assistant", Content: "我来处理"},
		{Role: "tool", Content: "read result"},
		{Role: "tool", Content: "test output"},
		{Role: "user", Content: "[system: retry differently]", Synthetic: true},
		{Role: "user", Content: "继续"},
	}

	if got := countGenuineUserTurns(messages); got != 2 {
		t.Fatalf("countGenuineUserTurns = %d, want 2", got)
	}
	if got := countToolMessages(messages); got != 2 {
		t.Fatalf("countToolMessages = %d, want 2", got)
	}
}

// Phrases that engine tests happen to send are ordinary user requests.
func TestTestPhrasesAreGenuineUserTurns(t *testing.T) {
	type msg = struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		Synthetic bool   `json:"synthetic,omitempty"`
	}
	messages := []msg{{Role: "user", Content: "do something about the flaky login test"}}
	if got := firstUserPreview(messages); got == "" {
		t.Error("a user message starting with \"do something\" got no preview")
	}
	if got := countGenuineUserTurns(messages); got != 1 {
		t.Errorf("countGenuineUserTurns = %d, want 1", got)
	}
}

func TestIncrementalSessionMetadataSurvivesReloadAndRewrite(t *testing.T) {
	store := newTestStore(t)
	record := &Record{ID: "incremental", Model: "claude-opus-5"}
	messages := []api.Message{
		{Role: "user", Content: "synthetic", Synthetic: true},
		{Role: "user", Content: "[system: old format]"},
		{Role: "user", Content: " "},
		{Role: "assistant", Content: "response"},
		{Role: "user", Content: "First\r\nrequest"},
		{Role: "tool", Content: "output"},
		{Role: "user", Parts: []api.MessagePart{{Type: "text", Text: "attachment"}}},
		{Role: "user", Content: "next request"},
	}
	for _, message := range messages {
		record.Messages = append(record.Messages, message)
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	check := func(wantTurns, wantCount int, wantPreview string) {
		t.Helper()
		listed, err := store.List()
		if err != nil || len(listed) != 1 {
			t.Fatalf("list = %+v, %v", listed, err)
		}
		if listed[0].UserTurns != wantTurns || listed[0].MessageCount != wantCount || listed[0].Preview != wantPreview {
			t.Fatalf("metadata = turns:%d count:%d preview:%q, want %d/%d/%q", listed[0].UserTurns, listed[0].MessageCount, listed[0].Preview, wantTurns, wantCount, wantPreview)
		}
	}
	check(4, 8, "First request")
	for iteration := 0; iteration < 2; iteration++ {
		var err error
		record, err = store.Load(record.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	record.Messages = append(record.Messages, api.Message{Role: "user", Content: "after reload"})
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	check(5, 9, "First request")
	record.Messages = []api.Message{{Role: "user", Content: "[Conversation Summary] summary"}, {Role: "user", Content: "replacement request"}}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	check(1, 2, "replacement request")
}

func BenchmarkSessionIndexEntry(b *testing.B) {
	path := filepath.Join(b.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	for _, count := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("messages_%d", count), func(b *testing.B) {
			record := &Record{ID: "benchmark", Messages: make([]api.Message, count)}
			for index := range record.Messages {
				record.Messages[index] = api.Message{Role: "tool", Content: "tool output"}
				if index%4 == 0 {
					record.Messages[index] = api.Message{Role: "user", Content: "hello world"}
				}
			}
			var state persistedState
			for _, message := range record.Messages {
				state.noteMessage(message)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				entry := entryFromState(record, "session.jsonl", info, &state)
				if entry.MessageCount != count {
					b.Fatal("incorrect message count")
				}
			}
		})
	}
}
