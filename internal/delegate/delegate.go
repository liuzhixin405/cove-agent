package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
	"github.com/liuzhixin405/cove-agent/internal/token"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// SubAgent is an isolated child agent that executes a specific sub-task.
type SubAgent struct {
	requireRegression bool
	regressionPassed  bool
	provider          api.Provider
	model             string
	registry          *tool.Registry
	maxIter           int
	cwd               string
	permissionMode    string
	authorize         Authorizer
	executor          Executor
	budgetExceeded    func() bool
	contextTokens     int
	progress          func(step string)
	fallback          string
}

// Authorizer decides whether a sub-agent may run one tool call. It is the same
// gate the engine applies to top-level calls, handed down as a callback so the
// delegate package does not have to know about permission modes or prompts.
// A nil return means the call may proceed; a non-nil error is shown to the
// sub-agent as the tool's result.
type Authorizer func(ctx context.Context, tc api.ToolCall) error

// Executor runs one tool call and returns the text handed back to the model.
// The engine supplies its own tool pipeline here (safety scan, hooks,
// guardrails, validation, permission, checkpoints, output limits), so a
// sub-agent's calls get exactly the treatment top-level calls get.
type Executor func(ctx context.Context, tc api.ToolCall) string

// Config configures a sub-agent.
type Config struct {
	// RequireRegression refuses a completion claim without tool-produced red/green evidence.
	RequireRegression bool
	Provider          api.Provider
	Model             string
	Tools             []tool.Tool // restricted tool set
	MaxIter           int         // max model calls (0 = DefaultMaxIter)
	// Cwd is the project directory tool calls resolve relative paths against.
	Cwd string
	// PermissionMode is the session's real mode. It is passed through to
	// tool.Context so tools that branch on it see the truth rather than a
	// hardcoded "auto".
	PermissionMode string
	// Authorize gates every tool call. Leave nil only if the sub-agent's tools
	// are all read-only; see gate(). Unused when Executor is set.
	Authorize Authorizer
	// Executor, when set, runs each tool call instead of the sub-agent.
	Executor Executor
	// BudgetExceeded, when set, is checked before every model call; a true
	// result stops the sub-agent.
	BudgetExceeded func() bool
	// ContextTokens is how many tokens the request (system prompt and
	// history) may take before old tool results are trimmed; 0 = no limit.
	ContextTokens int
	// Progress, when set, is told each tool step as it finishes (the
	// stepSummary line), for the front end's live progress.
	Progress func(step string)
	// Fallback is the model to move to, once, when the model is overloaded
	// or out of quota (503/429 after the provider's retries); "" = none.
	Fallback string
}

// Options adjusts a single delegated task.
type Options struct {
	// RequireRegression limits tools to code inspection and regression_verify.
	RequireRegression bool
	// ReadOnly restricts the sub-agent to read-only tools.
	ReadOnly bool
	// Exclude names tools the sub-agent does not get (a code review has no
	// use for web search).
	Exclude []string
}

// excludedTools are never offered to a sub-agent. The first group would let
// it spawn further sub-agents (execute_plan and team_create both run the plan
// executor); the second mutates the plan and task state that the parent's
// executor is driving; the last would have a background sub-agent prompt the
// user, flip the session's mode, or switch the session's active worktree.
var excludedTools = map[string]bool{
	"agent": true, "execute_plan": true, "team_create": true, "team_delete": true,
	"todowrite": true, "task": true, "task_update": true, "task_stop": true, "cron": true,
	"question": true, "plan_mode": true, "exit_plan_mode": true,
	"worktree": true, "exit_worktree": true,
}

// loopLimit is how many consecutive identical tool-call batches end a run.
const loopLimit = 3

// subAgentResultBytes backstops the size of one tool result in a sub-agent.
const subAgentResultBytes = 32 * 1024

// DefaultMaxIter is a sub-agent's model-call cap when none is configured
// (config "subagent_max_iterations").
const DefaultMaxIter = 60

