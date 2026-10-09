package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/diagnostic"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

type replTaskRunner struct {
	mu               sync.Mutex
	cond             *sync.Cond
	eng              *engine.Engine
	running          bool
	cancel           context.CancelFunc
	queue            []api.Message
	pendingFailedMsg *api.Message
	current          api.Message
	currentStart     time.Time
	queueStore       *session.QueueStore
	queueID          string
	queueSession     string
	queueCwd         string
	paused           bool
	persistenceError string
	// closing is set by CancelForExit: the program is leaving, so a task
	// that ends now neither reclaims its steer nor starts the next one.
	closing bool
}

// TaskSnapshot is a read-only view of the runner state for /tasks.
type TaskSnapshot struct {
	Running          bool
	Current          string
	Elapsed          time.Duration
	Queued           []string
	PendingRetry     string
	Paused           bool
	PersistenceError string
	// PendingSteer previews guidance steered into the running task that its
	// next model call has not picked up yet.
	PendingSteer string
}

func taskPreview(msg api.Message) string {
	s := strings.TrimSpace(msg.Content)
	if s == "" && len(msg.Parts) > 0 {
		s = "(含附件的消息)"
	}
	s = strings.Join(strings.Fields(s), " ")
	rs := []rune(s)
	if len(rs) > 60 {
		return string(rs[:60]) + "…"
	}
	return s
}

// Snapshot returns the current running/queued task state for display.
func (r *replTaskRunner) Snapshot() TaskSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := TaskSnapshot{Running: r.running, Paused: r.paused, PersistenceError: r.persistenceError}
	if r.running {
		snap.Current = taskPreview(r.current)
		snap.Elapsed = time.Since(r.currentStart)
		if r.eng != nil {
			text, _ := r.eng.PendingSteer()
			snap.PendingSteer = taskPreview(api.Message{Content: text})
		}
	}
	for _, m := range r.queue {
		snap.Queued = append(snap.Queued, taskPreview(m))
	}
	if r.pendingFailedMsg != nil {
		snap.PendingRetry = taskPreview(*r.pendingFailedMsg)
	}
	return snap
}

func canMergeQueuedTask(existing, incoming api.Message) bool {
	if existing.Role != "user" || incoming.Role != "user" {
		return false
	}
	if len(existing.Parts) > 0 || len(incoming.Parts) > 0 {
		return false
	}
	a := normalizeTaskForMerge(existing.Content)
	b := normalizeTaskForMerge(incoming.Content)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	// A short input is not "the same task" as a long one that happens to
	// contain it: "run" was merged into "run pytest and fix the failures"
	// and dropped, while the feedback said it had been added.
	if utf8.RuneCountInString(a) < minMergeRunes || utf8.RuneCountInString(b) < minMergeRunes {
		return false
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return true
	}
	if commonPrefixRunes(a, b) >= 24 {
		return true
	}
	return false
}

func mergeQueuedTask(existing, incoming api.Message) api.Message {
	merged := existing
	add := strings.TrimSpace(incoming.Content)
	if add == "" {
		return merged
	}
	base := strings.TrimSpace(existing.Content)
	if base == "" {
		merged.Content = add
		return merged
	}
	if strings.Contains(base, add) {
		return merged
	}
	merged.Content = base + "\n\n[补充要求]\n" + add
	return merged
}

func normalizeTaskForMerge(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"\n", " ", "\t", " ",
		"，", " ", "。", " ", "！", " ", "？", " ",
		",", " ", ".", " ", "!", " ", "?", " ",
		"：", " ", ":", " ", ";", " ", "；", " ",
	)
	v = replacer.Replace(v)
	return strings.Join(strings.Fields(v), " ")
}

func commonPrefixRunes(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	n := len(ar)
	if len(br) < n {
		n = len(br)
	}
	count := 0
	for i := 0; i < n; i++ {
		if ar[i] != br[i] {
			break
		}
		count++
	}
	return count
}

