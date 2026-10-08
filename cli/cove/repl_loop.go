package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/plugin"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// replInteractive 为 true 时表示处于交互式 REPL，主循环会经 TakePermInputCh
// 转发权限确认输入。默认 false：-p 一次性模式不安装授权处理器，需要授权的
// 工具由引擎以 fail-closed 方式拒绝（见 Engine.authorizeTool）。
var replInteractive bool

// runREPL runs the interactive shell until the user leaves it, and reports
// whether they left with /restart (main then starts cove again).
func runREPL(app *appBootstrap, cmdReg *command.Registry, bannerText string) (restart bool) {
	eng, toolReg, cfg, mcpPool := app.eng, app.toolReg, app.cfg, app.mcpPool
	skillMgr, memStore, pluginMgr, projCtx := app.skillMgr, app.memStore, app.pluginMgr, app.projCtx

	replInteractive = true

	// The loop below relays answer lines to a waiting prompt via
	// repl.TakePermInputCh, so tools that need approval can ask instead of being
	// denied by the engine's "no interactive approval handler" path.
	installPermissionPrompt(eng)
	// The question tool asks through the same relay.
	installQuestionPrompt(eng)
	// The same relay answers the prompt shown when a turn reaches its
	// iteration cap, its time limit or looks stuck.
	installLimitPrompt(eng)
	installBackgroundSummary(eng, term.IsTerminal(os.Stdout.Fd()))

	tasks := newREPLTaskRunner(eng)
	fe := &frontend{eng: eng, cfg: cfg, toolReg: toolReg, mcpPool: mcpPool, skillMgr: skillMgr,
		memStore: memStore, pluginMgr: pluginMgr, projCtx: projCtx, tasks: tasks,
		print:   func(s string) { repl.PrintAbove(s + "\r\n") },
		enqueue: func(msg api.Message) { tasks.Enqueue(msg) },
	}
	cmdReg = fe.install(cmdReg)
	defer fe.closeWorkflows()
	fe.installRemotePermissionPrompt()
	defer fe.stopRemote(context.Background())
	defer fe.closeRemoteOwner()

	allCommands := buildCommandList(cmdReg, toolReg)

	for name, c := range pluginMgr.CommandPrompts() {

		allCommands = append(allCommands, cmdEntry{Name: "/" + name, Desc: c.Description, Type: "cmd"})

	}

	// Build skill description map (short descriptions, not full prompts)

	skillDescs := buildSkillDescs(skillMgr)

	// Typed lines survive a restart (Up, Ctrl+R).
	if dir, err := config.ConfigDir(); err == nil {
		repl.SetHistoryFile(filepath.Join(dir, "input_history.jsonl"))
	}
	repl.SetHistoryFilter(historyLineKeeps)
	reader := repl.New(func(input string) []string {

		return complete(input, allCommands, skillDescs)

	})
	configureRemoteReader := func(reader *repl.LineReader) {
		reader.SetOwnerEventHook(fe.remoteWake, func() {
			fe.pollRemote()
			if tasks.IsRunning() {
				reader.SetPrompt(repl.PromptRunning())
			} else {
				reader.SetPrompt(repl.Prompt())
			}
		})
	}
	configureRemoteReader(reader)

	// Everything that writes to the terminal now goes through the editor, so
	// output lands above the input line instead of on top of it. That covers
	// the ~140 termui call sites, internal/log (whose default writer is
	// os.Stderr, i.e. straight at the cursor) and the engine's own diagnostics.
	termui.SetConsole(reader)
	defer termui.SetConsole(nil)
	log.SetWriter(replLogWriter{})
	defer log.SetWriter(os.Stderr)

	// Print the banner directly to the terminal (inline rendering).
	outp(bannerText)

	// Ctrl+C while a task is running:

	// During reader.ReadLine() the terminal is in raw mode, so Ctrl+C arrives as

	// a literal 0x03 byte and is handled as ErrInterrupt below. But while a task

	// runs, the main loop blocks in tasks.WaitIdleUntil() with the terminal restored

	// to cooked mode, so Ctrl+C is delivered as a SIGINT signal instead. Without

	// a handler the Go runtime's default action terminates the whole process —

	// the reported "提示按 Ctrl+C 可中断，实际却直接退出" bug. Install a persistent

	// handler that cancels the running task on SIGINT instead of killing the

	// program. (SIGTERM is intentionally left to the default action so external

	// `kill` still stops the program.)

	taskSigCh := make(chan os.Signal, 1)

	signal.Notify(taskSigCh, syscall.SIGINT)

	// Stop delivers no further signals once it returns, so closing the
	// channel ends the handler goroutine; it used to outlive the loop.
	defer func() {
		signal.Stop(taskSigCh)
		close(taskSigCh)
	}()

	go func() {

		for range taskSigCh {

			// In raw-mode ReadLine no SIGINT is generated (Ctrl+C is read as a

			// byte), so this only fires during task execution / cooked mode.

			// A prompt waiting on its answer channel does not see the
			// cancelled context; answer it too, or the next typed line is
			// taken as its answer.
			denyPendingPermissionPrompt()
			if tasks.CancelRunning() {

				repl.PrintAbove(fmt.Sprintf("\r\n%s[已中断] 正在停止当前任务…输入 /continue 可继续%s\r\n", repl.Yellow, repl.Reset))

			}

		}

	}()

	// On startup, check for interrupted draft and notify user
	if notice := tasks.queueRecoveryNotice(); notice != "" {
		repl.PrintAbove(notice + "\r\n")
	}

	if draft := usableInterruptedDraft(eng); draft != nil {

		age := time.Since(draft.UpdatedAt).Truncate(time.Second)

		repl.PrintAbove(fmt.Sprintf("您有一个未完成的片段草稿 (创建于 %v 前)。输入\u300e继续\u300f恢复，或直接输入新指令忽略。\r\n", age))

		repl.PrintAbove("  \x1b[2m提示: 使用 /history 可查看全部可恢复的历史会话\x1b[0m\r\n\r\n")

	}

	for {
		// Dynamic prompt: show ⚡ when a background task is running.
		if tasks.IsRunning() {
			reader.SetPrompt(repl.PromptRunning())
		} else {
			reader.SetPrompt(repl.Prompt())
		}

		input, err := reader.ReadLine()

		if errors.Is(err, repl.ErrInterrupt) {
			fe.revokeRemoteApproval()

			denyPendingPermissionPrompt()

			if tasks.IsRunning() {

				if tasks.CancelRunning() {

					repl.PrintAbove("[系统] 等待任务停止...\r\n")

				}

				continue

			}

			repl.PrintAbove("输入 /exit 或按 Ctrl+D 退出。\r\n")

			continue

		}

		// Esc on an empty line interrupts the running task, like Ctrl+C;
		// with nothing running it does nothing.
		if errors.Is(err, repl.ErrEscape) {
			if tasks.IsRunning() {
				fe.revokeRemoteApproval()
				denyPendingPermissionPrompt()
				if tasks.CancelRunning() {
					repl.PrintAbove(fmt.Sprintf("%s[已中断] 正在停止当前任务…输入 /continue 可继续%s\r\n", repl.Yellow, repl.Reset))
				}
			}
			continue
		}

		if errors.Is(err, repl.ErrExit) {

			// Ctrl+D / stdin EOF leaves like /exit. It used to save at once
			// while the task kept running: half a turn saved, the save racing
			// the task's appends, and the task then killed with no draft.
			fe.closeRemoteOwner()
			leaveREPL(tasks, func() { autoSaveSession(eng) })

			repl.PrintAbove("再见！\r\n")

			return

		}

		if err != nil {

			repl.PrintSafe("\r\n终端读取错误，正在重新初始化 REPL...\r\n")

			reader = repl.New(func(input string) []string {

				return complete(input, allCommands, skillDescs)

			})
			configureRemoteReader(reader)

			continue

		}

		// If a permission prompt is waiting for an answer, route this line to it

		// instead of processing it as a task.

		if ch, state, hint := repl.TakePromptInputFor(input); state != repl.PromptNone {
			if state == repl.PromptAnswer {
				ch <- input
				continue
			}
			// Not an answer: the prompt keeps waiting. An empty line just
			// repeats the hint; anything else is the next instruction and is
			// handled as usual (steered into the task or run as a command).
			if strings.TrimSpace(input) == "" {
				repl.PrintAbove("  \x1b[2m" + hint + "\x1b[0m\r\n")
				continue
			}
			repl.PrintAbove("  \x1b[2m提示仍在等待回答（" + hint + "）；这一行按普通输入处理\x1b[0m\r\n")
		}

		// The editor hands the line over as typed; every exact-match handler
		// below ("exit", "/history", "/compact"...) compared the raw text, so
		// a trailing space (Tab completion leaves one) made "/history " try
		// to resume a session named "", "/compact " fall through to the
		// generic command that only says to use the REPL's, and a line of
		// spaces went to the model as an empty message — a paid call, and a
		// failed one on providers that reject empty content. Headless trims
		// its lines; the shell does the same now.
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		if fe.takeHistoryPick(input, func(in string) {
			before := eng.SessionID()
			handleHistoryResume(in, eng)
			fe.noteSessionSwitch(before)
		}) {
			continue
		}

		switch {

		case input == "exit" || input == "/exit":

			fe.closeRemoteOwner()
			leaveREPL(tasks, func() { autoSaveSession(eng) })

			repl.PrintAbove("再见！\r\n")

			return

		// While a task runs, a bare "继续" is ordinary input (steered into the
		// task, or queued if it is finishing), like any other line. It used to
		// be refused with "当前有任务正在运行" — also in the moment after the
		// answer had been shown and the turn was only saving and cleaning up,
		// which is exactly when a person types it.
		case isContinueCommand(input) && !tasks.IsRunning():
			fe.historyPickPending = false
			fe.continueTyped(input)
			continue

		case input == "/":
			showQuickCommands(allCommands)
			continue

		case fe.dispatch(input):
			if fe.exitRequested {
				fe.closeRemoteOwner()
				leaveREPL(tasks, func() { autoSaveSession(eng) })
				if fe.restartRequested {
					repl.PrintAbove("正在重启 cove…\r\n")
					return true
				}
				repl.PrintAbove("再见！\r\n")
				return false
			}
			continue

		default:

			pc := cfg.EffectiveProvider()

			if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {

				repl.PrintAbove(budgetExceededRetryHint(eng.CostTracker()) + "\r\n")

				continue

			}

			// The same test as -p and headless: a --replay run needs no key.
			if runNeedsAPIKey(pc.APIKey, replayDir != "") {

				repl.PrintAbove(missingAPIKeyMessage(pc.Name) + "\r\n")

				continue

			}

			cwd, _ := os.Getwd()

			userMsg, warnings, err := buildUserMessage(input, cwd, fe.attachedFiles, cfg.Model)

			if err != nil {

				outf("警告: 构建用户消息时出错: %v\n", err)

				continue

			}

			if shouldAutoSwitchToVision(warnings) {

				if visionModel := preferredVisionModelForProvider(pc.Name, cfg.Model); visionModel != "" && visionModel != cfg.Model {

					if tasks.IsRunning() {
						// Switching reloads the provider under the running
						// turn (ReloadProvider mid-request, a data race the
						// "must not run while a task runs" rule keeps /model
						// out of). Sending the message anyway would queue an
						// image the current model cannot see, degraded to a
						// text note. So neither: the message is not sent,
						// its attachments stay mounted, and Up recalls the
						// line once the task has ended.
						fe.print(visionSwitchDeferredNotice(visionModel))
						continue
					}

					if err := applyProviderConfigChange(cfg, eng, func() error {

						cfg.Model = visionModel

						return nil

					}); err == nil {

						outf("[视觉] 检测到图片附件，已自动切换到视觉模型 %s。\n", visionModel)

						userMsg, warnings, err = buildUserMessage(input, cwd, fe.attachedFiles, cfg.Model)

						if err != nil {

							outf("警告: 构建用户消息时出错: %v\n", err)

							continue

						}

					}

				}

			}

			// Print warnings (e.g., non-vision model with image)

			for _, w := range warnings {

				repl.PrintAbove(fmt.Sprintf("  \x1b[33m%s\x1b[0m\r\n", w))

			}

			// Auto-clear attachments after sending (avoids resending images every turn)

			if len(fe.attachedFiles) > 0 {

				fe.attachedFiles = nil

			}

			// The same request sent again into a fresh session (after a
			// restart, typically) continues the unfinished session it started,
			// instead of opening one more copy of it in /history.
			// Not after /new: the person asked for a fresh conversation.
			startedByNew := fe.freshFromNew
			fe.freshFromNew = false
			if len(eng.Messages()) == 0 && !tasks.IsRunning() && len(userMsg.Parts) == 0 && !startedByNew {
				before := eng.SessionID()
				if rec, idx := resumeDuplicateSession(eng, userMsg.Content); rec != nil {
					fe.noteSessionSwitch(before)
					label := "会话"
					if idx > 0 {
						label = fmt.Sprintf("会话 #%d", idx)
					}
					repl.PrintAbove(fmt.Sprintf("[已恢复] 这条请求与%s（%s，%d 条消息）相同且该会话未完成，已在它上面继续，不再新建会话。\r\n", label, rec.UpdatedAt.Format("01-02 15:04"), len(rec.Messages)))
					userMsg = api.Message{Role: "user", Content: "继续"}
				}
			}
			// A request naming a directory outside the working directory
			// cannot be done with the file tools; say so before the model
			// spends an hour finding out.
			if hint := outsideCwdHint(userMsg.Content, currentProjectDir()); hint != "" {
				repl.PrintAbove(hint + "\r\n")
			}
			if msg := tasks.SubmitWithFeedback(userMsg); msg != "" {
				repl.PrintAbove(msg + "\r\n")
			}

			// Don't block: tasks run in the background, user can type again
			// immediately; typed while a task runs, the text steers that task.

		}

	}

}