// NewSubAgent creates a new isolated sub-agent.
func NewSubAgent(cfg Config) *SubAgent {
	if cfg.MaxIter <= 0 {
		cfg.MaxIter = DefaultMaxIter
	}
	reg := tool.NewRegistry()
	for _, t := range cfg.Tools {
		if excludedTools[t.Def().Name] {
			continue
		}
		reg.Register(t)
	}
	return &SubAgent{
		requireRegression: cfg.RequireRegression,
		provider:          cfg.Provider,
		model:             cfg.Model,
		registry:          reg,
		maxIter:           cfg.MaxIter,
		cwd:               cfg.Cwd,
		permissionMode:    cfg.PermissionMode,
		authorize:         cfg.Authorize,
		executor:          cfg.Executor,
		budgetExceeded:    cfg.BudgetExceeded,
		contextTokens:     cfg.ContextTokens,
		progress:          cfg.Progress,
		fallback:          cfg.Fallback,
	}
}

// keepRecentToolResults: the latest tool results are never trimmed; they are
// what the sub-agent is working from.
const keepRecentToolResults = 6

// trimmedToolHead is how much of a trimmed tool result is kept.
const trimmedToolHead = 400

const trimmedToolNote = "\n[earlier tool output trimmed to fit the context; run the tool again if you need it]"

// trimHistory cuts the oldest tool results (all but the latest
// keepRecentToolResults) to their first trimmedToolHead bytes until the
// history fits budget tokens. A sub-agent has no compaction: 60 rounds of
// 32KB results overflowed the window and the run died on a context-length
// error, losing its work.
func trimHistory(msgs []api.Message, budget int) []api.Message {
	if budget <= 0 {
		return msgs
	}
	total := 0
	var toolIdx []int
	for i, m := range msgs {
		total += token.Estimate(m.Content)
		for _, tc := range m.ToolCalls {
			args, _ := json.Marshal(tc.Input)
			total += token.Estimate(string(args))
		}
		if m.Role == "tool" {
			toolIdx = append(toolIdx, i)
		}
	}
	if total <= budget || len(toolIdx) <= keepRecentToolResults {
		return msgs
	}
	for _, i := range toolIdx[:len(toolIdx)-keepRecentToolResults] {
		c := msgs[i].Content
		if len(c) <= trimmedToolHead+len(trimmedToolNote) || strings.HasSuffix(c, trimmedToolNote) {
			continue
		}
		cut := textutil.ClipBytes(c, trimmedToolHead, "") + trimmedToolNote
		total -= token.Estimate(c) - token.Estimate(cut)
		msgs[i].Content = cut
		if total <= budget {
			break
		}
	}
	return msgs
}

// gate reports whether the sub-agent may run this tool call.
func (sa *SubAgent) gate(ctx context.Context, tc api.ToolCall, t tool.Tool) error {
	if sa.authorize != nil {
		return sa.authorize(ctx, tc)
	}
	// No gate was wired. Fail closed: a sub-agent that can mutate the
	// workspace with nobody checking is the exact hole this gate closes, so
	// only read-only tools go through. Read-only tools stay usable so a
	// sub-agent is still worth spawning.
	if t.Def().IsReadOnly {
		return nil
	}
	return fmt.Errorf("no permission gate configured for sub-agent: refusing non-read-only tool %q", tc.Name)
}

// Exit reasons of a sub-agent run (Result.ExitReason).
const (
	// ExitCompleted: the model answered without further tool calls.
	ExitCompleted = "completed"
	// ExitMaxIterations: the model-call cap stopped the run; Output holds
	// the partial result.
	ExitMaxIterations = "max_iterations"
	// ExitInterrupted: cancelled, timed out, or out of budget.
	ExitInterrupted = "interrupted"
	// ExitError: a model call failed.
	ExitError = "error"
	// ExitUnverified: the model finished without passing regression evidence.
	ExitUnverified = "unverified"
	// ExitLoop: the same tool-call batch was requested loopLimit times.
	ExitLoop = "loop"
)

