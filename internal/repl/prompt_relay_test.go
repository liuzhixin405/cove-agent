package repl

import (
	"strings"
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

// A prompt that allows an empty answer takes Enter on an empty line as "".
func TestTakePromptInputForAllowsEmptyWhenAsked(t *testing.T) {
	ch := make(chan string, 1)
	SetPromptInput(ch, nil, "hint")
	consoleMu.Lock()
	permAllowEmpty = true
	consoleMu.Unlock()
	got, state, _ := TakePromptInputFor("   ")
	if state != PromptAnswer || got == nil {
		t.Fatalf("empty line with AllowEmpty: state = %v", state)
	}
	// Without the flag an empty line is still not an answer.
	SetPromptInput(ch, nil, "hint")
	if _, state, hint := TakePromptInputFor(""); state != PromptNotAnswer || hint != "hint" {
		t.Fatalf("empty line without AllowEmpty: state = %v hint = %q", state, hint)
	}
	ClearPermInputCh()
}

// A front end that cannot block (a slash command on the input loop) arms a
// prompt and reads the next line itself: the keys still answer on their own
// and the status row says a confirmation is waiting, until it is disarmed.
func TestArmPromptOffersKeysUntilDisarmed(t *testing.T) {
	token, ok := ArmPrompt("等待确认", []string{"确认: x"}, "y 确认 / n 取消", "yn")
	if !ok {
		t.Fatal("arming on an idle relay must succeed")
	}
	if !promptKeyAnswers('y') || !promptKeyAnswers('N') || promptKeyAnswers('a') {
		t.Fatal("armed keys must answer on their own")
	}
	lr := typed("")
	lr.interactionState = "空闲"
	consoleMu.Lock()
	status := lr.statusLineLocked()
	consoleMu.Unlock()
	if !strings.Contains(status, "等待你确认") {
		t.Fatalf("status = %q", status)
	}
	DisarmPrompt(token)
	if promptKeyAnswers('y') {
		t.Fatal("keys must stop answering once disarmed")
	}
	if ch := TakePermInputCh(); ch != nil {
		t.Fatal("nothing may stay registered after DisarmPrompt")
	}
}

// Arming refuses while another prompt waits (a permission prompt must not be
// overwritten), and disarming only clears the prompt that was armed.
func TestArmPromptRefusesWhenAPromptWaitsAndDisarmsOnlyItself(t *testing.T) {
	ch := make(chan string, 1)
	SetPromptInput(ch, nil, "perm")
	if _, ok := ArmPrompt("等待确认", nil, "h", "yn"); ok {
		t.Fatal("must not arm over a waiting prompt")
	}
	if got := TakePermInputCh(); got != ch {
		t.Fatal("the waiting prompt's channel must survive a refused arm")
	}
	token, ok := ArmPrompt("等待确认", nil, "h", "yn")
	if !ok || token == 0 {
		t.Fatal("arming on an idle relay must succeed")
	}
	// A permission prompt registered afterwards owns the relay now.
	SetPromptInput(ch, nil, "perm")
	DisarmPrompt(token)
	if got := TakePermInputCh(); got != ch {
		t.Fatal("disarming a stale token must not clear another prompt")
	}
}
