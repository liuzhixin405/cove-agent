package main

import (
	"strings"
	"testing"
)

// The editor hands lines over as typed. Exact-match handlers compared the
// raw text, so "/history " (a trailing space, as Tab completion leaves)
// tried to resume a session named "", and a line of spaces was sent to the
// model as an empty message.
func TestHistorySubcommandWithAndWithoutArgument(t *testing.T) {
	for _, tc := range []struct {
		rest, name, arg string
		ok              bool
	}{
		{"detail", "detail", "", true},
		{"Detail", "detail", "", true},
		{"detail 3", "detail", "3", true},
		{"detail   session-x  ", "detail", "session-x", true},
		{"details", "detail", "", false},
		{"delete", "delete", "", true},
		{"delete 2", "delete", "2", true},
		{"3", "detail", "", false},
	} {
		_, arg, ok := historySubcommand(tc.rest, tc.name)
		if ok != tc.ok || arg != tc.arg {
			t.Errorf("historySubcommand(%q, %q) = (%q, %v), want (%q, %v)", tc.rest, tc.name, arg, ok, tc.arg, tc.ok)
		}
	}
}

// Bare "/history detail" prints the usage line instead of trying to resume a
// session called "detail".
func TestBareHistoryDetailPrintsUsage(t *testing.T) {
	out := captureOut(t)
	eng := newTestEngine(t)
	pending := false
	if !handleSessionCommand("/history detail", eng, &pending, nil) {
		t.Fatal("not handled")
	}
	if got := out.String(); !strings.Contains(got, "用法") || strings.Contains(got, "恢复会话失败") {
		t.Fatalf("output = %q, want the usage line", got)
	}
}
