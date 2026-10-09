package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
	ctxt "github.com/liuzhixin405/cove-agent/internal/context"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/mcp"
	"github.com/liuzhixin405/cove-agent/internal/memory"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/plugin"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/skills"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// frontend is what the slash commands of an interactive or headless front
// end work on. Both front ends dispatch every slash command through one
// command.Registry (dispatch): the generic commands of internal/command and
// the front-end commands below, which close over this state. There used to
// be three mechanisms — a switch in each front end's loop, two handler
// functions that intercepted lines before the registry, and the registry
// itself — with the command list written out five times (the switches, the
// handlers, the registry, the completion table, the "must not run while a
// task runs" list), and the two front ends had drifted apart: headless let
// a plugin command replace /status, and did not know /continue or /clear.
type frontend struct {
	eng       *engine.Engine
	cfg       *config.Config
	toolReg   *tool.Registry
	mcpPool   *mcp.Pool
	skillMgr  *skills.Manager
	memStore  *memory.Store
	pluginMgr *plugin.Manager
	projCtx   *ctxt.ProjectContext
	reg       *command.Registry

	// tasks is the interactive front end's task runner; nil in headless,
	// which runs each line synchronously.
	tasks *replTaskRunner

	attachedFiles []string
	// historyPickPending: /history listed sessions, so a bare number picks
	// one.
	historyPickPending bool
	// freshFromNew: /new emptied the conversation and nothing was sent since.
	freshFromNew bool
	// exitRequested is set by /exit; the loop leaves after the command.
	exitRequested bool
	// restartRequested is set by /restart, with exitRequested: the loop
	// leaves the same way, and main then starts cove again.
	restartRequested bool
	// terminated is set when a SIGTERM stopped a slash command. Headless
	// ends the run on it, as it does for a SIGTERM during a turn; the REPL
	// ignores it (there the signal only cancels the command, as before).
	terminated bool

	// print shows a notice line; enqueue runs a message as a task (queued
	// in the REPL, synchronously in headless).
	print   func(string)
	enqueue func(api.Message)
	choose  func(string, []repl.Choice) bool
}

func (fe *frontend) interactive() bool { return fe.tasks != nil }

func (fe *frontend) running() bool { return fe.tasks != nil && fe.tasks.IsRunning() }

func (fe *frontend) closeWorkflows() {
	if err := fe.stopRemote(context.Background()); err != nil && fe.print != nil {
		fe.print("remote shutdown: " + err.Error())
	}
	if fe.reg == nil {
		return
	}
	if entry, exists := fe.reg.Find("race"); exists {
		if closer, ok := entry.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil && fe.print != nil {
				fe.print("race shutdown: " + err.Error())
			}
		}
	}
}

// feCmd is a front-end command. run gets the line as typed. A command that
// overlays a generic one of the same name (base) handles what it knows and
// hands the rest to base: bare /config shows the configuration, /config
// <key> <value> is the generic command.
type feCmd struct {
	name, desc, help, category string
	aliases                    []string
	hints                      []string
	mutates                    func(args []string) bool
	run                        func(ctx context.Context, in command.Input) (handled bool)
	base                       command.Command
}

func (c *feCmd) Name() string        { return c.name }
func (c *feCmd) Aliases() []string   { return c.aliases }
func (c *feCmd) Description() string { return c.desc }
func (c *feCmd) Help() string {
	if c.help != "" {
		return c.help
	}
	return "/" + c.name + " - " + c.desc
}
func (c *feCmd) Category() string   { return c.category }
func (c *feCmd) ArgHints() []string { return c.hints }
func (c *feCmd) MutatesEngine(args []string) bool {
	if c.mutates != nil {
		return c.mutates(args)
	}
	return c.base != nil && command.Mutates(c.base, args)
}
func (c *feCmd) Execute(ctx context.Context, in command.Input) (command.Output, error) {
	if c.run(ctx, in) || c.base == nil {
		return command.Output{}, nil
	}
	return c.base.Execute(ctx, in)
}