// historyLineKeeps is the input history's filter (repl.SetHistoryFilter): a
// line that sets a credential is kept neither in memory (Up, Ctrl+R) nor in
// input_history.jsonl. Every line used to be recorded, "/api-key sk-..."
// included, in plain text. Refused: /api-key with a value, /config setting
// a key whose name says it is secret (api_key, token, secret, password),
// and /base-url with a user:password@ in the URL.
func historyLineKeeps(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return true
	}
	switch strings.ToLower(fields[0]) {
	case "/api-key":
		return false
	case "/config":
		key := strings.ToLower(fields[1])
		for _, s := range []string{"api_key", "api-key", "apikey", "token", "secret", "password"} {
			if strings.Contains(key, s) {
				return false
			}
		}
	case "/base-url":
		if u, err := url.Parse(fields[1]); err == nil && u.User != nil {
			return false
		}
	}
	return true
}

// visionSwitchDeferredNotice is shown for an image typed while a task runs
// on a model that cannot see it (see the REPL loop): the switch to
// visionModel waits until the task has ended, and the message with it.
func visionSwitchDeferredNotice(visionModel string) string {
	return fmt.Sprintf("[提示] 当前模型看不到图片，需要切换到视觉模型 %s，但任务运行中不能切换模型。这条消息未发送，附件仍保留；请等任务结束后重新发送（↑ 可找回刚才的输入），或先 /stop。", visionModel)
}

