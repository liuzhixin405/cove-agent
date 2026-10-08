package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

// steerTestEngine is a real engine whose files all land in temp dirs; the
// runner needs the concrete type for Steer/TakePendingSteer.
func steerTestEngine(t *testing.T) *engine.Engine {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
	t.Chdir(t.TempDir())
	eng, err := engine.New(engine.Config{
		Model:          "test-model",
		PermissionMode: "auto",
		MaxBudget:      100,
		Provider:       api.ProviderConfig{Name: "mock", APIKey: "sk-test"},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return eng
}

func TestDurableQueueRecoveryDoesNotExecuteAndRetainsAttachments(t *testing.T) {
	captureTurnOutput(t)
	eng := steerTestEngine(t)
	eng.SetAutoExtract(false)
	prov := &blockingProvider{firstStarted: make(chan struct{}), release: make(chan struct{})}
	eng.SetProvider(prov)
	r := newREPLTaskRunner(eng)
	t.Cleanup(func() {
		r.CancelForExit()
		r.WaitIdleUntil(time.Now().Add(10 * time.Second))
	})
	r.Enqueue(api.Message{Role: "user", Content: "first task"})
	select {
	case <-prov.firstStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("task did not start")
	}
	attachment := api.Message{Role: "user", Content: "attachment", Parts: []api.MessagePart{{Type: "text", Text: "persisted file contents"}}}
	r.Enqueue(attachment)
	r.Enqueue(api.Message{Role: "user", Content: "third"})
	saved, err := r.queueStore.Load(r.queueID)
	if err != nil || saved.Current == nil || saved.Current.Content != "first task" || len(saved.Pending) != 2 {
		t.Fatalf("in-flight snapshot = %+v, %v", saved, err)
	}
	r.CancelForExit()
	if !r.WaitIdleUntil(time.Now().Add(10 * time.Second)) {
		t.Fatal("cancelled runner did not finish")
	}
	restarted := newREPLTaskRunner(eng)
	if restarted.IsRunning() || len(prov.requests()) != 1 {
		t.Fatal("constructor replayed the saved queue")
	}
	if output := restarted.taskQueueCommand([]string{"restore", r.queueID}); !strings.Contains(output, "已恢复并暂停") {
		t.Fatal(output)
	}
	if output := restarted.taskQueueCommand([]string{"run"}); !strings.Contains(output, "状态不明") || len(prov.requests()) != 1 {
		t.Fatalf("uncertain task was replayed: %q", output)
	}
	restarted.taskQueueCommand([]string{"move", "2", "1"})
	restarted.taskQueueCommand([]string{"remove", "1"})
	saved, err = restarted.queueStore.Load(restarted.queueID)
	if err != nil || len(saved.Pending) != 1 || len(saved.Pending[0].Parts) != 1 || saved.Pending[0].Parts[0].Text != "persisted file contents" || saved.Current == nil {
		t.Fatalf("managed snapshot lost attachment/current: %+v, %v", saved, err)
	}
	restarted.taskQueueCommand([]string{"skip"})
	restarted.taskQueueCommand([]string{"run"})
	if !restarted.WaitIdleUntil(time.Now().Add(10 * time.Second)) {
		t.Fatal("confirmed queue did not finish")
	}
	if len(prov.requests()) != 2 {
		t.Fatalf("model calls = %d; wanted initial + explicitly confirmed attachment task", len(prov.requests()))
	}
	if _, err := restarted.queueStore.Load(restarted.queueID); !os.IsNotExist(err) {
		t.Fatalf("completed queue snapshot remains: %v", err)
	}
}

func TestDurableQueueStorageFailurePreventsExecution(t *testing.T) {
	captureTurnOutput(t)
	eng := steerTestEngine(t)
	prov := &blockingProvider{firstStarted: make(chan struct{}), release: make(chan struct{})}
	eng.SetProvider(prov)
	r := newREPLTaskRunner(eng)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	r.queueStore = session.NewQueueStore(blocked)
	feedback := r.SubmitWithFeedback(api.Message{Role: "user", Content: "must not run"})
	snapshot := r.Snapshot()
	if !snapshot.Paused || snapshot.Running || snapshot.PersistenceError == "" || len(snapshot.Queued) != 1 || !strings.Contains(feedback, "已暂停") || len(prov.requests()) != 0 {
		t.Fatalf("storage error started/lost work: %+v, %q", snapshot, feedback)
	}
}

func TestDurableQueueSessionSwitchKeepsOriginalOwnership(t *testing.T) {
	eng := steerTestEngine(t)
	r := newREPLTaskRunner(eng)
	r.paused = true
	r.pendingFailedMsg = &api.Message{Role: "user", Content: "uncertain"}
	r.Enqueue(api.Message{Role: "user", Content: "pending"})
	oldID, oldSession := r.queueID, r.queueSession
	eng.NewSession(context.Background())
	r.ClearPendingFailed()
	saved, err := r.queueStore.Load(oldID)
	if err != nil || saved.SessionID != oldSession || saved.Current == nil || len(saved.Pending) != 1 || saved.OwnerPID != 0 {
		t.Fatalf("session switch lost original queue: %+v, %v", saved, err)
	}
	if len(r.Snapshot().Queued) != 0 || r.queueSession != eng.SessionID() {
		t.Fatal("old work was mixed into the new session")
	}
}

// A line typed while a task runs is steered into that task, not queued: the
// engine holds it for its next model call, the queue stays empty and the
// feedback says so. Several lines join with a newline.
func TestTypedWhileRunningIsSteeredNotQueued(t *testing.T) {
	eng := steerTestEngine(t)
	r := newREPLTaskRunner(eng)
	r.mu.Lock()
	r.running = true
	r.current = api.Message{Role: "user", Content: "first"}
	r.mu.Unlock()

	if msg := r.SubmitWithFeedback(api.Message{Role: "user", Content: "只看 Go 文件"}); msg != steerFeedback {
		t.Errorf("feedback = %q, want %q", msg, steerFeedback)
	}
	if !strings.Contains(steerFeedback, "[已插入]") || !strings.Contains(steerFeedback, "当前任务") {
		t.Errorf("steer feedback %q does not say the line went into the running task", steerFeedback)
	}
	r.SubmitWithFeedback(api.Message{Role: "user", Content: "跳过测试"})

	if text, n := eng.PendingSteer(); text != "只看 Go 文件\n跳过测试" || n != 2 {
		t.Errorf("engine pending steer = %q, %d", text, n)
	}
	snap := r.Snapshot()
	if len(snap.Queued) != 0 {
		t.Errorf("queue = %v, want nothing queued", snap.Queued)
	}
	if !strings.Contains(snap.PendingSteer, "只看 Go 文件") {
		t.Errorf("snapshot pending steer = %q", snap.PendingSteer)
	}
	if out := formatTaskSnapshot(snap); !strings.Contains(out, "待生效指引: 只看 Go 文件") {
		t.Errorf("/tasks output lacks the pending steer line: %q", out)
	}
}

// Attachments cannot travel as guidance text, so a message with parts still
// queues behind the running task.
func TestAttachmentTypedWhileRunningStillQueues(t *testing.T) {
	eng := steerTestEngine(t)
	r := newREPLTaskRunner(eng)
	r.mu.Lock()
	r.running = true
	r.mu.Unlock()
	msg := api.Message{Role: "user", Content: "看看这张图", Parts: []api.MessagePart{{Type: "text", Text: "x"}}}
	if fb := r.SubmitWithFeedback(msg); !strings.Contains(fb, "[已排队]") {
		t.Errorf("feedback = %q, want queued", fb)
	}
	if _, n := eng.PendingSteer(); n != 0 {
		t.Errorf("attachment message was steered")
	}
	if snap := r.Snapshot(); len(snap.Queued) != 1 {
		t.Errorf("queue = %v", snap.Queued)
	}
}

// Guidance the task never got to consume is not lost: when the task ends it
// becomes a new task ahead of anything else queued.
func TestUnconsumedSteerIsReclaimedAsTheNextTask(t *testing.T) {
	eng := steerTestEngine(t)
	r := newREPLTaskRunner(eng)
	r.mu.Lock()
	r.running = true
	r.queue = append(r.queue, api.Message{Role: "user", Content: "queued earlier"})
	r.mu.Unlock()
	eng.Steer("改用表驱动测试")

	r.mu.Lock()
	reclaimed := r.reclaimSteerLocked()
	r.mu.Unlock()
	if !reclaimed {
		t.Fatal("pending steer was not reclaimed")
	}
	if got := eng.TakePendingSteer(); got != "" {
		t.Errorf("engine still holds %q", got)
	}
	snap := r.Snapshot()
	if len(snap.Queued) != 2 || snap.Queued[0] != "改用表驱动测试" {
		t.Errorf("queue = %v, want the steer first", snap.Queued)
	}
	r.mu.Lock()
	again := r.reclaimSteerLocked()
	r.mu.Unlock()
	if again {
		t.Error("reclaim reported guidance when none was pending")
	}
}

// "exit" cancels the running task; guidance typed before that must not be
// turned into a new task that starts while the program is leaving.
func TestCancelForExitDropsTheSteerInsteadOfStartingIt(t *testing.T) {
	eng := steerTestEngine(t)
	r := newREPLTaskRunner(eng)
	r.mu.Lock()
	r.running = true
	r.cancel = func() {}
	r.mu.Unlock()
	eng.Steer("x")
	if !r.CancelForExit() {
		t.Fatal("CancelForExit did not report a running task")
	}
	r.mu.Lock()
	reclaimed := r.reclaimSteerLocked()
	r.mu.Unlock()
	if reclaimed {
		t.Error("steer reclaimed as a task during exit")
	}
}

// blockingProvider answers the first request only after release is closed,
// which is the window in which the test steers; every request gets a plain
// reply without tool calls, so the first turn ends before its next model
// call could consume the guidance.
type blockingProvider struct {
	mu           sync.Mutex
	reqs         []api.ChatRequest
	firstStarted chan struct{}
	release      chan struct{}
}

func (p *blockingProvider) Name() string        { return "mock" }
func (p *blockingProvider) DisplayName() string { return "mock" }
func (p *blockingProvider) Validate() error     { return nil }

func (p *blockingProvider) Chat(ctx context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	p.mu.Lock()
	req.Messages = append([]api.Message(nil), req.Messages...)
	p.reqs = append(p.reqs, req)
	n := len(p.reqs) - 1
	p.mu.Unlock()
	if n == 0 {
		close(p.firstStarted)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &api.ChatResponse{Content: "done"}, nil
}

func (p *blockingProvider) ChatStream(ctx context.Context, req api.ChatRequest, h api.StreamHandler) (*api.ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err == nil && h != nil {
		h(api.StreamEvent{Type: "delta", Delta: resp.Content})
	}
	return resp, err
}

func (p *blockingProvider) requests() []api.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]api.ChatRequest(nil), p.reqs...)
}