// historyArgsResume reports whether "/history <args>" resumes a session,
// following handleSessionCommand's parse: every form except the listings
// (bare, all), detail, delete, clean and clear does. Only "/history <N>"
// used to count, so "/history all 2" and "/history <session-id>" swapped
// the session a running task was appending to.
func historyArgsResume(a []string) bool {
	if len(a) == 0 {
		return false
	}
	arg := strings.Join(a, " ")
	if strings.EqualFold(arg, "clean") {
		return false
	}
	if _, _, ok := parseHistoryClear(arg); ok {
		return false
	}
	if strings.EqualFold(a[0], "all") {
		if a = a[1:]; len(a) == 0 {
			return false
		}
	}
	switch strings.ToLower(a[0]) {
	case "detail", "delete":
		return false
	}
	return true
}

func always([]string) bool        { return true }
func withArgs(args []string) bool { return len(args) > 0 }

// Help sections, in the order /help shows them.
const (
	catModel   = "供应商 / 模型"
	catSession = "会话"
	catTasks   = "后台任务"
	catSystem  = "系统"
)

// helpCategories is the /help order; commands without a category come last,
// under "命令".
var helpCategories = []string{catModel, catSession, catTasks, catSystem}

// install registers fe's commands over the generic ones in reg and makes
// reg the registry fe dispatches through.
func (fe *frontend) install(reg *command.Registry) *command.Registry {
	fe.reg = reg
	for _, commands := range [][]command.Command{fe.automationCommands(), fe.browserVerificationCommands(), fe.raceCommands(), fe.remoteCommands(), fe.agentMapCommands()} {
		for _, entry := range commands {
			reg.Register(entry)
		}
	}
	base := func(name string) command.Command {
		c, _ := reg.Find(name)
		return c
	}
	config := func(ctx context.Context, in command.Input) bool {
		return handleBuiltinConfigCommand(in.Raw, fe.cfg, fe.eng)
	}
	usage := func(text string) func(context.Context, command.Input) bool {
		return func(ctx context.Context, in command.Input) bool {
			if handleBuiltinConfigCommand(in.Raw, fe.cfg, fe.eng) {
				return true
			}
			fe.print("用法: " + text)
			return true
		}
	}
	session := func(ctx context.Context, in command.Input) bool {
		all := strings.EqualFold(strings.TrimSpace(in.Raw), "/history all")
		if (strings.TrimSpace(in.Raw) == "/history" || all) && fe.interactive() && !fe.running() && fe.choose != nil {
			title := "当前项目会话"
			if all {
				title = "所有项目会话"
			}
			if fe.choose(title, historyPickerChoices(fe.eng, all)) {
				fe.historyPickPending = false
				return true
			}
		}
		return handleSessionCommand(in.Raw, fe.eng, &fe.historyPickPending)
	}

	for _, c := range []*feCmd{
		// Provider and model.
		{name: "model", desc: "设置模型", category: catModel, mutates: withArgs, run: usage("/model <名称>")},
		{name: "profile", desc: "管理配置档案 (list/switch/save/delete/show)", category: catModel,
			hints: []string{"list", "switch", "save", "delete", "show"},
			mutates: func(a []string) bool {
				return len(a) > 0 && (a[0] == "switch" || a[0] == "use" || a[0] == "delete")
			}, run: config},
		{name: "provider", desc: "设置供应商", category: catModel, hints: providerNameSuggestions(), mutates: withArgs,
			run: func(ctx context.Context, in command.Input) bool {
				if !handleBuiltinConfigCommand(in.Raw, fe.cfg, fe.eng) {
					fe.print("用法: /provider <名称>\n" + providerHelpLine())
				}
				return true
			}},
		{name: "api-key", desc: "设置 API 密钥", category: catModel, mutates: withArgs, run: usage("/api-key <密钥>")},
		// mutates like /model and /api-key: it reloads the provider, and
		// without it "/base-url <地址>" swapped the client mid-turn.
		{name: "base-url", desc: "设置 API 地址", category: catModel, mutates: withArgs, run: usage("/base-url <地址>")},
		{name: "mode", desc: "设置权限模式 (default|plan|auto|bypass)", category: catModel,
			hints: []string{"default", "plan", "auto", "bypass"},
			run: func(ctx context.Context, in command.Input) bool {
				if handleBuiltinConfigCommand(in.Raw, fe.cfg, fe.eng) {
					return true
				}
				fe.print(fmt.Sprintf("当前模式: %s（可选: %s）", fe.eng.PermissionMode(), "default|plan|auto|bypass"))
				return true
			}},
		{name: "budget", desc: "本会话预算上限 ($)；save 写入配置", category: catModel, run: config},
		{name: "record", desc: "录制会话事件 (status/start/stop)", category: catModel,
			hints: []string{"status", "start", "stop"}, run: config},
		{name: "config", desc: "查看完整配置；/config <键> <值> 修改", category: catModel, base: base("config"), run: config},
		{name: "cost", desc: "查看用量和费用", category: catModel, base: base("cost"), run: config},

		// Session.
		{name: "new", desc: "保存当前会话并开始新会话（清空对话上下文）", category: catSession, mutates: always,
			run: func(ctx context.Context, in command.Input) bool {
				saved := startNewSession(fe.eng, fe.tasks, &fe.attachedFiles)
				fe.freshFromNew = true
				fe.print(newSessionNotice(saved))
				return true
			}},
		{name: "compact", desc: "压缩对话历史", category: catSession, mutates: always, base: base("compact"), run: session},
		{name: "history", desc: "查看和继续历史会话（clear/delete/detail/all）", category: catSession,
			hints:   []string{"clear", "delete", "clean", "detail", "all"},
			mutates: historyArgsResume,
			base:    base("history"), run: session},
		{name: "resume", desc: "恢复已保存的会话", category: catSession, mutates: withArgs, base: base("resume"), run: session},
		{name: "export", desc: "导出当前会话为 Markdown", category: catSession, mutates: always, base: base("export"), run: session},
		{name: "continue", desc: "从中断处继续上一轮", category: catSession,
			run: func(ctx context.Context, in command.Input) bool {
				if fe.tasks != nil && fe.tasks.Snapshot().Paused {
					fe.print("队列已暂停，请通过 /tasks retry 或 /tasks skip 处理状态不明任务，再 /tasks run。")
					return true
				}
				if t := fe.eng.CostTracker(); t != nil && t.OverBudget() {
					fe.print(budgetExceededRetryHint(t))
					return true
				}
				fe.print(continueInterruptedTurn(fe.eng, fe.running(), func(msg api.Message) {
					if fe.tasks != nil {
						// The engine resumes the turn when its message is sent
						// again; the retry bookkeeping of "继续" is now stale.
						fe.tasks.ClearPendingFailed()
					}
					// This conversation's draft only; another project's or
					// session's is not what /continue resumes.
					_ = clearInterruptedDraftFor(fe.eng.SessionID())
					fe.enqueue(msg)
				}))
				return true
			}},
		{name: "attach", desc: "挂载图片或文件到后续提问（list/remove/clear）", category: catSession,
			hints: []string{"list", "clear", "remove", "add"},
			run: func(ctx context.Context, in command.Input) bool {
				cwd, _ := os.Getwd()
				handleAttachCommand(in.Raw, cwd, &fe.attachedFiles)
				return true
			}},

		{name: "x", aliases: []string{"expand"}, desc: "展开工具输出：/x [编号] [all]，编号是工具块后的 #N，省略则展开最近一个", category: catSession,
			hints: []string{"all"},
			run: func(ctx context.Context, in command.Input) bool {
				fe.print(expandCommand(in.Args))
				return true
			}},

		// Background tasks.
		{name: "acceptance", desc: "查看当前会话最新任务的验收证据", category: catTasks,
			run: func(ctx context.Context, in command.Input) bool {
				report, err := fe.eng.LastAcceptance()
				if err != nil {
					fe.print("读取验收报告失败: " + err.Error())
				} else if report == nil {
					fe.print("当前会话尚无任务验收报告。")
				} else {
					fe.print(report.Summary())
				}
				return true
			}},
		{name: "tasks", desc: "查看、恢复和管理持久化任务队列", category: catTasks,
			hints: []string{"saved", "restore", "remove", "move", "run", "retry", "skip"},
			run: func(ctx context.Context, in command.Input) bool {
				if fe.tasks == nil {
					fe.print("headless 模式按行同步执行，不维护后台任务队列。")
				} else {
					fe.print(fe.tasks.taskQueueCommand(in.Args))
				}
				return true
			}},
		{name: "stop", aliases: []string{"cancel"}, desc: "取消当前运行的任务", category: catTasks,
			run: func(ctx context.Context, in command.Input) bool {
				fe.stop()
				return true
			}},

		// System.
		{name: "help", desc: "显示帮助", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				printHelp(fe.reg, fe.toolReg, fe.pluginMgr)
				return true
			}},
		{name: "keys", aliases: []string{"shortcuts"}, desc: "查看输入快捷键", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				fe.print(keybindingHelp)
				return true
			}},
		{name: "tools", desc: "列出可用工具", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				printTools(fe.toolReg, fe.pluginMgr)
				return true
			}},
		{name: "skill", aliases: []string{"skills"}, desc: "列出、查看或调用技能", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				handleSkill(in.Raw, fe.eng)
				return true
			}},
		{name: "doctor", desc: "检查运行环境", category: catSystem, base: base("doctor"),
			run: func(ctx context.Context, in command.Input) bool {
				if len(in.Args) > 0 {
					return false
				}
				runDoctor()
				return true
			}},
		{name: "clear", aliases: []string{"cls"}, desc: "清屏，不影响对话上下文（快捷键 Ctrl+L）", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				if !fe.interactive() {
					fe.print("headless 模式没有可清的屏幕。")
				} else if !repl.ClearScreen() {
					fe.print("[提示] 任务输出中，暂不清屏；任务结束后再试（对话上下文不受影响）")
				}
				return true
			}},
		{name: "exit", aliases: []string{"quit"}, desc: "退出", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				fe.exitRequested = true
				return true
			}},
		{name: "trust", desc: "信任当前项目：目录（回合结束自动运行构建/测试校验）及其 .cove.json（启用其中的 MCP、provider、校验命令等设置，/restart 后生效）", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				fe.print(trustProjectConfig(fe.cfg))
				return true
			}},
		// mutates: a restart in the middle of a task would cut it off.
		{name: "restart", desc: "保存会话并重启 cove，之后继续当前会话（新装的技能、插件和新版本随之生效）", category: catSystem, mutates: always,
			run: func(ctx context.Context, in command.Input) bool {
				if !fe.interactive() {
					fe.print("headless 模式不支持 /restart：输入来自管道，重启后无法接着读。")
					return true
				}
				fe.exitRequested, fe.restartRequested = true, true
				return true
			}},
	} {
		reg.Register(c)
	}
	return reg
}

