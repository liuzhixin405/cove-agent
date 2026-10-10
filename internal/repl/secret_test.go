package repl

import (
	"errors"
	"strings"
	"testing"
)

func TestReadSecretMasksAndKeepsOutOfHistory(t *testing.T) {
	restore := captureStdout(t)
	lr := typedKeyByKey("sk-abc\x7fd\r")
	got, err := lr.readSecretRaw("key: ")
	out := restore()
	if err != nil || got != "sk-abd" {
		t.Fatalf("got %q, %v", got, err)
	}
	if strings.Contains(out, "sk-ab") {
		t.Fatalf("secret echoed: %q", out)
	}
	if strings.Count(out, "•") < 6 {
		t.Fatalf("mask glyphs missing: %q", out)
	}
	if len(lr.history) != 0 {
		t.Fatal("secret must not enter the history")
	}
}

func TestReadSecretInterrupt(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	if _, err := typedKeyByKey("ab\x03").readSecretRaw("key: "); !errors.Is(err, ErrInterrupt) {
		t.Fatalf("err = %v", err)
	}
}

// Output printed while a secret is being typed is held and shown after it,
// so a background notice does not land in the middle of the masked prompt.
func TestReadSecretHoldsOutputUntilDone(t *testing.T) {
	restore := captureStdout(t)
	beginSecretHold()
	PrintAbove("◆ 竞跑完成\n")
	consoleMu.Lock()
	held := len(secretHeld)
	consoleMu.Unlock()
	endSecretHold()
	out := restore()
	if held != 1 {
		t.Fatalf("output must be held while the secret is typed, held=%d", held)
	}
	if !strings.Contains(out, "竞跑完成") {
		t.Fatalf("held output must be printed afterwards: %q", out)
	}
}

// A fallback read for the secret must not leave the editor in fallback mode.
func TestReadSecretFallbackDoesNotPinFallbackReader(t *testing.T) {
	lr := New(nil)
	lr.fallbackReader = nil
	lr.restoreFallbackReaderAfterSecret(nil)
	if lr.fallbackReader != nil {
		t.Fatal("a nil fallback reader before the secret must be nil after it")
	}
}