// steerFeedback is the line shown for text typed while a task runs: it was
// steered into that task (SubmitWithFeedback) and takes effect at the task's
// next model call.
const steerFeedback = "[已插入] 已作为指引送入当前任务，下一步模型调用时生效"

// steerReclaimedFeedback follows a task that ended before its next model call
// could pick up the guidance typed during it: the runner queues that
// guidance as a task of its own (reclaimSteerLocked).
const steerReclaimedFeedback = "[已排队] 当前任务已结束，刚插入的指引将作为新任务执行"

// steerKeptFeedback follows a task that failed or was cancelled with
// guidance still pending: it stays for the resumed turn (/continue), whose
// first model call is where it lands.
const steerKeptFeedback = "[已保留] 刚插入的指引未生效，将在 /continue 继续或下一次模型调用时送入"

// enqueueFeedback is the line shown after a typed message was handed to the
// task runner: queuedAhead, merged and wasRunning are enqueueLocked's
// results. "" means say nothing, which is the case for a message that
// started right away: its output follows.
//
// A merge always lands in a task that is still queued — Enqueue only looks at
// the queue, never at the running task. The feedback used to say
// "已合并进当前处理任务", so the user expected the running task to pick the
// correction up, when it only runs after that task finishes.
func enqueueFeedback(queuedAhead int, merged, wasRunning bool) string {
	if merged {
		return fmt.Sprintf("[已补充] 已合并进排队中的第 %d 个任务，当前任务结束后执行", queuedAhead+1)
	}
	if queuedAhead > 0 {
		return fmt.Sprintf("[任务排队中] 前方排队数: %d", queuedAhead)
	}
	if !wasRunning {
		return ""
	}
	// Typed while a task runs: say where it went. It used to say nothing,
	// and with the input line off screen during streaming the text seemed
	// to have been swallowed.
	return "[已排队] 当前任务结束后执行"
}