// stop is /stop: cancel the running task and say when it is really gone.
func (fe *frontend) stop() {
	if fe.tasks == nil {
		fe.print("headless 模式当前没有可取消的后台任务。")
		return
	}
	if !fe.tasks.IsRunning() {
		fe.print("[提示] 当前没有运行中的任务")
		return
	}
	denyPendingPermissionPrompt()
	if fe.tasks.CancelRunning() {
		// Cancelling asks the task to stop; it is gone only once its
		// goroutine has returned, and saying "terminated" a moment early
		// made the next /continue answer "still running".
		if fe.tasks.WaitIdleUntil(time.Now().Add(1500 * time.Millisecond)) {
			fe.print("[已取消] 当前任务已终止，输入 /continue 可从中断处继续")
		} else {
			fe.print("[已中断] 正在停止当前任务…结束后可用 /continue 继续")
		}
	}
}

// mutates reports whether the slash line input must wait while a task runs.
func (fe *frontend) mutates(input string) bool {
	fields := strings.Fields(input)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return false
	}
	c, ok := fe.reg.Find(strings.TrimPrefix(fields[0], "/"))
	return ok && command.Mutates(c, fields[1:])
}

// dispatch runs the slash command line input and reports whether input was
// one. A built-in command wins over a skill or plugin command of the same
// name (and says so), so a plugin cannot replace /config or /permissions.
func (fe *frontend) dispatch(input string) bool {
	if !strings.HasPrefix(input, "/") {
		return false
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return false
	}
	name := strings.TrimPrefix(fields[0], "/")
	if fe.running() && fe.mutates(input) {
		fe.print(fmt.Sprintf("[提示] 任务运行中不能执行 %s：它会改写正在使用的会话状态。请等任务结束，或先 /stop。", fields[0]))
		return true
	}
	// A number right after /history picks from its list; any command in
	// between ends that (and /history starts it again).
	fe.historyPickPending = false

	var skillPrompts map[string]string
	if fe.eng != nil && fe.eng.Runtime() != nil {
		skillPrompts = fe.eng.Runtime().SkillPrompts
	}
	var pluginCmds map[string]plugin.CommandPrompt
	if fe.pluginMgr != nil {
		pluginCmds = fe.pluginMgr.CommandPrompts()
	}
	target, shadowed := resolveSlashCommand(name, fe.reg, skillPrompts, pluginCmds)
	switch target {
	case slashSkill:
		if fe.interactive() {
			handleSkillInvocation(input, fe.eng, fe.tasks)
		} else if prompt, name, ok := skillInvocationPrompt(input, fe.eng); ok {
			fmt.Fprintf(os.Stderr, "[技能: /%s]\n", name)
			fe.enqueue(api.Message{Role: "user", Content: prompt})
		}
		return true
	case slashPlugin:
		if fe.interactive() {
			handlePluginCommand(input, fe.pluginMgr, fe.tasks)
		} else if prompt, label, ok := pluginCommandPrompt(input, fe.pluginMgr); ok {
			fmt.Fprintf(os.Stderr, "[插件命令: /%s]\n", label)
			fe.enqueue(api.Message{Role: "user", Content: prompt})
		}
		return true
	case slashUnknown:
		handleUnknownCmd(input, fe.reg)
		return true
	}
	if shadowed != "" {
		fe.print(fmt.Sprintf("[提示] %s 与内置命令同名，已执行内置命令 /%s", shadowed, name))
	}
	before := ""
	if fe.eng != nil {
		before = fe.eng.SessionID()
	}
	if sig := withInterrupt(func(ctx context.Context) {
		fe.execute(ctx, input)
	}); turnEndsRun(sig) {
		fe.terminated = true
	}
	// /history N, /history all N, /history <id>, /resume <id>.
	fe.noteSessionSwitch(before)
	return true
}