// formatTaskSnapshot renders a TaskSnapshot for the /tasks command.
func formatTaskSnapshot(s TaskSnapshot) string {
	var sb strings.Builder
	if s.PersistenceError != "" {
		fmt.Fprintf(&sb, "任务队列持久化失败: %s\n", s.PersistenceError)
	}
	if s.Paused {
		sb.WriteString("队列已暂停；/tasks run 启动待执行任务，/tasks retry 重试状态不明任务，/tasks skip 放弃该任务\n")
	}
	if s.Running {
		fmt.Fprintf(&sb, "当前任务 (已运行 %s):\n  %s\n", s.Elapsed.Truncate(time.Second), s.Current)
		if s.PendingSteer != "" {
			fmt.Fprintf(&sb, "待生效指引: %s\n", s.PendingSteer)
		}
	} else {
		sb.WriteString("当前没有运行中的任务\n")
	}
	if len(s.Queued) > 0 {
		fmt.Fprintf(&sb, "排队中 (%d):\n", len(s.Queued))
		for i, q := range s.Queued {
			fmt.Fprintf(&sb, "  %d. %s\n", i+1, q)
		}
	}
	if s.PendingRetry != "" {
		if s.Paused {
			fmt.Fprintf(&sb, "状态不明 (检查副作用后 /tasks retry 或 /tasks skip): %s\n", s.PendingRetry)
		} else {
			fmt.Fprintf(&sb, "可重试 (输入“继续”): %s\n", s.PendingRetry)
		}
	}
	return sb.String()
}

func newREPLTaskRunner(eng *engine.Engine) *replTaskRunner {
	r := &replTaskRunner{
		eng:   eng,
		queue: make([]api.Message, 0),
	}
	r.cond = sync.NewCond(&r.mu)
	r.initQueueStore()
	if eng != nil {
		eng.OnSteerConsumed = r.steerConsumed
	}
	return r
}

// steerConsumed runs on the task goroutine when the engine hands steered
// guidance to the model. The count shown on the pinned row is re-read from
// the engine rather than zeroed, since a line steered between the engine's
// drain and this call is still pending.
func (r *replTaskRunner) steerConsumed() {
	_, n := r.eng.PendingSteer()
	repl.SetSteerCount(n)
}

func (r *replTaskRunner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

func (r *replTaskRunner) CancelRunning() bool {
	r.mu.Lock()
	cancel := r.cancel
	running := r.running
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return running
}

// CancelForExit is CancelRunning for the exit path: guidance the cancelled
// task had not consumed is dropped instead of becoming a new task, and
// nothing queued starts, since the program is leaving.
func (r *replTaskRunner) CancelForExit() bool {
	r.mu.Lock()
	r.closing = true
	if !r.running {
		r.persistQueueLocked()
	}
	r.mu.Unlock()
	return r.CancelRunning()
}

// sessionID is the engine's session, which the interrupted draft records.
func (r *replTaskRunner) sessionID() string {
	if r.eng == nil {
		return ""
	}
	return r.eng.SessionID()
}

func (r *replTaskRunner) PendingFailed() *api.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pendingFailedMsg
}

func (r *replTaskRunner) ClearPendingFailed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.syncQueueScopeLocked() {
		return
	}
	r.pendingFailedMsg = nil
	r.persistQueueLocked()
}

// Enqueue hands msg to the runner: it starts at once when nothing is
// running, otherwise it joins the queue (merged into a similar queued task
// when there is one). It returns the number of queued tasks ahead of it and
// whether it was merged.
func (r *replTaskRunner) Enqueue(msg api.Message) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	queuedAhead, merged, _ := r.enqueueLocked(msg)
	if queuedAhead < 0 {
		repl.PrintAbove("[未接收] 原会话队列保存失败，请处理持久化错误后重新提交。\r\n")
	}
	return queuedAhead, merged
}

// EnqueueWithFeedback is Enqueue plus the line to show the user for msg (see
// enqueueFeedback): "" when it started right away, otherwise where it waits.
// The decision is made inside the lock: an idle runner starts msg before
// Enqueue returns, so asking IsRunning afterwards always answered "running"
// and the line called the message just typed queued behind itself.
func (r *replTaskRunner) EnqueueWithFeedback(msg api.Message) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enqueueQueueFeedbackLocked(msg)
}

// SubmitWithFeedback hands a typed message to the right place and returns
// the line to show for it. While a task runs, plain text is steered into that
// task (engine.Steer): the model sees it as guidance at its next call, the
// way a correction typed mid-task is meant. It used to be queued as a new
// task behind the running one, so "别改那个文件" only ran after the file had
// been changed. Idle, or with attachments (which cannot travel as guidance
// text), the message goes through Enqueue as before.
func (r *replTaskRunner) SubmitWithFeedback(msg api.Message) string {
	feedback, _ := r.submitWithFeedback(msg)
	return feedback
}