// Result is the outcome of a sub-agent task.
type Result struct {
	Output  string
	Steps   int
	Success bool
	// ExitReason says why the run stopped: one of the Exit* constants.
	ExitReason string
	// Truncated reports that Output is a partial result rather than the
	// sub-agent's final answer.
	Truncated bool
	// CapReached reports that the run stopped at its model-call cap; Output
	// then holds the partial result (the steps done and the last text).
	// Kept for compatibility: it equals ExitReason == ExitMaxIterations.
	CapReached bool
	Error      string

	// Token usage across every model call the sub-agent made, and the model
	// that served them, so callers can report what the task cost.
	InputTokens  int
	OutputTokens int
	Model        string
}

// toolDefsFor builds the API tool definitions with each tool's real schema.
func toolDefsFor(tools []tool.Tool) []api.ToolDef {
	var defs []api.ToolDef
	for _, t := range tools {
		d := t.Def()
		schema := map[string]any{"type": "object"}
		if len(d.InputSchema) > 0 {
			var parsed map[string]any
			if err := json.Unmarshal(d.InputSchema, &parsed); err == nil && parsed != nil {
				schema = parsed
			}
		}
		defs = append(defs, api.ToolDef{Name: d.Name, Description: d.Description, InputSchema: schema})
	}
	return defs
}

