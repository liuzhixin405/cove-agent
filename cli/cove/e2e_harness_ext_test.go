package main

// Extensions of the e2e harness for the flow tests (flow_f*_test.go, test
// design 5.2): Ctrl+C / Ctrl+D, -r at start-up, pre-seeded homes, a fake
// model script that can be reset between subtests, and failure output that
// carries the tail of the terminal and a summary of what the model received.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/dream"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

// ---------- fake model ----------

// Reset replaces the script and forgets the requests received so far, so
// subtests sharing one server (and one e2eHome) each count their own calls.
func (f *fakeModel) Reset(steps ...fakeStep) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = steps
	f.next = 0
	f.reqs = nil
}

// WaitRequests blocks until the model has received at least n requests.
func (f *fakeModel) WaitRequests(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(f.Requests()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("model received %d requests, want %d within %v\n%s", len(f.Requests()), n, timeout, f.Summary())
}

// Summary is one line per request: its non-system messages, role and the
// first 60 runes of the text, for failure output.
func (f *fakeModel) Summary() string {
	var sb strings.Builder
	for i, r := range f.Requests() {
		fmt.Fprintf(&sb, "req %d (model %s):", i+1, r.Model)
		for _, m := range r.Messages {
			if m.Role == "system" {
				continue
			}
			txt := []rune(strings.Join(strings.Fields(m.Text()), " "))
			if len(txt) > 60 {
				txt = append(txt[:60], '…')
			}
			fmt.Fprintf(&sb, " | %s: %s", m.Role, string(txt))
		}
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		return "(no requests)\n"
	}
	return sb.String()
}

// lastMessage is the last non-system message of req.
func lastMessage(req capturedRequest) capturedMessage {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "system" {
			return req.Messages[i]
		}
	}
	return capturedMessage{}
}

// countMessages counts req's messages of role whose text contains sub
// (role "" matches any role).
func countMessages(req capturedRequest, role, sub string) int {
	n := 0
	for _, m := range req.Messages {
		if (role == "" || m.Role == role) && strings.Contains(m.Text(), sub) {
			n++
		}
	}
	return n
}

// ---------- home ----------

// e2eHomeOptions pre-seeds an e2eHome.
type e2eHomeOptions struct {
	// Config entries merged over the default config.json.
	Config map[string]any
	// Policies is written verbatim as policies.json in the config directory.
	Policies string
	// ProjectConfig is written verbatim as the project's .cove.json.
	ProjectConfig string
	// Memories are global memory files (name -> content) in ~/.cove/memory.
	Memories map[string]string
	// GitInit makes the project a git repository with one commit.
	GitInit bool
}

