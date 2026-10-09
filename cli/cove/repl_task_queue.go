package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

func (r *replTaskRunner) initQueueStore() {
	if r.eng == nil {
		return
	}
	dir, err := config.ConfigDir()
	if err != nil {
		r.persistenceError = err.Error()
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		r.persistenceError = err.Error()
		return
	}
	r.queueStore = session.NewQueueStore(filepath.Join(dir, "task-queues"))
	r.queueCwd = session.NormalizeProjectDir(cwd)
	r.queueSession = r.sessionID()
	r.queueID = rand.Text()
}

func (r *replTaskRunner) enqueueQueueFeedbackLocked(msg api.Message) string {
	feedback, _ := r.enqueueQueueFeedbackResultLocked(msg)
	return feedback
}

func (r *replTaskRunner) enqueueQueueFeedbackResultLocked(msg api.Message) (string, bool) {
	ahead, merged, running := r.enqueueLocked(msg)
	if ahead < 0 {
		return "[未接收] 原会话队列保存失败，请处理持久化错误后重新提交。", false
	}
	if r.paused {
		return "[已排队] 队列已暂停，输入 /tasks 查看并确认启动。", true
	}
	return enqueueFeedback(ahead, merged, running), true
}

func (r *replTaskRunner) persistQueueLocked() bool {
	if r.queueStore == nil {
		if r.persistenceError != "" {
			r.paused = true
			return false
		}
		return true
	}
	snapshot := session.QueueSnapshot{ID: r.queueID, SessionID: r.queueSession, Cwd: r.queueCwd, OwnerPID: os.Getpid(), Pending: r.queue}
	if r.running {
		current := r.current
		snapshot.Current = &current
	} else {
		snapshot.Current = r.pendingFailedMsg
		if r.closing || r.queueSession != r.sessionID() {
			snapshot.OwnerPID = 0
		}
	}
	var err error
	if snapshot.Current == nil && len(snapshot.Pending) == 0 {
		err = r.queueStore.Delete(r.queueID)
	} else {
		err = r.queueStore.Save(snapshot)
	}
	if err != nil {
		if r.persistenceError != err.Error() {
			repl.PrintAbove("任务队列持久化失败，队列已暂停: " + err.Error() + "\r\n")
		}
		r.persistenceError = err.Error()
		r.paused = true
		return false
	}
	r.persistenceError = ""
	return true
}

func (r *replTaskRunner) syncQueueScopeLocked() bool {
	if r.running || r.queueStore == nil || r.queueSession == r.sessionID() {
		return true
	}
	if !r.persistQueueLocked() {
		return false
	}
	r.queue = nil
	r.pendingFailedMsg = nil
	r.paused = false
	r.queueID = rand.Text()
	r.queueSession = r.sessionID()
	repl.SetQueuedCount(0)
	return true
}

func (r *replTaskRunner) savedQueues() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.queueStore == nil {
		return "队列存储不可用: " + r.persistenceError
	}
	snapshots, err := r.queueStore.List(r.queueCwd)
	var text strings.Builder
	if err != nil {
		fmt.Fprintf(&text, "部分队列快照读取失败: %v\n", err)
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == r.queueID {
			continue
		}
		state := "可恢复"
		if snapshot.OwnerRunning() {
			state = "进程仍在运行，不可接管"
		}
		fmt.Fprintf(&text, "%s  会话 %s  待执行 %d  %s", snapshot.ID, snapshot.SessionID, len(snapshot.Pending), state)
		if snapshot.Current != nil {
			fmt.Fprintf(&text, "  状态不明: %s", taskPreview(*snapshot.Current))
		}
		text.WriteByte('\n')
	}
	if text.Len() == 0 {
		return "当前项目没有其他可恢复队列。"
	}
	text.WriteString("先 /resume <会话ID>，再 /tasks restore <队列ID>；恢复不会自动执行。")
	return text.String()
}

func (r *replTaskRunner) queueRecoveryNotice() string {
	if r.queueStore == nil {
		return "任务队列存储不可用: " + r.persistenceError
	}
	snapshots, err := r.queueStore.List(r.queueCwd)
	if err != nil {
		return "部分队列快照无法读取；/tasks saved 查看详情: " + err.Error()
	}
	count := 0
	for _, snapshot := range snapshots {
		if snapshot.ID != r.queueID && !snapshot.OwnerRunning() {
			count++
		}
	}
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("发现 %d 个未完成任务队列；/tasks saved 查看并显式恢复，不会自动执行。", count)
}