// fingerprint identifies a tool-call batch by names and arguments, ignoring
// the call IDs the provider assigns.
func fingerprint(calls []api.ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, tc := range calls {
		args, _ := json.Marshal(tc.Input) // map keys marshal sorted
		parts = append(parts, tc.Name+string(args))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// Run executes the sub-agent's task and returns a summary.
func (sa *SubAgent) Run(ctx context.Context, task string, systemPrompt string) *Result {
	messages := []api.Message{{Role: "user", Content: task}}
	toolDefs := toolDefsFor(sa.registry.All())
	res := &Result{Model: sa.model}

	var lastPrint string
	repeats := 0
	// steps and lastText make up the partial result when the cap is reached:
	// what the sub-agent did and what it last said.
	var steps []string
	var lastText string
	for iter := 0; iter < sa.maxIter; iter++ {
		res.Steps = iter
		select {
		case <-ctx.Done():
			// A deadline is the sub-agent's own time limit running out, not
			// the user cancelling; "cancelled" for both made a timeout look
			// like a Ctrl+C in the plan summary.
			res.Error, res.ExitReason = "cancelled", ExitInterrupted
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.Error = "timed out"
			}
			return res
		default:
		}
		if sa.budgetExceeded != nil && sa.budgetExceeded() {
			res.Error, res.ExitReason = "budget exceeded", ExitInterrupted
			return res
		}

		if sa.contextTokens > 0 {
			messages = trimHistory(messages, sa.contextTokens-token.Estimate(systemPrompt))
		}
		resp, err := sa.provider.Chat(ctx, api.ChatRequest{
			Model:      sa.model,
			Messages:   messages,
			SystemBase: systemPrompt,
			Tools:      toolDefs,
			// Sized per model, as the engine's turns are: a fixed 16000 was
			// rejected with a 400 by deepseek-chat (8192) and claude-3-haiku
			// (4096) on every sub-agent call. sa.model may be the fallback.
			MaxTokens: api.MaxOutputTokensForModel(sa.model),
		})
		// An overloaded model hands the run to the fallback once, as the
		// engine does for a turn: a sub-agent used to die on the first 503.
		if err != nil && ctx.Err() == nil && sa.fallback != "" && sa.fallback != sa.model {
			if k := api.Classify(err); k == api.KindServerError || k == api.KindRateLimit {
				if sa.progress != nil {
					sa.progress("模型 " + sa.model + " 不可用，改用 " + sa.fallback)
				}
				sa.model, sa.fallback = sa.fallback, ""
				iter--
				continue
			}
		}
		if err != nil {
			res.Error, res.ExitReason = err.Error(), ExitError
			if ctx.Err() != nil {
				// The call failed because the run was stopped, not on its own.
				res.ExitReason = ExitInterrupted
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.Error = "timed out: " + res.Error
			}
			return res
		}
		res.InputTokens += resp.InputTokens
		res.OutputTokens += resp.OutputTokens
		if resp.Model != "" {
			res.Model = resp.Model
		}

		if len(resp.ToolCalls) == 0 {
			res.Output, res.Success, res.Steps = resp.Content, true, iter+1
			res.ExitReason = ExitCompleted
			if sa.requireRegression && !sa.regressionPassed {
				res.Success, res.ExitReason, res.Error = false, ExitUnverified, "no passing red/green regression evidence; model claims are not proof"
			}
			return res
		}

		if fp := fingerprint(resp.ToolCalls); fp == lastPrint {
			repeats++
		} else {
			lastPrint, repeats = fp, 1
		}
		if repeats >= loopLimit {
			res.Error = fmt.Sprintf("loop detected: the same tool calls were requested %d times in a row", repeats)
			res.Steps = iter + 1
			res.ExitReason = ExitLoop
			return res
		}

		if strings.TrimSpace(resp.Content) != "" {
			lastText = resp.Content
		}
		messages = append(messages, api.Message{
			Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls, ThinkingBlocks: resp.ThinkingBlocks,
		})
		for _, tc := range resp.ToolCalls {
			content := sa.runTool(ctx, tc)
			if sa.requireRegression && tc.Name == "regression_verify" {
				var evidence RegressionEvidence
				sa.regressionPassed = json.Unmarshal([]byte(content), &evidence) == nil && evidence.Status == "passed" && len(evidence.Tests) > 0 && evidence.Baseline != "" && evidence.Current != "" && evidence.Before.ExitCode == 1 && evidence.After.ExitCode == 0 && evidence.Before.Error == "" && evidence.After.Error == ""
			}
			steps = append(steps, stepSummary(tc, content))
			if sa.progress != nil {
				sa.progress(steps[len(steps)-1])
			}
			// Truncate large results on a rune boundary (a byte slice lands
			// inside a multi-byte rune for Chinese output, and invalid UTF-8
			// goes straight into the next request's JSON body). The engine's
			// executor already applies its own, window-aware limit; this is
			// only a backstop, so it must not be the binding one — at 4000
			// bytes a sub-agent saw little more than the first page of a file.
			content = textutil.ClipBytes(content, subAgentResultBytes, "\n[...truncated]")
			messages = append(messages, api.Message{
				Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: content,
			})
		}
	}

	res.Steps = sa.maxIter
	res.Error = fmt.Sprintf("已达上限（%d 次模型调用），以下为部分结果", sa.maxIter)
	res.Output = partialOutput(steps, lastText)
	res.ExitReason = ExitMaxIterations
	res.CapReached = true
	res.Truncated = true
	return res
}

// maxPartialSteps bounds how many completed steps a partial result lists.
const maxPartialSteps = 30

// stepSummary is one line of a partial result: the tool, its main argument
// and whether it failed.
func stepSummary(tc api.ToolCall, result string) string {
	line := tc.Name
	for _, k := range []string{"filePath", "file_path", "path", "command", "pattern", "query", "url"} {
		if v, ok := tc.Input[k].(string); ok && strings.TrimSpace(v) != "" {
			line += " " + textutil.ClipRunes(strings.TrimSpace(v), 80)
			break
		}
	}
	if strings.HasPrefix(result, "Error:") {
		line += "（失败）"
	}
	return line
}

// partialOutput is the Output of a sub-agent stopped at its cap: the steps it
// completed (the latest maxPartialSteps) and its last text.
func partialOutput(steps []string, lastText string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "已完成 %d 个步骤：\n", len(steps))
	shown := steps
	if len(shown) > maxPartialSteps {
		fmt.Fprintf(&b, "（省略前 %d 个）\n", len(shown)-maxPartialSteps)
		shown = shown[len(shown)-maxPartialSteps:]
	}
	for i, st := range shown {
		fmt.Fprintf(&b, "%d. %s\n", len(steps)-len(shown)+i+1, st)
	}
	if t := strings.TrimSpace(lastText); t != "" {
		b.WriteString("\n最后一次模型输出：\n" + textutil.ClipRunes(t, 2000))
	}
	return strings.TrimRight(b.String(), "\n")
}