// denyPendingPermissionPrompt answers a waiting approval prompt with a denial
// and reports whether there was one.
//
// Ctrl+C cancels the task's context, but the prompt waits on its answer
// channel, not on the context. So the task stayed blocked for up to
// permissionPromptTimeout (15 minutes), and the next line the user typed — a
// new request — was taken as the answer and discarded. The channel is
// buffered, so this never blocks.
func denyPendingPermissionPrompt() bool {
	ch := repl.TakePermInputCh()
	if ch == nil {
		return false
	}
	ch <- promptInterrupt
	return true
}

// exitTaskWait bounds how long leaving the REPL waits for the cancelled task
// to finish its cleanup (saving the interrupted draft).
const exitTaskWait = 3 * time.Second

// exitTasks is the part of the task runner leaving the REPL needs.
type exitTasks interface {
	CancelForExit() bool
	WaitIdleUntil(deadline time.Time) bool
}

// leaveREPL is every way out of the REPL — Ctrl+D / stdin EOF, "exit",
// /exit and /restart: answer a waiting prompt, stop the running task, give
// it exitTaskWait to clean up, then save.
//
// Ctrl+D used to save at once while the task kept running (a half turn
// saved, racing the task's appends, and no interrupted draft when the
// process then died). And none of the paths answered a waiting
// permission/question/limit prompt: the task, blocked on the prompt's
// answer channel rather than its context, could not return, so the exit
// waited the full 3 seconds and saved with the task still blocked.
func leaveREPL(tasks exitTasks, save func()) {
	denyPendingPermissionPrompt()
	if tasks.CancelForExit() {
		_ = tasks.WaitIdleUntil(time.Now().Add(exitTaskWait))
	}
	// /exit typed while a turn streamed left the pinned input row's scroll
	// region set: the shell then scrolled inside the shortened window.
	repl.ReleaseTerminal()
	save()
}

