package plan

import (
	"context"
	"fmt"
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/delegate"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// MaxParallelAgents is the default number of sub-agents that run concurrently
// when the caller did not set max_agents (tool.WithMaxAgents).
const MaxParallelAgents = 4

// DefaultMaxRetries is how many times the supervisor re-dispatches a failed
// task (feeding the failure reason back into the next attempt).
const DefaultMaxRetries = 1

// ExecutionResult holds the outcome of a plan execution.
type ExecutionResult struct {
	PlanID  string
	Tasks   []*Task
	Success bool
}

// PlanExecutor executes a Plan using delegate.SubAgent for each task.
// Tasks are scheduled respecting dependencies — independent tasks
// (those at the same BFS depth level) may run concurrently.
type PlanExecutor struct {
	delegator  *delegate.Delegator
	runtime    *tool.Runtime
	maxRetries int
	// onTaskContext, when set, sees the context each task's sub-agent runs
	// with (tests read the write-claim table through it).
	onTaskContext func(context.Context)
}

// NewPlanExecutor creates a PlanExecutor backed by the given Delegator.
func NewPlanExecutor(d *delegate.Delegator, rt *tool.Runtime) *PlanExecutor {
	return &PlanExecutor{
		delegator:  d,
		runtime:    rt,
		maxRetries: DefaultMaxRetries,
	}
}

// SetMaxRetries configures how many times the supervisor retries a failed task.
func (pe *PlanExecutor) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	pe.maxRetries = n
}

// Execute runs all tasks in the plan.
func (pe *PlanExecutor) Execute(ctx context.Context, plan *Plan) *ExecutionResult {
	taskByID := make(map[string]*Task)
	for _, t := range plan.Tasks {
		taskByID[t.ID] = t
	}

	levels := topologicalSort(plan.Tasks, taskByID)
	if levels == nil {
		// Cycle detected in dependencies
		for _, t := range plan.Tasks {
			t.Status = "failed"
			t.Error = "circular dependency detected"
		}
		return &ExecutionResult{
			PlanID:  plan.ID,
			Tasks:   plan.Tasks,
			Success: false,
		}
	}

	completed := make(map[string]bool)
	allSuccess := true

	for _, level := range levels {
		// Mark tasks whose dependencies failed (or were themselves skipped)
		// BEFORE the level runs, and drop them from the runnable set.
		//
		// This used to be done after the level completed, which never worked:
		// the next level's runTask sets Status = "running" as its first
		// statement, overwriting "skipped", so the downstream task executed
		// anyway — with an empty depOutputs, i.e. without the very context it
		// declared a dependency on. Filtering up front also propagates the skip
		// transitively, since a task skipped here blocks its own dependents.
		runnable := make([]*Task, 0, len(level))
		for _, t := range level {
			// Once the user has cancelled, nothing more is dispatched, and
			// what did not run stays pending so a later execute_plan resumes
			// it. It used to run on into failed/skipped, which left nothing
			// for "continue" to pick up.
			if ctx.Err() != nil {
				pe.markCancelled(t)
				allSuccess = false
				continue
			}
			if blocker := blockingDep(t, taskByID); blocker != "" {
				t.Status = "skipped"
				t.Error = fmt.Sprintf("dependency %q did not succeed", blocker)
				pe.markRuntimeStatus(t.ID, "skipped")
				completed[t.ID] = true
				allSuccess = false
				continue
			}
			runnable = append(runnable, t)
		}
		level = runnable
		if len(level) == 0 {
			continue
		}

		if plan.Parallel && len(level) > 1 {
			// Execute level concurrently
			var wg sync.WaitGroup
			// execute_plan's max_agents (1-8) travels in the context.
			sem := make(chan struct{}, tool.MaxAgentsFrom(ctx, MaxParallelAgents))
			// Sibling tasks share one working tree: the engine's sub-agent
			// executor claims each written path through this table and
			// refuses a second task's write instead of letting it win.
			claims := NewWriteClaims()
			results := make([]struct {
				task    *Task
				success bool
			}, len(level))

			for i, t := range level {
				wg.Add(1)
				sem <- struct{}{}
				go func(idx int, task *Task) {
					defer wg.Done()
					defer func() { <-sem }()
					success := pe.runTask(WithTaskClaims(ctx, task.ID, claims), task, completed)
					results[idx] = struct {
						task    *Task
						success bool
					}{task, success}
				}(i, t)
			}
			wg.Wait()

			for _, r := range results {
				completed[r.task.ID] = true
				if !r.success {
					allSuccess = false
				}
			}
		} else {
			// Serial execution
			for _, t := range level {
				success := pe.runTask(ctx, t, completed)
				completed[t.ID] = true
				if !success {
					allSuccess = false
				}
			}
		}

	}

	return &ExecutionResult{
		PlanID:  plan.ID,
		Tasks:   plan.Tasks,
		Success: allSuccess,
	}
}

// blockingDep returns the ID of the first dependency of task that did not
// succeed ("failed" or "skipped"), or "" when every dependency is clear. An
// unknown dependency ID is treated as clear — topologicalSort has already
// validated the graph, and a task should not be silently dropped over a typo
// that the planner accepted.
func blockingDep(task *Task, taskByID map[string]*Task) string {
	for _, depID := range task.DependsOn {
		dep, ok := taskByID[depID]
		if !ok {
			continue
		}
		if dep.Status == "failed" || dep.Status == "skipped" {
			return depID
		}
	}
	return ""
}