func (r *replTaskRunner) taskQueueCommand(args []string) string {
	if len(args) == 0 {
		return strings.TrimRight(formatTaskSnapshot(r.Snapshot()), "\r\n")
	}
	if len(args) == 1 && args[0] == "saved" {
		return r.savedQueues()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	usage := "用法: /tasks [saved | restore <队列ID> | remove <序号> | move <序号> <目标序号> | run | retry | skip]"
	if r.closing {
		return "正在退出，不能修改队列。"
	}
	if r.queueStore != nil && r.queueSession != r.sessionID() && args[0] != "restore" {
		return "队列属于之前的会话，请切回原会话，或从 /tasks saved 恢复。"
	}
	switch args[0] {
	case "restore":
		if len(args) != 2 {
			return usage
		}
		if !r.syncQueueScopeLocked() {
			return "原会话队列保存失败，不能切换队列: " + r.persistenceError
		}
		if r.running || len(r.queue) > 0 || r.pendingFailedMsg != nil {
			return "请先处理当前队列，再恢复其他队列。"
		}
		if r.queueStore == nil {
			return "队列存储不可用。"
		}
		snapshot, err := r.queueStore.Claim(args[1], r.queueCwd, r.sessionID())
		if err != nil {
			return "恢复失败: " + err.Error()
		}
		r.queueID, r.queueSession = snapshot.ID, snapshot.SessionID
		r.pendingFailedMsg, r.queue = snapshot.Current, snapshot.Pending
		r.paused = true
		repl.SetQueuedCount(len(r.queue))
		return "队列已恢复并暂停。状态不明的任务可能已产生副作用；先 /tasks retry 或 /tasks skip，再 /tasks run。"
	case "remove", "move":
		want := 2
		if args[0] == "move" {
			want = 3
		}
		if len(args) != want {
			return usage
		}
		from, err := strconv.Atoi(args[1])
		if err != nil || from < 1 || from > len(r.queue) {
			return "任务序号超出待执行队列范围。"
		}
		to := 0
		if want == 3 {
			to, err = strconv.Atoi(args[2])
			if err != nil || to < 1 || to > len(r.queue) {
				return "目标序号超出待执行队列范围。"
			}
		}
		msg := r.queue[from-1]
		r.queue = append(r.queue[:from-1], r.queue[from:]...)
		if want == 3 {
			r.queue = append(r.queue, api.Message{})
			copy(r.queue[to:], r.queue[to-1:])
			r.queue[to-1] = msg
		}
		repl.SetQueuedCount(len(r.queue))
	case "skip":
		if len(args) != 1 || r.running || !r.paused {
			return usage
		}
		r.pendingFailedMsg = nil
		_ = clearInterruptedDraftFor(r.sessionID())
	case "run", "retry":
		if len(args) != 1 || r.running || !r.paused {
			return "只有已暂停、未运行的队列可以确认启动。"
		}
		if args[0] == "retry" {
			if r.pendingFailedMsg == nil {
				return "没有状态不明的任务；使用 /tasks run 启动待执行队列。"
			}
			r.queue = append([]api.Message{*r.pendingFailedMsg}, r.queue...)
			r.pendingFailedMsg = nil
		} else if r.pendingFailedMsg != nil {
			return "请先 /tasks retry 或 /tasks skip 处理状态不明的任务。"
		}
		if len(r.queue) == 0 {
			return "没有待执行任务。"
		}
		if !r.persistQueueLocked() {
			return "持久化失败，队列保持暂停: " + r.persistenceError
		}
		r.paused = false
		r.startNextLocked()
		if r.paused {
			return "持久化失败，队列保持暂停: " + r.persistenceError
		}
		return "已确认启动队列。"
	default:
		return usage
	}
	if !r.persistQueueLocked() {
		return "修改尚未持久化，队列已暂停: " + r.persistenceError
	}
	return "队列已更新；/tasks 查看当前状态。"
}