// e2eHomeWith is e2eHome with the options applied.
func e2eHomeWith(t *testing.T, model *fakeModel, o e2eHomeOptions) (home, project string) {
	t.Helper()
	home, project = e2eHome(t, model)
	cfgDir := filepath.Join(home, ".cove")
	if len(o.Config) > 0 {
		path := filepath.Join(cfgDir, "config.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg := map[string]any{}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		for k, v := range o.Config {
			cfg[k] = v
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if o.Policies != "" {
		writeTestFile(t, filepath.Join(cfgDir, "policies.json"), o.Policies)
	}
	if o.ProjectConfig != "" {
		writeTestFile(t, filepath.Join(project, ".cove.json"), o.ProjectConfig)
	}
	for name, body := range o.Memories {
		writeTestFile(t, filepath.Join(cfgDir, "memory", name), body)
	}
	if o.GitInit {
		gitInitProject(t, project)
	}
	return home, project
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// gitInitProject makes dir a repository with one commit (skips without git).
func gitInitProject(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	writeTestFile(t, filepath.Join(dir, "README.md"), "# project\n")
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=e2e@example.com", "-c", "user.name=e2e", "add", "."},
		{"-c", "user.email=e2e@example.com", "-c", "user.name=e2e", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// sessionFile loads session id from the home's sessions directory, the way
// a new process would (not through the running engine).
func sessionFile(t *testing.T, home, id string) *session.Record {
	t.Helper()
	r, err := session.NewStoreAt(filepath.Join(home, ".cove", "sessions")).Load(id)
	if err != nil {
		t.Fatalf("session %s not on disk: %v", id, err)
	}
	return r
}

// ---------- session ----------

// startREPLResumed starts the REPL the way `cove -r id` does.
func startREPLResumed(t *testing.T, id string) *e2eSession {
	t.Helper()
	return startREPLWith(t, func(app *appBootstrap) {
		if _, _, err := resumeStartupSession(app.eng.Store(), id, currentProjectDir(), app.eng.ResumeSession); err != nil {
			t.Fatalf("resume %s: %v", id, err)
		}
	})
}

// CloseStdin is Ctrl+D on an empty line: the plain reader sees the end of
// its input, which the REPL handles like /exit.
func (s *e2eSession) CloseStdin() {
	_ = s.stdinW.Close()
}

// WaitExited blocks until the REPL loop has returned.
func (s *e2eSession) WaitExited(timeout time.Duration) {
	s.t.Helper()
	select {
	case <-s.done:
	case <-time.After(timeout):
		s.t.Fatalf("REPL did not exit within %v; output:\n%s", timeout, s.Tail(40))
	}
}

// Restarted reports whether the REPL returned asking for a restart; call
// after WaitExited.
func (s *e2eSession) Restarted() bool {
	<-s.done
	return s.restarted
}

// Mark is the current length of the output, for OutputSince.
func (s *e2eSession) Mark() int { return len(s.Output()) }

// OutputSince is what was printed after mark.
func (s *e2eSession) OutputSince(mark int) string {
	out := s.Output()
	if mark > len(out) {
		return ""
	}
	return out[mark:]
}

// WaitForSince is WaitFor looking only at the output printed after mark.
func (s *e2eSession) WaitForSince(mark int, want string, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(s.OutputSince(mark), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %q after offset %d; last 40 lines:\n%s", want, mark, s.Tail(40))
}

// Tail is the last n lines of the output.
func (s *e2eSession) Tail(n int) string {
	lines := strings.Split(s.Output(), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// WaitIdle asks /tasks until the REPL says no task is running.
func (s *e2eSession) WaitIdle(timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mark := s.Mark()
		s.Type("/tasks")
		step := time.Now().Add(2 * time.Second)
		for time.Now().Before(step) {
			out := s.OutputSince(mark)
			if strings.Contains(out, "当前没有运行中的任务") {
				return
			}
			if strings.Contains(out, "当前任务 (已运行") {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("the task never went idle; last 40 lines:\n%s", s.Tail(40))
}

// interruptNote is printed by both ways Interrupt stops a task (the SIGINT
// handler's "[已中断] …输入 /continue 可继续" and /stop's "[已取消] …输入
// /continue 可从中断处继续").
const interruptNote = "输入 /continue 可"

// ---------- failure context ----------

// flow bundles what a flow subtest's failure message carries: the last 40
// lines of the terminal and the requests the fake model received.
type flow struct {
	t     *testing.T
	s     *e2eSession
	model *fakeModel
}

func (f flow) Fatalf(format string, args ...any) {
	f.t.Helper()
	tail := "(no session)"
	if f.s != nil {
		tail = f.s.Tail(40)
	}
	f.t.Fatalf(format+"\n--- last 40 lines ---\n%s\n--- requests ---\n%s", append(args, tail, f.model.Summary())...)
}

func (f flow) Errorf(format string, args ...any) {
	f.t.Helper()
	tail := "(no session)"
	if f.s != nil {
		tail = f.s.Tail(40)
	}
	f.t.Errorf(format+"\n--- last 40 lines ---\n%s\n--- requests ---\n%s", append(args, tail, f.model.Summary())...)
}

// Requests fails unless the model received exactly n requests, and returns them.
func (f flow) Requests(n int) []capturedRequest {
	f.t.Helper()
	reqs := f.model.Requests()
	if len(reqs) != n {
		f.Fatalf("model received %d requests, want %d", len(reqs), n)
	}
	return reqs
}

// Absent fails when the output contains any of subs.
func (f flow) Absent(subs ...string) {
	f.t.Helper()
	out := f.s.Output()
	for _, sub := range subs {
		if strings.Contains(out, sub) {
			f.Fatalf("output contains %q, want it absent", sub)
		}
	}
}

// ---------- cove as a child process ----------

// coveMainArgsEnv carries the JSON argument list TestCoveMainHelperProcess
// runs main with.
const coveMainArgsEnv = "COVE_E2E_MAIN_ARGS"

// TestCoveMainHelperProcess is not a test: coveMainCommand re-runs the test
// binary with only this function selected, and it runs main as `cove
// <args>` would, so exit codes and a process killed mid-task can be
// observed. It inherits the parent's HOME / COVE_CONFIG_DIR and directory.
func TestCoveMainHelperProcess(t *testing.T) {
	raw := os.Getenv(coveMainArgsEnv)
	if raw == "" {
		t.Skip("helper process for coveMainCommand")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		fmt.Fprintf(os.Stderr, "bad %s: %v\n", coveMainArgsEnv, err)
		os.Exit(3)
	}
	os.Args = append([]string{"cove"}, args...)
	if os.Getenv(coveSpawnEnv) == "1" {
		dreamSpawn = helperDreamSpawn
	}
	main()
	os.Exit(0)
}

// coveMainCommand is `cove args...` as a child process in the current
// directory and environment (the e2eHome's HOME, config directory and
// plain-reader settings). extraEnv entries are added as KEY=VALUE.
func coveMainCommand(t *testing.T, extraEnv []string, args ...string) *exec.Cmd {
	t.Helper()
	b, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoveMainHelperProcess$")
	cmd.Env = append(append(os.Environ(), coveMainArgsEnv+"="+string(b)), extraEnv...)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Dir = dir
	return cmd
}

// sessionPath is the .jsonl file of session id in the home.
func sessionPath(home, id string) string {
	return filepath.Join(home, ".cove", "sessions", id+".jsonl")
}

// ---------- M6 helpers (flow F5-F7) ----------

// coveRun is the result of one `cove args...` child process.
type coveRun struct {
	code           int
	stdout, stderr string
}

// runCove runs `cove args...` as a child process (coveMainCommand) with
// stdin as its standard input (nil: no input, the null device) and waits
// for it, up to e2eTimeout.
func runCove(t *testing.T, stdin io.Reader, extraEnv []string, args ...string) coveRun {
	t.Helper()
	r, err := tryRunCove(t, stdin, extraEnv, args...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// tryRunCove is runCove reporting a failure to run instead of failing the
// test, for callers on another goroutine.
func tryRunCove(t *testing.T, stdin io.Reader, extraEnv []string, args ...string) (coveRun, error) {
	cmd := coveMainCommand(t, extraEnv, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return coveRun{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(e2eTimeout):
		_ = cmd.Process.Kill()
		<-done
		return coveRun{}, fmt.Errorf("cove %q did not exit within %v\nstdout:\n%s\nstderr:\n%s", args, e2eTimeout, out.String(), errb.String())
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return coveRun{}, fmt.Errorf("cove %q: %w", args, err)
		}
		code = exit.ExitCode()
	}
	return coveRun{code: code, stdout: out.String(), stderr: errb.String()}, nil
}

// waitUntil polls cond until it holds or fails the test after timeout.
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

// systemText is the joined text of req's system messages.
func systemText(req capturedRequest) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		if m.Role == "system" {
			sb.WriteString(m.Text())
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// readConfigFile parses the home's config.json into its top-level keys.
func readConfigFile(t *testing.T, home string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".cove", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("config.json: %v\n%s", err, b)
	}
	return m
}

// headlessRunHere is headlessRun in the home and directory already set up
// (headlessRun makes a fresh home each time).
func headlessRunHere(t *testing.T, input string) (failed bool, stdout, stderr string) {
	t.Helper()
	resetE2EGlobals()
	t.Cleanup(resetE2EGlobals)
	app, err := bootstrapApp(false, "", false)
	if err != nil {
		t.Fatal(err)
	}
	app.eng.SetAutoExtract(false)
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	outC, errC := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); outC <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errC <- string(b) }()
	failed = runHeadlessFrom(strings.NewReader(input), app, registerAllCommands(), "")
	app.eng.SaveSession()
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	return failed, <-outC, <-errC
}

// ---------- background learning (F7) ----------

// learningModel is a fake provider whose base URL the engine does not take
// for a local server: engine.localProvider skips background learning
// (memory extraction, skill review) for 127.0.0.1 and localhost, so the
// server listens on another loopback address, 127.0.0.2. It routes each
// request by what it is to its own script — fg (the conversation's turns),
// extract (memory extraction), review (skill review), dream (consolidation)
// — each a fakeModel with its own steps and recorded requests, so the
// background calls never consume a turn's scripted answer.
type learningModel struct {
	srv                        *httptest.Server
	fg, extract, review, dream *fakeModel
	mu                         sync.Mutex
	holds                      map[string]*learningHold
}

// learningHold keeps requests of one kind waiting until released.
type learningHold struct {
	arrived     chan struct{}
	arrivedOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func (h *learningHold) open() { h.releaseOnce.Do(func() { close(h.release) }) }

// newLearningModel starts the server, or skips the test when 127.0.0.2
// cannot be bound (macOS routes only 127.0.0.1 by default).
func newLearningModel(t *testing.T) *learningModel {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("cannot listen on 127.0.0.2 (needed so background learning treats the fake model as remote): %v", err)
	}
	m := &learningModel{fg: newFakeModel(t), extract: newFakeModel(t), review: newFakeModel(t), dream: newFakeModel(t),
		holds: map[string]*learningHold{}}
	m.srv = httptest.NewUnstartedServer(http.HandlerFunc(m.route))
	m.srv.Listener = ln
	m.srv.Start()
	t.Cleanup(func() { m.releaseAll(); m.srv.Close() })
	return m
}

func (m *learningModel) BaseURL() string { return m.srv.URL + "/v1" }

// Provider is the provider entry of config.json pointing at the server.
func (m *learningModel) Provider() map[string]any {
	return map[string]any{"name": "openai-compatible", "base_url": m.BaseURL(), "api_key": "test-key"}
}

// Hold makes requests of kind ("extract", "review", "dream") wait, before
// they are recorded or answered, until release is called; arrived is
// closed when the first one comes in.
func (m *learningModel) Hold(kind string) (arrived <-chan struct{}, release func()) {
	h := &learningHold{arrived: make(chan struct{}), release: make(chan struct{})}
	m.mu.Lock()
	m.holds[kind] = h
	m.mu.Unlock()
	return h.arrived, func() {
		m.mu.Lock()
		if m.holds[kind] == h {
			delete(m.holds, kind)
		}
		m.mu.Unlock()
		h.open()
	}
}

func (m *learningModel) releaseAll() {
	m.mu.Lock()
	holds := m.holds
	m.holds = map[string]*learningHold{}
	m.mu.Unlock()
	for _, h := range holds {
		h.open()
	}
}

// learningKind tells a background request from a turn by its prompt.
func learningKind(req capturedRequest) string {
	for _, msg := range req.Messages {
		txt := msg.Text()
		switch {
		case strings.Contains(txt, "memory extraction agent"), strings.Contains(txt, "extract any important information worth remembering"):
			return "extract"
		case strings.Contains(txt, "对话回顾助手"):
			return "review"
		case strings.Contains(txt, "Sessions since last consolidation"):
			return "dream"
		}
	}
	return "turn"
}

func (m *learningModel) route(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req capturedRequest
	_ = json.Unmarshal(body, &req)
	kind := learningKind(req)
	target := map[string]*fakeModel{"turn": m.fg, "extract": m.extract, "review": m.review, "dream": m.dream}[kind]
	m.mu.Lock()
	h := m.holds[kind]
	m.mu.Unlock()
	if h != nil {
		h.arrivedOnce.Do(func() { close(h.arrived) })
		select {
		case <-h.release:
		case <-r.Context().Done():
			return
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	target.handle(w, r)
}

// coveSpawnEnv makes a child cove (coveMainCommand) start its session-end
// dream worker for real, as another helper process (helperDreamSpawn):
// TestMain replaces the spawn with a no-op in every test process.
const coveSpawnEnv = "COVE_E2E_DREAM_SPAWN"

// helperDreamSpawn is dream.SpawnWorker for the test binary: `cove args...`
// as a TestCoveMainHelperProcess child with its output appended to
// dream.log, not waited for. (os.Executable is the test binary, which does
// not take cove's flags.)
func helperDreamSpawn(args []string) (int, error) {
	exe, err := os.Executable() // os.Args[0] is "cove" in a helper process
	if err != nil {
		return 0, err
	}
	b, _ := json.Marshal(args)
	cmd := exec.Command(exe, "-test.run=^TestCoveMainHelperProcess$")
	cmd.Env = append(os.Environ(), coveMainArgsEnv+"="+string(b), coveSpawnEnv+"=")
	logf, err := os.OpenFile(dream.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logf.Close() }()
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

// waitArrived waits for a Hold's arrived channel, up to e2eTimeout.
func waitArrived(t *testing.T, arrived <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-arrived:
	case <-time.After(e2eTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TypeUntil types line until want appears in the output printed after it
// (for a command whose answer depends on background work finishing).
func (s *e2eSession) TypeUntil(line, want string, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mark := s.Mark()
		s.Type(line)
		step := time.Now().Add(time.Second)
		for time.Now().Before(step) {
			if strings.Contains(s.OutputSince(mark), want) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	s.t.Fatalf("%q never answered %q within %v; last 40 lines:\n%s", line, want, timeout, s.Tail(40))
}