// markRuntimeStatus mirrors a task's status into the shared runtime state.
func (pe *PlanExecutor) markRuntimeStatus(taskID, status string) {
	if pe.runtime == nil {
		return
	}
	pe.runtime.Lock()
	defer pe.runtime.Unlock()
	if tr, ok := pe.runtime.Tasks[taskID]; ok {
		tr.Status = status
	}
}

// runTask executes a single task via delegate.SubAgent, with supervisor retry.
func (pe *PlanExecutor) runTask(ctx context.Context, task *Task, completed map[string]bool) (success bool) {
	// A task already ruled out (a dependency failed) must never be revived
	// here; Execute filters those out, and this is the backstop.
	if task.Status == "skipped" {
		return false
	}
	task.Status = "running"

	// Update the runtime task state
	pe.runtime.Lock()
	parentID := ""
	if tr, ok := pe.runtime.Tasks[task.ID]; ok {
		tr.Status = "running"
		parentID = tr.ParentID
	}
	pe.runtime.Unlock()

	// Get context from completed dependencies
	var depOutputs []string
	for _, depID := range task.DependsOn {
		pe.runtime.Lock()
		if tr, ok := pe.runtime.Tasks[depID]; ok && tr.Output != "" {
			depOutputs = append(depOutputs, fmt.Sprintf("[%s output]: %s", depID, tr.Output))
		}
		pe.runtime.Unlock()
	}

	// Actively deliver any messages addressed to this task, its team (parent),
	// or broadcast ("all"). Delivered messages are marked so they are injected
	// once and surface in the agent's prompt instead of being passively logged.
	messages := pe.takeMessagesFor(task.ID, parentID)

	basePrompt := task.Description
	for _, dep := range depOutputs {
		basePrompt += "\n" + dep
	}
	for _, m := range messages {
		basePrompt += "\n[收到消息] " + m
	}

	systemPrompt := "You are a task execution agent. Complete the assigned task efficiently. " +
		"Use available tools to read, write, and modify files. " +
		"Report your results concisely. Do not ask for confirmation — just do the task."

	if pe.onTaskContext != nil {
		pe.onTaskContext(ctx)
	}
	prompt := basePrompt
	var lastErr, lastOutput string
	for attempt := 0; attempt <= pe.maxRetries; attempt++ {
		if attempt > 0 {
			// Supervisor re-dispatch: feed the prior failure back in.
			prompt = basePrompt + fmt.Sprintf(
				"\n\n[上一次尝试失败 (%d/%d)] 原因: %s\n请修正问题后重试。",
				attempt, pe.maxRetries, lastErr)
		}

		result := pe.delegator.Delegate(ctx, task.ID, prompt, systemPrompt)

		if result != nil {
			task.ExitReason = result.ExitReason
		}
		if result == nil {
			lastErr = "delegator returned nil result"
		} else if result.Error != "" {
			lastErr = result.Error
			lastOutput = result.Output
			if result.CapReached || result.Truncated {
				// Re-running the task from scratch would redo the same work
				// and hit the same cap (or the same 5-minute deadline);
				// keep what was done instead.
				break
			}
		} else if !result.Success {
			lastErr = "task did not complete successfully"
		} else {
			task.Status = "done"
			task.Output = result.Output
			task.Error = ""
			pe.syncRuntimeTask(task)
			return true
		}

		// Don't retry if the context was cancelled.
		if ctx.Err() != nil {
			break
		}
	}

	if ctx.Err() != nil {
		// Interrupted, not failed: keep it resumable.
		pe.markCancelled(task)
		return false
	}
	task.Status = "failed"
	task.Error = lastErr
	// A sub-agent stopped at its cap hands back what it did; losing it made
	// the model redo the whole task.
	task.Output = lastOutput
	pe.syncRuntimeTask(task)
	return false
}

// takeMessagesFor returns and marks-as-delivered the pending messages addressed
// to the given task ID, its team (parentID), or broadcast targets.
func (pe *PlanExecutor) takeMessagesFor(taskID, parentID string) []string {
	pe.runtime.Lock()
	defer pe.runtime.Unlock()
	var out []string
	for i := range pe.runtime.Messages {
		m := &pe.runtime.Messages[i]
		if m.Delivered {
			continue
		}
		if m.To == taskID || m.To == "all" || (parentID != "" && m.To == parentID) {
			out = append(out, m.Message)
			m.Delivered = true
		}
	}
	return out
}

// markCancelled records a task the user's cancellation stopped (or kept from
// starting). The result says "cancelled"; the runtime says "pending", which is
// what FromRuntime picks up, so running the plan again resumes it.
func (pe *PlanExecutor) markCancelled(task *Task) {
	task.Status = "cancelled"
	task.Error = "cancelled by user"
	pe.markRuntimeStatus(task.ID, "pending")
}

// syncRuntimeTask copies the task status/output back into the shared runtime.
func (pe *PlanExecutor) syncRuntimeTask(task *Task) {
	pe.runtime.Lock()
	if tr, ok := pe.runtime.Tasks[task.ID]; ok {
		tr.Status = task.Status
		tr.Output = task.Output
	}
	pe.runtime.Unlock()
}