func (r *replTaskRunner) submitWithFeedback(msg api.Message) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running && r.eng != nil && len(msg.Parts) == 0 && strings.TrimSpace(msg.Content) != "" {
		r.eng.Steer(msg.Content)
		_, n := r.eng.PendingSteer()
		repl.SetSteerCount(n)
		return steerFeedback, true
	}
	return r.enqueueQueueFeedbackResultLocked(msg)
}

// reclaimSteerLocked moves guidance the finished task never consumed to the
// front of the queue as a task of its own, and reports whether it did. The
// user typed it for "now", so it goes ahead of anything queued earlier. It
// does nothing while the program is leaving (CancelForExit). Callers hold
// r.mu.
func (r *replTaskRunner) reclaimSteerLocked() bool {
	if r.eng == nil {
		return false
	}
	steer := r.eng.TakePendingSteer()
	repl.SetSteerCount(0)
	if r.closing || strings.TrimSpace(steer) == "" {
		return false
	}
	// On its own the guidance names nothing ("只要llama不要其他的" showed up
	// in /history as a task and a draft), so the new request says which task
	// it was meant for; r.current is still that task here.
	content := steer
	if prev := taskPreview(r.current); prev != "" {
		content = steer + "\n\n（这是在上一个任务「" + prev + "」运行期间补充的指引，该任务已结束，请在它的结果基础上继续。）"
	}
	r.queue = append([]api.Message{{Role: "user", Content: content}}, r.queue...)
	repl.SetQueuedCount(len(r.queue))
	return true
}

// enqueueLocked is Enqueue's body. wasRunning reports that a task was
// already running when msg arrived, so msg waits. Callers hold r.mu.
func (r *replTaskRunner) enqueueLocked(msg api.Message) (queuedAhead int, merged, wasRunning bool) {
	if !r.syncQueueScopeLocked() {
		return -1, false, false
	}
	defer func() { repl.SetQueuedCount(len(r.queue)) }()
	defer r.persistQueueLocked()
	wasRunning = r.running
	if r.running && len(r.queue) > 0 {
		for i := len(r.queue) - 1; i >= 0; i-- {
			if canMergeQueuedTask(r.queue[i], msg) {
				r.queue[i] = mergeQueuedTask(r.queue[i], msg)
				return i, true, true
			}
		}
	}

	r.queue = append(r.queue, msg)
	queueSize := len(r.queue)
	r.startNextLocked()
	if wasRunning {
		return queueSize - 1, false, true
	}
	return 0, false, false
}

func (r *replTaskRunner) WaitIdleUntil(deadline time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.running {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.AfterFunc(remaining, func() {
			r.mu.Lock()
			r.cond.Broadcast()
			r.mu.Unlock()
		})
		r.cond.Wait()
		timer.Stop()
	}
	return true
}

func (r *replTaskRunner) startNextLocked() {
	if r.running || r.closing || r.paused || len(r.queue) == 0 {
		return
	}
	msg := r.queue[0]
	r.queue = r.queue[1:]
	repl.SetQueuedCount(len(r.queue))
	r.running = true
	r.current = msg
	r.currentStart = time.Now()
	if !r.persistQueueLocked() {
		r.running = false
		r.current = api.Message{}
		r.currentStart = time.Time{}
		r.queue = append([]api.Message{msg}, r.queue...)
		repl.SetQueuedCount(len(r.queue))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel

	go r.run(ctx, msg)
}

func (r *replTaskRunner) run(ctx context.Context, userMsg api.Message) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Warnf("repl task panic: %v", recovered)
			r.mu.Lock()
			msgCopy := userMsg
			r.pendingFailedMsg = &msgCopy
			_ = saveInterruptedDraftFor(userMsg, fmt.Errorf("internal panic: %v", recovered), r.sessionID())
			// Reclaim before finishLocked, as afterRun does: the reclaimed
			// guidance names the task it was typed during (taskPreview of
			// r.current), which finishLocked clears.
			reclaimed := r.reclaimSteerLocked()
			r.finishLocked()
			repl.PrintAbove(fmt.Sprintf("\r\n%s任务执行出现内部异常，已恢复输入。可输入“继续”重试。%s\r\n", repl.Red, repl.Reset))
			if reclaimed {
				repl.PrintAbove(steerReclaimedFeedback + "\r\n")
			}
			r.startNextLocked()
			r.mu.Unlock()
		}
	}()

	// Before the model call: a process killed mid-task (crash, power cut,
	// kill -9) runs neither the error branch of afterRun nor the recover
	// above, and used to leave no draft at all. The session ID is known here:
	// the engine assigns it when the session starts, not at its first save.
	saveTaskStartDraft(userMsg, r.sessionID())
	_, reqErr := runChatInteractionMessage(ctx, r.eng, userMsg)
	r.afterRun(userMsg, reqErr)
}

