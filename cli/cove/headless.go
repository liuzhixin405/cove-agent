package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/dream"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/log"
)

// runHeadless is the non-interactive frontend used when stdin/stdout is not a
// terminal (pipes, redirects) or when the TUI is explicitly disabled
// (--no-tui / COVE_TUI=0). It replaces the classic line REPL for these cases:
// it reads commands/prompts line-by-line from stdin and runs them synchronously,
// writing engine output to stdout and diagnostics to stderr. There is no
// alternate screen, raw-mode reader, or task queue — output is script-friendly.
//
// failed reports that some input was not answered: a turn ended in an error
// (or was cancelled), a prompt line could not be sent (no API key, over
// budget, an attachment that could not be read), or a SIGTERM ended the run
// before the input did. main exits 1 then, after
// finishSession. The run used to exit 0 whatever happened, so a script
// piping prompts into cove could not tell a run whose every turn failed
// from one that worked. Slash commands do not count: they report their own
// errors as text and have no status to go by.
func runHeadless(app *appBootstrap, cmdReg *command.Registry, bannerText string) (failed bool) {
	return runHeadlessFrom(os.Stdin, app, cmdReg, bannerText)
}

// runHeadlessFrom is runHeadless reading its input from in.
func runHeadlessFrom(in io.Reader, app *appBootstrap, cmdReg *command.Registry, bannerText string) (failed bool) {
	eng, toolReg, cfg, mcpPool := app.eng, app.toolReg, app.cfg, app.mcpPool
	skillMgr, memStore, pluginMgr, projCtx := app.skillMgr, app.memStore, app.pluginMgr, app.projCtx
	// Headless runs exit when stdin ends, so a skill review started on the last
	// turn would be abandoned mid-request like in -p; skip it here too.
	if eng != nil {
		eng.SetNonInteractive(true)
		wireNonInteractiveOutput(eng)
	}
	// Banner goes to stderr so stdout carries only assistant/command output.
	if strings.TrimSpace(bannerText) != "" {
		fmt.Fprint(os.Stderr, bannerText)
	}

	scanner := bufio.NewScanner(in)
	// Allow long single-line inputs (e.g. pasted prompts) up to 8 MiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	// The same command registry and dispatch as the REPL (frontend.go); what
	// differs is that each line runs synchronously and notices go to stdout.
	// terminated: a turn was stopped by SIGTERM, so the run ends after it
	// (see turnEndsRun), whether the turn came from a prompt line or a
	// command such as /continue or a skill — or a slash command itself was
	// running when the SIGTERM came (fe.terminated).
	terminated := false
	fe := &frontend{eng: eng, cfg: cfg, toolReg: toolReg, mcpPool: mcpPool, skillMgr: skillMgr,
		memStore: memStore, pluginMgr: pluginMgr, projCtx: projCtx,
		print: func(s string) { outln(s) },
		enqueue: func(msg api.Message) {
			term, turnFailed := runHeadlessTurn(eng, msg)
			terminated = terminated || term
			failed = failed || turnFailed
		},
	}
	// 前端已持有该注册表并由它分发；返回值是同一个注册表，此路径其后不再使用。
	fe.install(cmdReg)
	defer fe.closeWorkflows()

	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			// Windows PowerShell pipes text into a program with a UTF-8
			// byte-order mark: "/context" arrived as BOM + "/context", was
			// not a command, and went to the model as a message.
			line, first = strings.TrimPrefix(line, utf8BOM), false
		}
		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}
		if fe.historyPickPending {
			// Like the shell's takeHistoryPick: only the line right after
			// /history may pick a session by number; any other line ends
			// the pick. It used to stay pending, so a bare number typed
			// turns later resumed a session instead of going to the model.
			fe.historyPickPending = false
			if isPositiveNumber(input) {
				handleHistoryResume(input, eng)
				continue
			}
		}
		if input == "exit" || input == "quit" {
			break
		}
		if fe.dispatch(input) {
			if fe.terminated {
				terminated = true
			}
			if fe.exitRequested || terminated {
				break
			}
			continue
		}

		// Normal prompt → engine turn.
		pc := cfg.EffectiveProvider()
		if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {
			fmt.Fprintln(os.Stderr, budgetExceededRetryHint(eng.CostTracker()))
			failed = true
			continue
		}
		if runNeedsAPIKey(pc.APIKey, replayDir != "") {
			fmt.Fprintln(os.Stderr, missingAPIKeyMessage(pc.Name))
			failed = true
			continue
		}

		cwd, _ := os.Getwd()
		userMsg, warnings, err := buildUserMessage(input, cwd, fe.attachedFiles, cfg.Model)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			failed = true
			continue
		}

		// Auto-switch to a vision model when an image attachment is detected but
		// the current model can't see it (parity with the classic REPL).
		if shouldAutoSwitchToVision(warnings) {
			if visionModel := preferredVisionModelForProvider(pc.Name, cfg.Model); visionModel != "" && visionModel != cfg.Model {
				if switchErr := applyProviderConfigChange(cfg, eng, func() error {
					cfg.Model = visionModel
					return nil
				}); switchErr == nil {
					fmt.Fprintf(os.Stderr, "[视觉] 检测到图片附件，已自动切换到视觉模型 %s。\n", visionModel)
					userMsg, warnings, err = buildUserMessage(input, cwd, fe.attachedFiles, cfg.Model)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error: %v\n", err)
						failed = true
						continue
					}
				}
			}
		}
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, w) // the warning carries its own ⚠
		}
		// Clear one-shot attachments after sending (avoids resending each turn).
		fe.attachedFiles = nil

		term, turnFailed := runHeadlessTurn(eng, userMsg)
		failed = failed || turnFailed
		if term {
			terminated = true
			break
		}
	}

	if terminated {
		// The rest of the input was not run, which is a failure too.
		fmt.Fprintln(os.Stderr, "[已终止] 收到 SIGTERM，不再读取后续输入")
		return true
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stdin 读取错误: %v\n", err)
		failed = true
	}
	return failed
}

