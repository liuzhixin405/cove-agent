package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestRestartArgs(t *testing.T) {
	for _, tc := range []struct {
		orig      []string
		sessionID string
		want      []string
	}{
		{nil, "", nil},
		{nil, "abc", []string{"-r", "abc"}},
		// The session to continue replaces the one cove was started with.
		{[]string{"--resume", "old", "--profile", "work"}, "new", []string{"--profile", "work", "-r", "new"}},
		{[]string{"-r", "old"}, "", nil},
		// Attachments went with the first message; sending them again would
		// attach them to whatever the user types next.
		{[]string{"--image", "a.png", "--no-auto", "--file", "b.txt"}, "s", []string{"--no-auto", "-r", "s"}},
		{[]string{"--tui", "-d"}, "s", []string{"--tui", "-d", "-r", "s"}},
		// A replay restarted from its first recorded response would answer
		// the next message with the recording's opening reply again.
	} {
		if got := restartArgs(tc.orig, tc.sessionID); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("restartArgs(%q, %q) = %q, want %q", tc.orig, tc.sessionID, got, tc.want)
		}
	}
}

// Headless reads its input from a pipe, which a restarted process could not
// pick up where this one stopped.
func TestRestartRefusedInHeadless(t *testing.T) {
	fe, notices := headlessFrontend(t)
	if !fe.dispatch("/restart") {
		t.Fatal("/restart not dispatched")
	}
	if fe.exitRequested || fe.restartRequested {
		t.Fatal("headless /restart asked the loop to leave")
	}
	if all := strings.Join(*notices, "\n"); !strings.Contains(all, "headless") {
		t.Fatalf("no explanation: %q", all)
	}
}

func TestRestartLeavesTheInteractiveLoop(t *testing.T) {
	fe, _ := headlessFrontend(t)
	fe.tasks = newREPLTaskRunner(fe.eng)
	if !fe.dispatch("/restart") {
		t.Fatal("/restart not dispatched")
	}
	if !fe.exitRequested || !fe.restartRequested {
		t.Fatalf("exitRequested=%v restartRequested=%v, want both", fe.exitRequested, fe.restartRequested)
	}
}

// A restart would cut off the running task, so it waits like /new does.
func TestRestartWaitsForTheRunningTask(t *testing.T) {
	if !commandMutatesEngine("/restart") {
		t.Fatal("/restart must not run while a task runs")
	}
}