// afterRun settles a finished task: the retry bookkeeping, what happens to
// guidance the task never consumed, and the next task.
//
// Guidance is kept pending when the task failed or was cancelled: the
// interrupted turn is what /continue resumes, and the guidance reaches the
// model at that turn's first call. Starting it as a new task here used to
// clear the interrupted turn and made the "/continue 可继续" hint printed a
// moment earlier a lie. After a normal completion there is nothing to
// continue, so the guidance runs as the next task.
func (r *replTaskRunner) afterRun(userMsg api.Message, reqErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if reqErr != nil {
		msgCopy := userMsg
		r.pendingFailedMsg = &msgCopy
		if isBudgetExceededError(reqErr) {
			// No draft for a budget stop, as before the task-start draft
			// existed: "继续" retries it once /budget allows, in this process.
			_ = clearInterruptedDraftFor(r.sessionID())
			repl.PrintAbove(budgetExceededRetryHint(r.eng.CostTracker()) + "\n")
		} else {
			_ = saveInterruptedDraftFor(userMsg, reqErr, r.sessionID())
			if hint := taskErrorHint(reqErr); hint != "" {
				repl.PrintAbove(hint + "\n")
			}
		}
		if r.eng != nil {
			if _, n := r.eng.PendingSteer(); n > 0 && !r.closing {
				repl.PrintAbove(steerKeptFeedback + "\r\n")
			}
		}
	} else {
		r.pendingFailedMsg = nil
		// Only this session's draft in this project: the draft of another
		// project, or of another session, is still unfinished.
		_ = clearInterruptedDraftFor(r.sessionID())
		// Guidance typed too late for this task (it completed before its
		// next model call) runs as the next task instead of vanishing.
		if r.reclaimSteerLocked() {
			repl.PrintAbove(steerReclaimedFeedback + "\r\n")
		}
	}
	r.finishLocked()
	r.startNextLocked()
}

func (r *replTaskRunner) finishLocked() {
	r.running = false
	if r.cancel != nil {
		r.cancel() // release the task's context; it used to leak one per task
	}
	r.cancel = nil
	r.current = api.Message{}
	r.currentStart = time.Time{}
	r.persistQueueLocked()
	r.cond.Broadcast()
}

// interruptedTaskHint follows a task that ended with an error: the engine
// kept its completed steps, and /continue resumes it from there ("继续"
// still works too).
const interruptedTaskHint = "输入 /continue 可从中断处继续刚才的任务。"

// contextOverflowHint follows a task whose request did not fit the model's
// context window even after the engine's compact-and-retry. The generic hint
// left the user guessing whether /continue could possibly work; it can,
// because the resumed turn compacts first, and when even that is not enough
// the model's window itself has to grow.
const contextOverflowHint = "上下文超出模型窗口。输入 /continue 会先压缩对话历史再重试；若仍失败，需要调大模型的上下文长度" +
	"（llama-server 加 -c 65536、LM Studio 的 Context Length、Ollama 的 num_ctx），cove 的系统提示词和工具定义本身约占 13K token。"

// taskErrorHint is the line shown after a task that ended with err. A turn
// stopped at its limit gets none: the stop line already says /continue
// resumes it (three hints in a row used to follow an "s").
func taskErrorHint(err error) string {
	var le *engine.LimitError
	if errors.As(err, &le) {
		return ""
	}
	hint := interruptedTaskHint
	if api.IsContextLengthError(err) {
		hint = contextOverflowHint
	}
	// The same E-number /diagnose errors shows, so the two can be matched up.
	if code, _ := diagnostic.Classify(err, diagnostic.Context{}); code != "" {
		hint = "[" + string(code) + "] " + hint
	}
	return hint
}

// minMergeRunes is the shortest normalised message that may be merged into
// a queued task by similarity rather than equality.
const minMergeRunes = 8