// commandMutatesEngine reports whether the typed line input is a command
// that must not run while a task runs, from the commands' own metadata.
func commandMutatesEngine(input string) bool {
	fe := &frontend{}
	fe.install(registerAllCommands())
	return fe.mutates(input)
}

// trustProjectConfig is /trust: it trusts the .cove.json content this process
// loaded (not whatever the file holds now, so a file swapped after the
// warning stays untrusted) and its directory, and returns the notice to show.
// With no untrusted .cove.json it trusts the project directory, which lets
// the automatic build/test verification run: /trust used to answer "nothing
// to trust" there, and that verification ran in every clone unasked.
func trustProjectConfig(cfg *config.Config) string {
	if cfg == nil {
		return "配置不可用"
	}
	path, fields := cfg.UntrustedProjectConfig()
	if path == "" {
		return trustProjectDir(currentProjectDir())
	}
	// The loaded file is the startup directory's. After /cd it used to be
	// trusted anyway while the notice pointed at /restart, and the restarted
	// cove, now in the new directory, found that directory's .cove.json
	// still untrusted.
	if !session.SameProjectDir(filepath.Dir(path), currentProjectDir()) {
		return fmt.Sprintf("工作目录已切换（%s），/trust 只能信任启动时加载的 %s。请先 /restart，再对新目录的 .cove.json 执行 /trust。", currentProjectDir(), path)
	}
	if err := cfg.TrustLoadedProjectConfig(); err != nil {
		return fmt.Sprintf("信任 %s 失败: %v", path, err)
	}
	return fmt.Sprintf("已信任 %s（按当前内容，文件改动后需要重新 /trust）。输入 /restart 让 %s 生效。", path, strings.Join(fields, "、"))
}

// trustProjectDir trusts the project root of dir (its git root, else dir),
// the directory the engine's verification gate checks along with dir itself.
func trustProjectDir(dir string) string {
	root := permission.ProjectRoot(dir)
	for _, d := range []string{dir, root} {
		if ok, _ := config.IsProjectDirTrusted(d); ok {
			return "此项目已受信任。"
		}
	}
	if err := config.TrustProjectDir(root); err != nil {
		return fmt.Sprintf("信任 %s 失败: %v", root, err)
	}
	return fmt.Sprintf("已信任此项目目录 %s：回合结束时会自动运行构建/测试校验。", root)
}