// takeHistoryPick handles a line typed right after /history listed sessions
// and reports whether it consumed it: a bare number resumes that session
// (resume); anything else ends the pick and is handled as usual.
//
// While a task runs the number is refused, the way the dispatcher refuses
// "/history 2": resuming swaps the session the task is appending to. The
// bare number used to go straight to ResumeSession. The pick stays pending,
// so the number can be typed again once the task has ended.
func (fe *frontend) takeHistoryPick(input string, resume func(string)) bool {
	if !fe.historyPickPending || strings.HasPrefix(input, "/") {
		return false
	}
	if !isPositiveNumber(input) {
		fe.historyPickPending = false
		return false
	}
	if fe.running() {
		fe.print("[提示] 任务运行中不能恢复历史会话：它会改写正在使用的会话状态。请等任务结束后再输入编号，或先 /stop。")
		return true
	}
	resume(input)
	fe.historyPickPending = false
	return true
}

// continueTyped handles a bare "继续" typed at the prompt: retry the request
// that just failed, else the interrupted draft of this session, else send
// "继续" on.
func (fe *frontend) continueTyped(input string) {
	eng, tasks := fe.eng, fe.tasks
	if tasks.Snapshot().Paused {
		fe.print("队列已暂停，请通过 /tasks retry 或 /tasks skip 处理状态不明任务，再 /tasks run。")
		return
	}
	if tasks.IsRunning() {
		fe.print("[提示] 当前有任务正在运行，请等待其结束后再重试。")
		return
	}
	if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {
		fe.print(budgetExceededRetryHint(eng.CostTracker()))
		return
	}
	requeue := func(msg api.Message) {
		// Don't block: the task runs in the background.
		if _, merged := tasks.Enqueue(msg); merged {
			fe.print("[恢复] 任务记录已合并。")
		} else {
			fe.print("[恢复] 任务已排队，即将开始处理。")
		}
	}
	if pf := tasks.PendingFailed(); pf != nil {
		tasks.ClearPendingFailed()
		if isLowSignalResumeInput(pf.Content) {
			// This session's draft only: pf says nothing about a draft
			// another project or session left in the one-per-user file.
			_ = clearInterruptedDraftFor(eng.SessionID())
			fe.continueConversation(input)
			return
		}
		requeue(*pf)
		return
	}
	if draft := usableInterruptedDraft(eng); draft != nil {
		// Typed after a restart: the draft's session is not the one in use
		// (that is empty). Go back to it, so the request continues where it
		// stopped instead of starting over in a new session. This comes
		// before the low-signal check: a killed "继续" leaves a draft that
		// says nothing but still names its session, and continuing an empty
		// conversation used to resume the best-scored session instead.
		if !eng.HasMessages() && draft.SessionID != "" && draft.SessionID != eng.SessionID() && eng.Store() != nil {
			if rec, err := eng.Store().Load(draft.SessionID); err == nil {
				eng.ResumeSession(rec)
				fe.print(fmt.Sprintf("[恢复] 已切回中断任务所在的会话（%s，%d 条消息）。", shortDesc(effectiveHistoryTitle(*rec)), len(rec.Messages)))
			}
		}
		if isLowSignalResumeInput(draft.UserContent) {
			_ = clearInterruptedDraft()
			fe.continueConversation(input)
			return
		}
		// A process killed mid-task saved the request at turn start and
		// never answered it; with no interrupted turn in memory, sending the
		// draft again would put the same request in the session twice.
		// "继续" goes on from it instead.
		if requestPending(eng, draft.UserContent) {
			fe.continueConversation(input)
			return
		}
		requeue(api.Message{Role: "user", Content: draft.UserContent})
		return
	}
	fe.continueConversation(input)
}

