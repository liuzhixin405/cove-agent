package repl

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"
)

func TestOwnerInputWakeWithoutTyping(t *testing.T) {
	source, writer := io.Pipe()
	defer source.Close()
	defer writer.Close()
	wake := make(chan struct{}, 1)
	polled := make(chan struct{}, 8)
	reader := &LineReader{}
	reader.SetOwnerEventHook(func() <-chan struct{} { return wake }, func() { polled <- struct{}{} })
	input := bufio.NewReader(ownerInput{source: source, lr: reader})
	done := make(chan inputResult, 1)
	go func() {
		line, err := input.ReadString('\n')
		done <- inputResult{data: []byte(line), err: err}
	}()
	wake <- struct{}{}
	select {
	case <-polled:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not wake while stdin was silent")
	}
	select {
	case <-done:
		t.Fatal("wake submitted an input line")
	default:
	}
	if _, err := io.WriteString(writer, "partial"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	select {
	case got := <-done:
		if string(got.data) != "partial" || got.err != io.EOF {
			t.Fatalf("input=%q err=%v", got.data, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EOF did not finish the reader")
	}
}

func TestOwnerInputPreservesPartialEscapeAndPaste(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	source, writer := io.Pipe()
	defer source.Close()
	defer writer.Close()
	wake := make(chan struct{}, 1)
	polled := make(chan struct{}, 8)
	reader := New(nil)
	reader.SetOwnerEventHook(func() <-chan struct{} { return wake }, func() { polled <- struct{}{} })
	reader.rawReader = bufio.NewReaderSize(ownerInput{source: source, lr: reader}, rawInputBufferSize)
	done := make(chan inputResult, 1)
	go func() {
		line, err := reader.editLine()
		done <- inputResult{data: []byte(line), err: err}
	}()
	if _, err := io.WriteString(writer, "ab"); err != nil {
		t.Fatal(err)
	}
	wake <- struct{}{}
	select {
	case <-polled:
	case <-time.After(3 * time.Second):
		t.Fatal("partial input prevented owner wake")
	}
	if _, err := io.WriteString(writer, "\x1b[DZ\x1b[200~x\n\tY\x1b[201~\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if string(got.data) != "aZx\n\tYb" || got.err != nil {
			t.Fatalf("input=%q err=%v", got.data, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("escape/paste input did not submit")
	}
}

func TestReadInputRuneDecodesUTF8(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("你好\r"))

	first, err := readInputRune(r)
	if err != nil {
		t.Fatalf("read first rune: %v", err)
	}
	if first != '你' {
		t.Fatalf("first rune = %q, want %q", first, '你')
	}

	second, err := readInputRune(r)
	if err != nil {
		t.Fatalf("read second rune: %v", err)
	}
	if second != '好' {
		t.Fatalf("second rune = %q, want %q", second, '好')
	}

	third, err := readInputRune(r)
	if err != nil {
		t.Fatalf("read third rune: %v", err)
	}
	if third != '\r' {
		t.Fatalf("third rune = %q, want carriage return", third)
	}
}

func TestCompletionCycleAdvancesCandidates(t *testing.T) {
	list := []string{"/api-key", "/attach", "/base-url"}

	next, idx, ok := completionCycleNext("/", "/", list, -1)
	if !ok {
		t.Fatal("first cycle did not advance")
	}
	if next != "/api-key" || idx != 0 {
		t.Fatalf("first candidate = (%q, %d), want (/api-key, 0)", next, idx)
	}

	next, idx, ok = completionCycleNext("/api-key", "/", list, idx)
	if !ok {
		t.Fatal("second cycle did not advance")
	}
	if next != "/attach" || idx != 1 {
		t.Fatalf("second candidate = (%q, %d), want (/attach, 1)", next, idx)
	}

	if _, _, ok = completionCycleNext("/custom", "/", list, idx); ok {
		t.Fatal("cycle advanced after user-edited input")
	}
}
