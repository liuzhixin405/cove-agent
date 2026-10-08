package repl

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestAskWithExternalSerializedAndLocalPreserved(t *testing.T) {
	defer captureStdout(t)()
	defer ClearPermInputCh()
	firstAnswers := make(chan ExternalAnswer, 2)
	firstRegistered := make(chan struct{})
	secondRegistered := make(chan struct{})
	var firstCleaned atomic.Bool
	type response struct {
		answer string
		ok     bool
	}
	firstDone, secondDone := make(chan response, 1), make(chan response, 1)
	go func() {
		answer, ok := AskWith(AskSpec{Timeout: 3 * time.Second, External: func() (<-chan ExternalAnswer, func()) {
			close(firstRegistered)
			return firstAnswers, func() { firstCleaned.Store(true) }
		}})
		firstDone <- response{answer, ok}
	}()
	<-firstRegistered
	go func() {
		answer, ok := AskWith(AskSpec{Timeout: 3 * time.Second, External: func() (<-chan ExternalAnswer, func()) {
			if !firstCleaned.Load() {
				t.Error("next external prompt registered before previous cleanup")
			}
			close(secondRegistered)
			return nil, nil
		}})
		secondDone <- response{answer, ok}
	}()
	select {
	case <-secondRegistered:
		t.Fatal("parallel prompt replaced the current local relay")
	case <-time.After(20 * time.Millisecond):
	}
	firstAnswers <- ExternalAnswer{Answer: "y", Validate: func() bool { return false }}
	select {
	case got := <-firstDone:
		if got.ok {
			t.Fatal("invalid external authorization accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("external answer did not release prompt")
	}
	<-secondRegistered
	firstAnswers <- ExternalAnswer{Answer: "y"}
	local, state, _ := TakePromptInputFor("n")
	if state != PromptAnswer {
		t.Fatal("external registration replaced the local AskWith relay")
	}
	local <- "n"
	select {
	case got := <-secondDone:
		if !got.ok || got.answer != "n" {
			t.Fatalf("second prompt=%+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("local answer did not release second prompt")
	}
}

func TestAskWithLocalCancelDuringExternalValidation(t *testing.T) {
	defer captureStdout(t)()
	defer ClearPermInputCh()
	external := make(chan ExternalAnswer, 1)
	validating, release := make(chan struct{}), make(chan struct{})
	done := make(chan string, 1)
	external <- ExternalAnswer{Answer: "y", Validate: func() bool {
		close(validating)
		<-release
		return true
	}}
	go func() {
		answer, ok := AskWith(AskSpec{Timeout: time.Second, External: func() (<-chan ExternalAnswer, func()) {
			return external, nil
		}})
		if !ok {
			t.Error("local cancellation was not delivered")
		}
		done <- answer
	}()
	<-validating
	local := TakePermInputCh()
	if local == nil {
		t.Fatal("local prompt relay disappeared during external validation")
	}
	local <- "cancel"
	close(release)
	select {
	case answer := <-done:
		if answer != "cancel" {
			t.Fatalf("answer=%q; external approval overrode local cancellation", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("validation did not finish")
	}
}

// A prompt registers what counts as an answer. A line that is one is handed
// to the prompt; any other line leaves the prompt waiting and is the
// caller's to dispatch, with the prompt's hint to show. Type-ahead for the
// next task used to be swallowed as a "no".
func TestTakePromptInputForDistinguishesAnswers(t *testing.T) {
	t.Cleanup(ClearPermInputCh)
	ch := make(chan string, 1)
	yesNo := func(s string) bool { return s == "y" || s == "n" }
	SetPromptInput(ch, yesNo, "请输入 y 或 n")

	got, state, hint := TakePromptInputFor("请把测试也改掉")
	if state != PromptNotAnswer || got != nil || hint != "请输入 y 或 n" {
		t.Fatalf("non-answer: ch=%v state=%v hint=%q", got, state, hint)
	}
	if _, state, _ = TakePromptInputFor(""); state != PromptNotAnswer {
		t.Fatalf("empty line: state=%v, want not-answer", state)
	}
	got, state, _ = TakePromptInputFor("y")
	if state != PromptAnswer || got == nil {
		t.Fatalf("answer: ch=%v state=%v", got, state)
	}
	if _, state, _ = TakePromptInputFor("y"); state != PromptNone {
		t.Fatalf("after the answer the prompt must be gone: %v", state)
	}

	// A prompt that takes any text (the question tool) still refuses an
	// empty line, and Ctrl+C's TakePermInputCh takes the channel whatever
	// the line would have been.
	SetPromptInput(ch, nil, "请输入回答")
	if _, state, _ := TakePromptInputFor(""); state != PromptNotAnswer {
		t.Fatalf("empty line for a free-text prompt: %v", state)
	}
	if _, state, _ := TakePromptInputFor("anything"); state != PromptAnswer {
		t.Fatalf("free text not accepted: %v", state)
	}
	SetPromptInput(ch, yesNo, "")
	if TakePermInputCh() == nil {
		t.Fatal("TakePermInputCh must take the channel unconditionally")
	}
}

// ⚡ (U+26A1) is East Asian Wide: measured as one cell the running prompt
// was one column short and the row could touch the last column.
func TestLightningIsTwoCellsWide(t *testing.T) {
	if w := runeCellWidth('⚡'); w != 2 {
		t.Fatalf("width(⚡) = %d, want 2", w)
	}
}