// requestPending reports whether eng's conversation ends with content as an
// unanswered request that no in-memory interrupted turn covers: what a
// session saved at turn start by a process that was then killed looks like.
// An interrupted turn of this process is resumed by re-sending its message
// (the engine matches it), so that case is not pending here.
func requestPending(eng *engine.Engine, content string) bool {
	if eng.HasInterruptedTurn() {
		return false
	}
	msgs := eng.Messages()
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	return last.Role == "user" && strings.TrimSpace(last.Content) == strings.TrimSpace(content)
}

// continueConversation sends "继续" when there is nothing of this session to
// retry. With a conversation in progress it goes to that conversation, as
// any message would (the engine resumes an interrupted turn itself). It
// used to go to resumeAndContinue whatever the state, which resumes the
// saved session that scores highest — so "继续" after a completed turn
// could swap in a different session and continue that one. Only an empty
// conversation, which has nothing to continue, recovers the most relevant
// saved session.
func (fe *frontend) continueConversation(input string) {
	if fe.eng.HasMessages() {
		if msg := fe.tasks.SubmitWithFeedback(api.Message{Role: "user", Content: input}); msg != "" {
			fe.print(msg)
		}
		return
	}
	fe.print("[提示] 已为您推荐相关历史任务...")
	before := fe.eng.SessionID()
	resumeAndContinue(fe.eng, fe.tasks)
	fe.noteSessionSwitch(before)
}

// noteSessionSwitch drops the REPL's retry bookkeeping of the session that
// was in use when the engine has since moved to another one (before is the
// session ID from before the command). The failed request "继续" retries
// used to survive /history N, /resume <id> and the duplicate-request
// resume, so "继续" sent session A's failed request into session B.
func (fe *frontend) noteSessionSwitch(before string) {
	if fe.tasks == nil || fe.eng == nil || fe.eng.SessionID() == before {
		return
	}
	fe.tasks.ClearPendingFailed()
}

// promptInterrupt is what Ctrl+C sends to a waiting prompt instead of a
// typed answer: the permission and limit prompts treat it as a denial or a
// stop, the question tool as a cancellation. It used to be "n", which the
// question tool recorded as the user's answer before asking its next
// question on the same relay.
const promptInterrupt = tool.AskUserCancelled

// slashTarget is what "/name ..." resolves to.
type slashTarget int

const (
	slashUnknown slashTarget = iota
	slashBuiltin
	slashSkill
	slashPlugin
)