// printModeBackgroundWait bounds how long cove -p waits, after the answer,
// for the engine's background work (memory extraction) before exiting. A -p
// process used to exit the moment the answer was printed, so the extraction
// started at the end of the turn was always killed and -p never learned.
const printModeBackgroundWait = 20 * time.Second

// backgroundWaiter is the engine's WaitBackground (waits for its background
// goroutines, returning early when ctx ends). Asserted rather than called
// directly so this builds against an engine that does not have it yet.
type backgroundWaiter interface {
	WaitBackground(ctx context.Context)
}

// waitForBackground waits for v's background work up to limit and reports
// whether v supports waiting at all. The limit is enforced here, not left to
// WaitBackground: a waiter that ignores its context still cannot hold the
// exit up (its goroutine is simply abandoned; the process is exiting).
func waitForBackground(v any, limit time.Duration) bool {
	w, ok := v.(backgroundWaiter)
	if !ok || w == nil {
		log.Debugf("[-p] engine has no WaitBackground; exiting without waiting for background work")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.WaitBackground(ctx)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Debugf("[-p] background work still running after %v; exiting anyway", limit)
	}
	return true
}

// runPrintModeSession is the whole -p run: the turn (runPrintMode), then the
// wrap-up every -p exit owes — waiting for memory extraction, then
// finishSession. Automatic dream is switched off for the process first: its
// run would be killed at exit with the consolidation lock already stamped, so
// the sessions it was reviewing would count as consolidated.
func runPrintModeSession(eng *engine.Engine, argPrompt, prompt string, debug bool, attachmentPaths []string, cfg *config.Config, pool interface{ DisconnectAll() }) int {
	dream.SuppressAuto("-p 模式：回答后进程立即退出，整理会被中途终止")
	log.Debugf("[autoDream] skipped for this process: -p mode exits right after the answer")
	if eng != nil {
		// The skill review would be abandoned at exit after its paid call.
		eng.SetNonInteractive(true)
		wireNonInteractiveOutput(eng)
	}
	code := runPrintMode(eng, argPrompt, prompt, debug, attachmentPaths, cfg)
	if eng != nil {
		waitForBackground(eng, printModeBackgroundWait)
	}
	finishSession(eng, pool)
	return code
}

// watchTurnSignal cancels the turn when a signal arrives on sigCh. The
// returned channel yields that signal, or is closed without one once ctx
// ends — the watcher never outlives its turn. It used to be
// `go func() { <-sigCh; cancel() }()`, and since signal.Stop does not close
// the channel, every turn without a signal leaked that goroutine.
func watchTurnSignal(ctx context.Context, sigCh <-chan os.Signal, cancel context.CancelFunc) <-chan os.Signal {
	out := make(chan os.Signal, 1)
	go func() {
		defer close(out)
		select {
		case sig := <-sigCh:
			cancel()
			out <- sig
		case <-ctx.Done():
		}
	}()
	return out
}

// turnEndsRun reports whether the signal that stopped a headless turn ends
// the whole run: SIGTERM does (the sender wants the process gone), SIGINT
// only cancels the turn. SIGTERM used to be treated like SIGINT, and the loop
// went on to run the next input lines.
func turnEndsRun(sig os.Signal) bool { return sig == syscall.SIGTERM }

// runHeadlessTurn drives a single engine turn synchronously, printing the reply
// to stdout. SIGINT/SIGTERM cancel the in-flight turn instead of killing the
// process outright; terminated reports a SIGTERM, after which the caller
// ends the run the normal way (finishSession) instead of reading on. failed
// reports that the turn ended in an error or was cancelled (runHeadless's
// exit status).
func runHeadlessTurn(eng *engine.Engine, userMsg api.Message) (terminated, failed bool) {
	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	watched := watchTurnSignal(ctx, sigCh, cancel)
	defer func() {
		signal.Stop(sigCh)
		cancel()
		terminated = turnEndsRun(<-watched)
	}()

	resp, err := eng.RunMessageWithStream(ctx, userMsg, nil, nil)
	if err != nil {
		failed = true
		if s := eng.LastWrapUp(); s != "" {
			outln(s) // the stopped turn's no-tool summary
		}
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "[已取消] 当前任务已终止")
		} else {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return
	}
	noteTurnCompleted()
	outln(resp)
	if eng.HasMessages() {
		eng.SaveSession()
	}
	return
}
