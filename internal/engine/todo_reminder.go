package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/log"
)

// todoReminderRounds is how many tool rounds may pass without a todowrite
// call, while the task list still has open items, before the list is shown
// to the model again. The list otherwise reached the model once, as the
// todowrite result: many rounds later it was far back in the history (or
// masked, or summarised away) and long tasks lost track of their plan.
const todoReminderRounds = 8

// isTodoWrite reports whether a tool call name is todowrite (or its alias).
func isTodoWrite(name string) bool { return strings.EqualFold(name, "todowrite") }

// todoBlock wraps the task list for the model.
func todoBlock(intro, list string) string {
	return "<todo_list>\n" + intro + "\n" + list + "</todo_list>"
}

// todoRoundReminder returns, every todoReminderRounds tool rounds without a
// todowrite call, the open task list to append to the round's last tool
// result; "" otherwise. A round that calls todowrite restarts the count and
// marks the list as part of this turn's work (t.todoTouched).
//
// The list lives for the whole conversation, so a list an earlier task left
// open was shown as "your task list" while the model worked on an unrelated
// request. A list this turn has not touched is introduced as the earlier
// work it is, to be continued only if the latest request is part of it.
func (e *Engine) todoRoundReminder(t *turn, results []toolResult) string {
	for _, r := range results {
		if isTodoWrite(r.Name) && !r.Failed {
			t.todoTouched = true
			e.roundsSinceTodo = 0
			return ""
		}
	}
	if e.runtime == nil {
		return ""
	}
	list, open := e.runtime.TodoList()
	if open == 0 {
		e.roundsSinceTodo = 0
		return ""
	}
	e.roundsSinceTodo++
	if e.roundsSinceTodo < todoReminderRounds {
		return ""
	}
	e.roundsSinceTodo = 0
	if !t.todoTouched {
		return todoBlock("Reminder, the task list from earlier in this conversation (not updated during the current request). Continue it only if the latest request is part of that work; otherwise leave it as it is:", list)
	}
	return todoBlock("Reminder, your task list (update it with todowrite as items finish; do not stop while items are open unless you are blocked):", list)
}

// todoFinishNudge is, once per turn, the message sent back to a model that
// is ending the turn while its task list still has open items; "" when the
// list is done (or there is none), or when this turn never called todowrite.
// A list only counts as this turn's work once the turn wrote it: an
// unrelated request that followed a task with open items used to be told to
// carry on with them and report again in full, and its report came out
// covering the old task.
func (e *Engine) todoFinishNudge(t *turn) string {
	if t.todoChecked || !t.todoTouched || e.runtime == nil {
		return ""
	}
	list, open := e.runtime.TodoList()
	if open == 0 {
		return ""
	}
	t.todoChecked = true
	return todoBlock(fmt.Sprintf("[system: You are ending the turn with %d item(s) of your task list still open. If they are done, mark them completed with todowrite; if not, carry on with them. Then write your final report on the latest request to the user again, in full: only your last message is shown to them.]", open), list)
}

// todoWrittenSince reports whether msgs (the part of the history a resumed
// turn already ran) hold a todowrite call: a turn resumed after an
// interruption keeps the list as its own work.
func todoWrittenSince(msgs []api.Message) bool {
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if isTodoWrite(c.Name) {
				return true
			}
		}
	}
	return false
}

// fallbackModel is the configured model to move to when model is
// overloaded: the fast model for the main one and the main one for the fast
// one; "" when there is no other.
func (e *Engine) fallbackModel(model string) string {
	for _, m := range []string{e.config.ModelFast, e.config.Model} {
		if m != "" && m != model && (!e.hasImageMessages() || api.IsVisionCapableModel(m)) {
			return m
		}
	}
	return ""
}

// todoAfterCompaction appends the open task list to the first message of a
// compacted history (the summary): the summary may not carry it, and the
// todowrite result that did is gone.
//
// rewritten says the older history was really replaced (summarized, or cut
// by the truncation fallback). A compaction that only trimmed old tool
// results also reports Compressed, but keeps the history as it was: its
// first message is then the user's own request, and every such compaction
// used to append another copy of the list to it. A block this function
// appended before (known by its intro line) is replaced by the current list
// rather than joined by a second. Any other <todo_list> text is left alone:
// replacing from the first "<todo_list>" to the next "</todo_list>" could
// start in a block cut short by truncation and delete everything up to a
// later block's end.
func (e *Engine) todoAfterCompaction(rewritten bool) {
	if !rewritten || e.runtime == nil || len(e.messages) == 0 || e.messages[0].Role != "user" {
		return
	}
	list, open := e.runtime.TodoList()
	if open == 0 {
		return
	}
	content := e.messages[0].Content
	head := "<todo_list>\n" + compactionTodoIntro + "\n"
	if i := strings.LastIndex(content, head); i >= 0 {
		if j := strings.Index(content[i:], "</todo_list>"); j >= 0 {
			content = strings.TrimRight(content[:i], "\n") + content[i+j+len("</todo_list>"):]
		}
	}
	e.messages[0].Content = content + "\n\n" + todoBlock(compactionTodoIntro, list)
}