// resolveSlashCommand decides which handler owns /name. shadowed names the
// skill or plugin command that also claims the name when a built-in won.
//
// Built-ins come first. Skills and plugin commands used to be matched before
// them, so a plugin shipping commands/config.md or permissions.md silently
// replaced /config or /permissions — the very commands a user reaches for to
// see and change what tools may do — with a prompt of its own.
func resolveSlashCommand(name string, cmdReg *command.Registry, skillPrompts map[string]string, pluginCmds map[string]plugin.CommandPrompt) (target slashTarget, shadowed string) {
	_, isSkill := skillPrompts[name]
	isSkill = isSkill && name != "" && name != "skill" && name != "skills"
	pc, isPlugin := pluginCmds[name]

	if cmdReg != nil {
		if _, ok := cmdReg.Find(name); ok {
			switch {
			case isPlugin:
				shadowed = fmt.Sprintf("插件 %s 的命令 /%s", pc.Plugin, name)
			case isSkill:
				shadowed = fmt.Sprintf("技能 %s", name)
			}
			return slashBuiltin, shadowed
		}
	}
	switch {
	case isSkill:
		return slashSkill, ""
	case isPlugin:
		return slashPlugin, ""
	}
	return slashUnknown, ""
}

// handlePluginCommand checks whether the slash command matches a command
// provided by an enabled plugin. If so, it injects the command's prompt body
// (plus any trailing arguments) into the engine as a user message and returns
// true. Returns false when no plugin command matches.
func handlePluginCommand(input string, pluginMgr *plugin.Manager, tasks *replTaskRunner) bool {
	if pluginMgr == nil {
		return false
	}
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return false
	}
	name := strings.TrimPrefix(parts[0], "/")
	cmds := pluginMgr.CommandPrompts()
	cmd, ok := cmds[name]
	if !ok {
		return false
	}
	// $ARGUMENTS in the command file is replaced by what was typed after the
	// name; it used to be appended instead, leaving the placeholder literal.
	prompt := command.ExpandArguments(cmd.Prompt, strings.TrimPrefix(input, parts[0]))
	repl.PrintAbove(fmt.Sprintf("[插件命令: /%s (%s)]\r\n", name, cmd.Plugin))
	if msg := tasks.EnqueueWithFeedback(api.Message{Role: "user", Content: prompt}); msg != "" {
		repl.PrintAbove(msg + "\r\n")
	}
	return true
}

// handleSkillInvocation runs "/<skill> [args]" as a task, the way a plugin
// command runs (skillInvocationPrompt).
func handleSkillInvocation(input string, eng *engine.Engine, tasks *replTaskRunner) {
	prompt, name, ok := skillInvocationPrompt(input, eng)
	if !ok {
		repl.PrintSafe("未找到技能: %s\n", name)
		return
	}
	repl.PrintAbove(fmt.Sprintf("[技能: /%s]\r\n", name))
	if msg := tasks.EnqueueWithFeedback(api.Message{Role: "user", Content: prompt}); msg != "" {
		repl.PrintAbove(msg + "\r\n")
	}
}

// replLogWriter routes internal/log through the editor.
//
// The logger's default writer is os.Stderr, which in a raw-mode terminal lands
// at the cursor — i.e. on the input line the user is typing into. One Warnf
// from a background goroutine was enough to scramble it.
type replLogWriter struct{}

func (replLogWriter) Write(p []byte) (int, error) {
	if s := strings.TrimRight(string(p), "\r\n"); strings.TrimSpace(s) != "" {
		repl.PrintAbove(s + "\r\n")
	}
	return len(p), nil
}

// interruptedTurnSource is the part of *engine.Engine /continue needs.
type interruptedTurnSource interface {
	InterruptedTurn() (api.Message, bool)
}

// continueInterruptedTurn is /continue: when the last turn ended before
// completing (iteration or time limit, Ctrl+C, an API error) its message is
// sent again through enqueue, which the engine takes as "resume from where it
// stopped" (completed steps are kept, not redone). It returns the notice to
// show.
func continueInterruptedTurn(src interruptedTurnSource, running bool, enqueue func(api.Message)) string {
	if running {
		return "[提示] 当前有任务正在运行，请等待其结束后再输入 /continue。"
	}
	msg, ok := src.InterruptedTurn()
	if !ok {
		return "没有可继续的回合"
	}
	enqueue(msg)
	return "[继续] 正在从上一轮中断处继续…"
}
