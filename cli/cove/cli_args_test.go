package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

func TestParseCLIArgsResumeKeepsSessionID(t *testing.T) {
	for _, flag := range []string{"-r", "--resume"} {
		opts, err := parseCLIArgs([]string{flag, "session-42"})
		if err != nil {
			t.Fatalf("%s: unexpected error %v", flag, err)
		}
		if opts.resumeID != "session-42" {
			t.Fatalf("%s: resumeID = %q, want session-42", flag, opts.resumeID)
		}
	}
}

func TestParseCLIArgsResumeWithPrint(t *testing.T) {
	opts, err := parseCLIArgs([]string{"-r", "session-42", "-p", "继续"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.resumeID != "session-42" || !opts.printMode || opts.printPrompt != "继续" {
		t.Fatalf("got resume=%q print=%v prompt=%q", opts.resumeID, opts.printMode, opts.printPrompt)
	}
}

// "-r -p x" used to take "-p" as the session ID and drop print mode.
func TestParseCLIArgsResumeWithoutIDIsAnError(t *testing.T) {
	for _, args := range [][]string{{"-r"}, {"--resume", "-p", "hi"}} {
		if _, err := parseCLIArgs(args); err == nil {
			t.Fatalf("%v: expected an error for a missing session id", args)
		}
	}
}

// A typo such as --no-tiu used to be dropped silently and cove started with
// the defaults the user was trying to change.
func TestParseCLIArgsRejectsUnknownFlag(t *testing.T) {
	_, err := parseCLIArgs([]string{"--no-tiu"})
	if err == nil || !strings.Contains(err.Error(), "--no-tiu") {
		t.Fatalf("expected an error naming the flag, got %v", err)
	}
}

// Without -p a bare word was ignored and the REPL started as if nothing had
// been typed.
func TestParseCLIArgsRejectsStrayPositional(t *testing.T) {
	if _, err := parseCLIArgs([]string{"hello"}); err == nil {
		t.Fatal("expected an error for a positional argument without -p")
	}
}

// "cove -p --image a.png 描述" sent "--image" as the prompt and dropped both
// the image and the real prompt.
func TestParseCLIArgsPrintDoesNotSwallowFollowingFlag(t *testing.T) {
	opts, err := parseCLIArgs([]string{"-p", "--image", "a.png", "描述这张图"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.printPrompt != "描述这张图" {
		t.Fatalf("prompt = %q, want 描述这张图", opts.printPrompt)
	}
	if !reflect.DeepEqual(opts.attachments, []string{"a.png"}) {
		t.Fatalf("attachments = %v", opts.attachments)
	}
}

// An unquoted prompt used to keep only its first word.
func TestParseCLIArgsPrintJoinsUnquotedWords(t *testing.T) {
	opts, err := parseCLIArgs([]string{"-p", "explain", "this", "code"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.printPrompt != "explain this code" {
		t.Fatalf("prompt = %q", opts.printPrompt)
	}
}

// A prompt may itself start with a dash; only an exact known flag stops -p
// from taking the next argument.
func TestParseCLIArgsPrintAcceptsDashPrompt(t *testing.T) {
	opts, err := parseCLIArgs([]string{"-p", "-v 参数是什么意思"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.printPrompt != "-v 参数是什么意思" {
		t.Fatalf("prompt = %q", opts.printPrompt)
	}
}

func TestParseCLIArgsValueFlagsNeedAValue(t *testing.T) {
	for _, flag := range []string{"--image", "--file", "--profile"} {
		if _, err := parseCLIArgs([]string{flag}); err == nil {
			t.Fatalf("%s without a value: expected an error", flag)
		}
	}
}

func TestParseCLIArgsRejectsRemovedRecordingFlags(t *testing.T) {
	for _, flag := range []string{"--record", "--replay"} {
		if _, err := parseCLIArgs([]string{flag, "recordings"}); err == nil || !strings.Contains(err.Error(), "未知参数") {
			t.Fatalf("%s: expected unknown flag error, got %v", flag, err)
		}
	}
}

func TestParseCLIArgsActionsStopParsing(t *testing.T) {
	opts, err := parseCLIArgs([]string{"--list-sessions", "all"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.action != actionListSessions || !opts.listAll {
		t.Fatalf("got action=%v all=%v", opts.action, opts.listAll)
	}
	opts, err = parseCLIArgs([]string{"--doctor", "--whatever"})
	if err != nil || opts.action != actionDoctor {
		t.Fatalf("--doctor: action=%v err=%v", opts.action, err)
	}
}

func TestParseCLIArgsFlags(t *testing.T) {
	// --image/--file belong to -p: without it they were silently dropped
	// (the interactive session never read them), now they are refused.
	if _, err := parseCLIArgs([]string{"--image", "b.png"}); err == nil || !strings.Contains(err.Error(), "-p") {
		t.Fatalf("--image without -p: err=%v, want a refusal naming -p", err)
	}
	opts, err := parseCLIArgs([]string{"-d", "--no-auto", "--no-tui", "--dump-system-prompt", "--profile", "work", "--file", "a.go", "--image", "b.png", "-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.debug || !opts.noAuto || !opts.noTUI || !opts.dumpPrompt {
		t.Fatalf("boolean flags not set: %+v", opts)
	}
	if opts.profile != "work" {
		t.Fatalf("value flags not set: %+v", opts)
	}
	if !reflect.DeepEqual(opts.attachments, []string{"a.go", "b.png"}) {
		t.Fatalf("attachments = %v", opts.attachments)
	}
}

// Git Bash's mintty presents the terminal as a named pipe. Treating it as
// piped input would make every "cove -p" there wait for stdin data that the
// user is never going to type.
func TestIsMSYSPtyPipeName(t *testing.T) {
	cases := map[string]bool{
		`\msys-1888ae32e00d56aa-pty0-from-master`:                  true,
		`\cygwin-e022582115c10879-pty4-to-master`:                  true,
		`\Device\NamedPipe\msys-1888ae32e00d56aa-pty1-from-master`: true,
		`\msys-1888ae32e00d56aa-cygwin-pipe`:                       false,
		`\pipe\my-app-stdin`:                                       false,
		``:                                                         false,
	}
	for name, want := range cases {
		if got := isMSYSPtyPipeName(name); got != want {
			t.Errorf("isMSYSPtyPipeName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPrintModeFailureExitCodes(t *testing.T) {
	var buf strings.Builder
	if code := printModeFailure(&buf, true, errors.New("context canceled")); code != 130 {
		t.Fatalf("canceled run exit code = %d, want 130", code)
	}
	if !strings.Contains(buf.String(), "已取消") {
		t.Fatalf("canceled run message = %q", buf.String())
	}
	buf.Reset()
	if code := printModeFailure(&buf, false, errors.New("api: 401")); code != 1 {
		t.Fatalf("failed run exit code = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "api: 401") {
		t.Fatalf("failed run message = %q", buf.String())
	}
}

type fakeSessionLoader map[string]*session.Record

func (f fakeSessionLoader) Load(id string) (*session.Record, error) {
	if r, ok := f[id]; ok {
		return r, nil
	}
	return nil, os.ErrNotExist
}

func TestResumeStartupSessionLoadsMessages(t *testing.T) {
	cwd := t.TempDir()
	store := fakeSessionLoader{"s1": {ID: "s1", Title: "t", Cwd: cwd, Messages: []api.Message{{Role: "user", Content: "hi"}}}}
	var loaded []api.Message
	r, warning, err := resumeStartupSession(store, "s1", cwd, func(r *session.Record) { loaded = r.Messages })
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "s1" || len(loaded) != 1 || loaded[0].Content != "hi" {
		t.Fatalf("session not loaded: r=%v loaded=%v", r, loaded)
	}
	if warning != "" {
		t.Fatalf("same project must not warn, got %q", warning)
	}
}

// Users copy the file name from ~/.cove/sessions; the extension is not part
// of the ID.
func TestResumeStartupSessionAcceptsFileName(t *testing.T) {
	store := fakeSessionLoader{"s1": {ID: "s1"}}
	for _, arg := range []string{"s1.json", "s1.jsonl", " s1.jsonl ", "s1"} {
		if _, _, err := resumeStartupSession(store, arg, "", func(*session.Record) {}); err != nil {
			t.Fatalf("%q: %v", arg, err)
		}
	}
	// A dot inside an ID is not an extension.
	dotted := fakeSessionLoader{"2026.09.25": {ID: "2026.09.25"}}
	if _, _, err := resumeStartupSession(dotted, "2026.09.25", "", func(*session.Record) {}); err != nil {
		t.Fatalf("dotted id: %v", err)
	}
}

func TestResumeStartupSessionWarnsForOtherProject(t *testing.T) {
	other := filepath.Join(t.TempDir(), "other")
	store := fakeSessionLoader{"s1": {ID: "s1", Cwd: other}}
	_, warning, err := resumeStartupSession(store, "s1", t.TempDir(), func(*session.Record) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning, other) {
		t.Fatalf("expected a warning naming %s, got %q", other, warning)
	}
}

func TestResumeStartupSessionMissingIDFails(t *testing.T) {
	called := false
	_, _, err := resumeStartupSession(fakeSessionLoader{}, "nope", "", func(*session.Record) { called = true })
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected a not-exist error, got %v", err)
	}
	if called {
		t.Fatal("messages must not be replaced when the session is missing")
	}
}

// "cove -r index.json" names the sessions index, not a session: it fails
// with a clear message instead of loading the index as a session.
func TestResumeStartupSessionRejectsIndex(t *testing.T) {
	store := fakeSessionLoader{"index": {ID: "index"}}
	for _, arg := range []string{"index", "index.json", "index.jsonl"} {
		called := false
		_, _, err := resumeStartupSession(store, arg, "", func(*session.Record) { called = true })
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("%q: err = %v, want a reserved-id error", arg, err)
		}
		if called {
			t.Fatalf("%q: resume called for the index file", arg)
		}
	}
}