// compactionTodoIntro is the intro line of the block todoAfterCompaction
// appends; it identifies the block on a later compaction.
const compactionTodoIntro = "The task list at the time of compaction (keep it current with todowrite):"

// previousPlanMaxAge: a plan left unfinished longer ago than this is not
// offered to a new conversation any more.
const previousPlanMaxAge = 7 * 24 * time.Hour

// persistPlan saves the task list to the project's session notes after a
// todowrite call: the list while items are open (cleared once all are done)
// and each completed item as a task note, the progress log later sessions
// see. It is flushed at once; a crash mid-task must not lose the plan.
func (e *Engine) persistPlan() {
	if e.runtime == nil || e.sessionNotes == nil {
		return
	}
	list, open := e.runtime.TodoList()
	if open == 0 {
		list = ""
	}
	e.sessionNotes.SetPlan(list)
	for _, done := range e.runtime.CompletedTodos() {
		e.sessionNotes.AddTask("完成：" + done)
	}
	if err := e.sessionNotes.Flush(); err != nil {
		log.Warnf("session notes: %v", err)
	}
}

// previousPlanNote handles the plan an earlier session of this project left
// unfinished, on the first turn of a conversation that has no task list of
// its own. It goes to the model only when the user asks to continue
// (asksToContinue); otherwise the user is told it exists, once. It used to go
// to the model with any first message, and a model given "/context" (a
// command mangled by a byte-order mark) redid the old task.
func (e *Engine) previousPlanNote(query string) string {
	if e.sessionNotes == nil || e.runtime == nil {
		return ""
	}
	if list, _ := e.runtime.TodoList(); list != "" {
		return ""
	}
	plan, at := e.sessionNotes.Plan()
	if plan == "" || time.Since(at) > previousPlanMaxAge {
		return ""
	}
	if !asksToContinue(query) {
		if !e.planOffered {
			e.planOffered = true
			e.engineOutput(fmt.Sprintf("  \x1b[2m上次会话（%s）留下了未完成的计划（%d 项未完成）；要接着做，回复「继续上次的计划」\x1b[0m",
				at.Format("01-02 15:04"), strings.Count(plan, "[ ]")+strings.Count(plan, "[>]")))
		}
		return ""
	}
	e.planOffered = true
	return "<previous_plan>\nThe user asked to continue; earlier work in this project (an earlier task or session) left this task list unfinished (saved " + at.Format("2006-01-02 15:04") +
		"). Check the current state of the code, restore the list with todowrite and carry on from the open items.\n" +
		plan + "\n</previous_plan>"
}

// continueWords are what a message asking to pick earlier work up says.
var continueWords = []string{"继续", "接着", "上次", "未完成", "之前的计划", "continue", "resume", "pick up", "previous plan", "last plan"}

// asksToContinue reports whether query asks to continue earlier work.
func asksToContinue(query string) bool {
	q := strings.ToLower(query)
	for _, w := range continueWords {
		if strings.Contains(q, w) {
			return true
		}
	}
	return false
}

// restoreTodos rebuilds the task list from the last todowrite call in msgs,
// so a resumed session keeps its plan (the list lives in memory only). A
// history without one (a new session, a rewind to before the plan) has no
// list.
func (e *Engine) restoreTodos(msgs []api.Message) {
	if e.runtime == nil {
		return
	}
	e.runtime.ClearTodos()
	for i := len(msgs) - 1; i >= 0; i-- {
		calls := msgs[i].ToolCalls
		for j := len(calls) - 1; j >= 0; j-- {
			if !isTodoWrite(calls[j].Name) {
				continue
			}
			if todos, ok := calls[j].Input["todos"].([]any); ok {
				e.runtime.SetTodos(todos)
				return
			}
		}
	}
}