// runTool executes one call and returns the text for its tool result.
func (sa *SubAgent) runTool(ctx context.Context, tc api.ToolCall) string {
	// The sub-agent's own registry decides what it may call. The executor
	// can reach every engine tool, including the excluded ones.
	t, ok := sa.registry.Find(tc.Name)
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", tc.Name)
	}
	if sa.executor != nil {
		return sa.executor(ctx, tc)
	}
	if err := sa.gate(ctx, tc, t); err != nil {
		return "Error: " + err.Error()
	}
	result, err := t.Call(ctx, tc.Input, tool.Context{
		Cwd:            sa.cwd,
		ToolUseID:      tc.ID,
		PermissionMode: sa.permissionMode,
	})
	if err != nil {
		return "Error: " + err.Error()
	}
	return result.Data
}

// Delegator manages sub-agent lifecycle.
type Delegator struct {
	mu     sync.Mutex
	tools  []tool.Tool
	active map[string]context.CancelFunc

	// providerSource is consulted when each task starts, so a provider or
	// model switch after construction reaches later sub-agents.
	providerSource func() (api.Provider, string)

	// Tool gate handed to every sub-agent this delegator spawns. Set before
	// the first Delegate call; not guarded by mu.
	cwd            string
	permissionMode string
	authorize      Authorizer
	executor       Executor
	budgetExceeded func() bool
	// maxIter is each sub-agent's model-call cap; 0 = DefaultMaxIter.
	maxIter int
	// contextSource, when set, returns the project context (environment,
	// instruction files, outline) appended to every sub-agent's system
	// prompt. Sub-agents used to get only a one-line role, so they worked
	// without the project's rules or even its working directory.
	contextSource func() string
	// contextBudget, when set, is the token budget of a sub-agent request
	// on model (Config.ContextTokens).
	contextBudget func(model string) int
	// progress, when set, receives one line per sub-agent event: start,
	// each tool step, end. Sub-agents used to run silently until their
	// result came back as one collapsed block.
	progress func(line string)
	// fallbackFor, when set, names the model a sub-agent on model moves to
	// when that one is overloaded (Config.Fallback).
	fallbackFor func(model string) string
}

// SetFallback installs the overload fallback (see fallbackFor). Call it
// before the first Delegate; not guarded by mu.
func (d *Delegator) SetFallback(f func(model string) string) { d.fallbackFor = f }

// SetProgress installs the progress sink (see progress). Call it before the
// first Delegate; not guarded by mu. It is called from the sub-agents'
// goroutines, in parallel for a parallel plan.
func (d *Delegator) SetProgress(f func(line string)) { d.progress = f }

// SetContextSource installs the project context source (see contextSource).
// Call it before the first Delegate; not guarded by mu.
func (d *Delegator) SetContextSource(src func() string) { d.contextSource = src }

// SetContextBudget installs the per-model request token budget (see
// contextBudget). Call it before the first Delegate; not guarded by mu.
func (d *Delegator) SetContextBudget(budget func(model string) int) { d.contextBudget = budget }

// NewDelegator creates a sub-agent delegator.
func NewDelegator(provider api.Provider, model string, tools []tool.Tool) *Delegator {
	return &Delegator{
		tools:          tools,
		active:         make(map[string]context.CancelFunc),
		providerSource: func() (api.Provider, string) { return provider, model },
	}
}

// SetProviderSource makes the delegator resolve its provider and model when
// each task starts.
func (d *Delegator) SetProviderSource(src func() (api.Provider, string)) {
	d.providerSource = src
}

// SetGate installs the permission gate that sub-agents run their tool calls
// through, along with the project directory they operate in. Call it before the
// first Delegate. A delegator with no gate can only run read-only tools.
func (d *Delegator) SetGate(cwd, permissionMode string, authorize Authorizer) {
	d.cwd = cwd
	d.permissionMode = permissionMode
	d.authorize = authorize
}