// End to end through the runner: a task that finishes without consuming the
// guidance typed during it is followed by a second task carrying that
// guidance as its user message.
func TestSteerTypedTooLateRunsAsTheNextTask(t *testing.T) {
	captureTurnOutput(t)
	eng := steerTestEngine(t)
	eng.SetAutoExtract(false) // no background memory/review/dream calls: the test counts model calls exactly
	prov := &blockingProvider{firstStarted: make(chan struct{}), release: make(chan struct{})}
	eng.SetProvider(prov)
	r := newREPLTaskRunner(eng)

	if fb := r.SubmitWithFeedback(api.Message{Role: "user", Content: "第一个任务"}); fb != "" {
		t.Fatalf("idle start gave feedback %q", fb)
	}
	select {
	case <-prov.firstStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("first task never called the model")
	}
	if fb := r.SubmitWithFeedback(api.Message{Role: "user", Content: "然后写测试"}); fb != steerFeedback {
		t.Fatalf("feedback while running = %q", fb)
	}
	close(prov.release)

	if !r.WaitIdleUntil(time.Now().Add(15 * time.Second)) {
		t.Fatal("runner did not go idle")
	}
	reqs := prov.requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2 (the reclaimed steer as a second task)", len(reqs))
	}
	found := false
	for _, m := range reqs[1].Messages {
		if m.Role == "user" && strings.Contains(m.Content, "然后写测试") {
			found = true
		}
	}
	if !found {
		t.Fatalf("second task did not carry the steer as its user message: %+v", reqs[1].Messages)
	}
	if got := eng.TakePendingSteer(); got != "" {
		t.Errorf("steer left in the engine: %q", got)
	}
}