// SetExecutor routes every sub-agent tool call through exec. It supersedes the
// gate installed by SetGate.
func (d *Delegator) SetExecutor(exec Executor) { d.executor = exec }

// SetMaxIter sets each later sub-agent's model-call cap (config
// "subagent_max_iterations"); n <= 0 restores DefaultMaxIter. Call it before
// the first Delegate; not guarded by mu.
func (d *Delegator) SetMaxIter(n int) {
	if n < 0 {
		n = 0
	}
	d.maxIter = n
}

func (d *Delegator) maxIterOrDefault() int {
	if d.maxIter > 0 {
		return d.maxIter
	}
	return DefaultMaxIter
}

// SetBudgetCheck stops sub-agents before a model call once exceeded() is true.
func (d *Delegator) SetBudgetCheck(exceeded func() bool) { d.budgetExceeded = exceeded }

// Delegate spawns a sub-agent for the given task. Blocks until completion.
func (d *Delegator) Delegate(ctx context.Context, taskID, task, systemPrompt string) *Result {
	return d.DelegateWith(ctx, taskID, task, systemPrompt, Options{})
}

// DelegateWith is Delegate with per-task options.
func (d *Delegator) DelegateWith(ctx context.Context, taskID, task, systemPrompt string, opts Options) *Result {
	subCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)

	d.mu.Lock()
	d.active[taskID] = cancel
	d.mu.Unlock()

	defer func() {
		cancel()
		d.mu.Lock()
		delete(d.active, taskID)
		d.mu.Unlock()
	}()

	log.Debugf("delegate: starting sub-agent for task %s", taskID)

	tools := d.tools
	if opts.ReadOnly || len(opts.Exclude) > 0 || opts.RequireRegression {
		excluded := map[string]bool{}
		for _, n := range opts.Exclude {
			excluded[n] = true
		}
		tools = nil
		for _, t := range d.tools {
			name := t.Def().Name
			if opts.RequireRegression && name != "read" && name != "glob" && name != "grep" && name != "repo_map" && name != "regression_verify" {
				continue
			}
			if (opts.ReadOnly || opts.RequireRegression) && !t.Def().IsReadOnly && !(opts.RequireRegression && name == "regression_verify") || excluded[name] {
				continue
			}
			tools = append(tools, t)
		}
	}
	provider, model := d.providerSource()
	if d.contextSource != nil {
		if c := strings.TrimSpace(d.contextSource()); c != "" {
			systemPrompt += "\n\n" + c
		}
	}
	contextTokens := 0
	if d.contextBudget != nil {
		contextTokens = d.contextBudget(model)
	}
	fallback := ""
	if d.fallbackFor != nil {
		fallback = d.fallbackFor(model)
	}
	sa := NewSubAgent(Config{
		RequireRegression: opts.RequireRegression,
		Provider:          provider,
		Model:             model,
		Tools:             tools,
		MaxIter:           d.maxIterOrDefault(),
		Cwd:               d.cwd,
		PermissionMode:    d.permissionMode,
		Authorize:         d.authorize,
		Executor:          d.executor,
		BudgetExceeded:    d.budgetExceeded,
		ContextTokens:     contextTokens,
		Fallback:          fallback,
	})
	if d.progress == nil {
		return sa.Run(subCtx, task, systemPrompt)
	}
	sa.progress = func(step string) { d.progress("├ " + taskID + " · " + step) }
	d.progress("▸ " + taskID + " 开始：" + textutil.ClipRunes(strings.Join(strings.Fields(task), " "), 80))
	start := time.Now()
	res := sa.Run(subCtx, task, systemPrompt)
	mark, how := "✓", "完成"
	if !res.Success {
		mark, how = "✗", "未完成："+textutil.ClipRunes(res.Error, 60)
	}
	d.progress(fmt.Sprintf("%s %s %s · %d 步 · %s", mark, taskID, how, res.Steps, time.Since(start).Round(time.Second)))
	return res
}
