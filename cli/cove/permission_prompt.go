package main

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/render"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// permissionPromptTimeout bounds how long a gated tool waits for the user's
// answer. The prompt is rendered on the task goroutine while the main loop sits
// in ReadLine, so a prompt nobody answers (Ctrl+D at the prompt, a detached
// terminal, /exit) would otherwise block the run forever. It is a variable so
// tests can shorten it.
var permissionPromptTimeout = 15 * time.Minute

// installPermissionPrompt wires eng.PermissionPrompt so tools that need
// approval ask the user instead of being denied outright.
//
// The engine denies every "ask" decision when PermissionPrompt is nil
// (see Engine.authorizeTool), which makes write/bash tools unusable in the
// default permission mode with a misleading "no interactive approval handler is
// installed" error. Only a frontend whose input loop serves
// repl.TakePermInputCh may install a handler; without the handler the deny path
// stays in place, which is the correct fail-closed behaviour.
func installPermissionPrompt(eng *engine.Engine) {
	if eng == nil {
		return
	}
	eng.PermissionPromptEx = func(toolName string, input map[string]any, reason string) engine.PermissionAnswer {
		return askToolPermissionAnswer(eng, toolName, input, reason)
	}
	// The bool callback stays installed for code (and tests) that only know it.
	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
		return askToolPermissionAnswer(eng, toolName, input, reason).Allow
	}
}

// permissionRuleAdder is the part of *engine.Engine the prompt needs: the
// engine consults its own manager, so "always" rules must be added there.
type permissionRuleAdder interface {
	AddPermissionRule(permission.Decision, permission.Rule)
}

// permissionExplainer is implemented by *engine.Engine: it says why the
// rules remembered for a tool do not cover the call being asked about.
type permissionExplainer interface {
	ExplainPermissionGap(toolName string, input map[string]any) string
}

// permissionRulePersister is implemented by *engine.Engine: it writes allow
// rules to the policies file (engine.PolicyFilePath) in one save, scoped to
// the engine's project root, and on success installs them for this session as
// rules loaded from policies.json (so /cd drops them). The "[p]" option is
// only offered when the adder can persist.
type permissionRulePersister interface {
	PersistPermissionRules(rules []permission.Rule, scope string) error
	PermissionScope() string
}

// keyHintShown: the "how to answer" note was shown on a prompt already.
var keyHintShown atomic.Bool

// askToolPermission renders the approval box, waits for the answer line that
// the REPL loop relays through repl.TakePermInputCh, and reports the decision.
//
// Answers: "y"/"yes" allow this call once, "a"/"always" additionally remember
// the scope from alwaysAllowScope for the rest of the session (an engine-wide
// policy rule), "p"/"permanent" also write that rule to the policies file
// (engine.PolicyFilePath) for this project, "e" denies after asking for a
// reason the model reads, "n" (optionally followed by a reason) denies.
func askToolPermission(eng permissionRuleAdder, toolName string, input map[string]any, reason string) bool {
	return askToolPermissionAnswer(eng, toolName, input, reason).Allow
}

// askToolPermissionAnswer is askToolPermission with the denial's reason.
func askToolPermissionAnswer(eng permissionRuleAdder, toolName string, input map[string]any, reason string) engine.PermissionAnswer {
	return askToolPermissionExternalAnswer(eng, toolName, input, reason, nil)
}

func askToolPermissionExternal(eng permissionRuleAdder, toolName string, input map[string]any, reason string, external func() (<-chan repl.ExternalAnswer, func())) bool {
	return askToolPermissionExternalAnswer(eng, toolName, input, reason, external).Allow
}

func askToolPermissionExternalAnswer(eng permissionRuleAdder, toolName string, input map[string]any, reason string, external func() (<-chan repl.ExternalAnswer, func())) engine.PermissionAnswer {
	deny := engine.PermissionAnswer{}
	if !replInteractive {
		// Nothing is reading answer lines (e.g. a -p one-shot run), so deny
		// rather than block on a prompt that cannot be answered.
		return deny
	}

	rules, what, canRemember := alwaysAllowScope(toolName, input)
	persister, canPersist := eng.(permissionRulePersister)
	canPersist = canPersist && canRemember
	// Leaving plan mode is a decision about the plan, not about a command:
	// the box shows the plan and offers to revise it; nothing to remember.
	isPlanExit := toolName == "exit_plan_mode"
	if isPlanExit {
		canRemember, canPersist = false, false
	}
	// Short labels on the answer line; what an "a"/"p" would remember is
	// stated once, on its own line below, instead of being repeated inside
	// both options. The line used to read [a] 本次会话总是允许 "cd"、"git
	// push"、"echo"、"git log" 开头的命令 [p] 永久允许 "cd"、"git push"、… and
	// had to be read in full before answering.
	options := "[y] 允许"
	if isPlanExit {
		options = "[y] 执行计划"
	}
	// "e" is typed, not a key: with a prompt waiting, a line starting with e
	// ("edit the test first") must stay the next instruction.
	keys := "yn"
	if canRemember {
		options += "   [a] 本会话记住"
		keys += "a"
	}
	if canPersist {
		options += "   [p] 本项目记住"
		keys += "p"
	}
	if isPlanExit {
		options += "   [e] 修改计划   [n] 继续规划"
	} else {
		options += "   [n] 拒绝   [e] 拒绝并说明"
	}
	// The answer line starts at the prompt's content column, the scope line
	// one step further in, so the block reads as one indented unit.
	box := termui.PermissionPrompt(toolName, permissionPromptDescription(input, reason)) + permissionDiffPreview(toolName, input)
	if isPlanExit {
		box = termui.GutterBox("退出计划模式", toolName, planExitDescription(input), false)
	}
	text := box + termui.PromptContentIndent + termui.Styled(termui.Bold, options)
	// How to answer is said on the first prompt of the process only; it
	// used to follow every answer line.
	if keyHintShown.CompareAndSwap(false, true) {
		text += "   " + termui.Styled(termui.Dim, "（按键即答，无需回车；Ctrl+C 拒绝并停止任务）")
	}
	text += "\n"
	if canRemember {
		// what names the executable of the command, or the model-supplied
		// MCP server and tool; it was printed raw, so a name carrying escape
		// sequences could redraw the prompt it was being approved in.
		text += termui.PromptContentIndent + "    " + termui.Styled(termui.Dim, "记住范围: "+render.VisibleControls(what)) + "\n"
	}
	// Rules were remembered but do not cover this line: say why, or the
	// prompt reads as if remembering had not worked. Dim: information, not
	// a warning (it was yellow).
	if ex, ok := eng.(permissionExplainer); ok {
		if why := ex.ExplainPermissionGap(toolName, input); why != "" {
			// The reason can quote words of the command: shown the same way.
			text += termui.PromptContentIndent + "    " + termui.Styled(termui.Dim, render.VisibleControls(why)) + "\n"
		}
	}

	hint := permissionAnswerHintFor(canRemember, canPersist)
	if isPlanExit {
		hint = "等待确认计划：y 执行计划 / e 修改计划 / n 继续规划"
	}
	preview := []string{"审批: " + toolName, "操作: " + permissionPromptDescription(input, reason), options}
	if isPlanExit {
		preview = []string{"审批: 退出计划模式", "y 执行计划，e 提修改意见，n 继续规划", options}
	}
	if canRemember {
		preview = append(preview, "记住范围: "+what)
	}
	answer, ok := repl.AskWith(repl.AskSpec{Text: text, Title: "等待授权", Preview: preview, Accepts: permissionAnswerAccepted,
		Hint: hint, Timeout: permissionPromptTimeout, Keys: keys, External: external})
	if !ok {
		termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, "授权超时，已拒绝 "+toolName) + "\n")
		return deny
	}
	if answer == promptInterrupt {
		// Ctrl+C: a denial, not an answer the user typed.
		return deny
	}
	if permissionAnswerExplains(answer) {
		label, hint := "拒绝理由（直接回车跳过）: ", "输入拒绝理由，或直接回车跳过"
		if isPlanExit {
			label, hint = "修改意见（直接回车跳过）: ", "输入对计划的修改意见，或直接回车跳过"
		}
		// Local only: the remote side has no reason line, and offering it
		// the same approval again made its y/n the "reason".
		reasonText, cancelled := askDenyReason(label, hint, nil)
		return denyWithReason(toolName, reasonText, cancelled)
	}

	allow, always, persist := permissionAnswerDecision(answer)
	switch {
	case allow && always && !canRemember:
		// "a"/"p" was typed although it was not offered: honour the allow, but
		// remembering a wider scope than the user saw would be a surprise.
		termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, "此命令无法按前缀记住，仅允许本次") + "\n")
		return engine.PermissionAnswer{Allow: true}
	case allow && persist && canPersist:
		root := persister.PermissionScope()
		// A successful persist installs the rules itself, registered as
		// rules loaded from policies.json so /cd to another project drops
		// them; adding a session copy here would outlive the /cd.
		if err := persister.PersistPermissionRules(rules, root); err != nil {
			for _, r := range rules {
				eng.AddPermissionRule(permission.DAllow, r)
			}
			termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, "已允许 "+what+"；未能写入，仅本次会话有效（policies.json: "+err.Error()+"）") + "\n")
			return engine.PermissionAnswer{Allow: true}
		}
		termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, "已记住 "+what+"（项目 "+root+"，已写入 "+policiesFileForDisplay()+"）") + "\n")
		return engine.PermissionAnswer{Allow: true}
	case allow && always:
		// The engine consults its own manager (e.perm), so the rules have to be
		// added there; they apply for this session only and are not persisted.
		for _, r := range rules {
			eng.AddPermissionRule(permission.DAllow, r)
		}
		termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, "已记住 "+what+"（本次会话）") + "\n")
		return engine.PermissionAnswer{Allow: true}
	case allow:
		return engine.PermissionAnswer{Allow: true}
	default:
		// A typed "n <reason>" carries the reason; a bare "n" does not.
		return denyWithReason(toolName, permissionDenyReason(answer), false)
	}
}

// planSummaryMaxLines bounds the rendered plan in the exit_plan_mode box;
// planListMaxItems and planListItemRunes bound each list the model attached
// (the engine's own 40-item limit applies only to the plan it saves, not to
// what reaches this box).
const (
	planSummaryMaxLines = 80
	planListMaxItems    = 40
	planListItemRunes   = 200
)

// planExitDescription is the body of the exit_plan_mode approval box: the
// model's plan summary rendered as Markdown, followed by the files,
// decisions and checks when the model listed them. The text is the model's,
// so its controls are made visible before rendering; the box used to show
// only the engine's reason ("exiting plan mode requires confirmation") and
// the person approved a plan they had not seen.
func planExitDescription(input map[string]any) string {
	summary, _ := input["summary"].(string)
	summary = strings.TrimSpace(summary)
	var body string
	if summary == "" {
		body = "（模型未提供计划摘要）\n退出计划模式后模型将开始修改文件。"
	} else {
		lines := strings.Split(strings.TrimRight(render.Markdown(render.VisibleControls(summary)), "\n"), "\n")
		if len(lines) > planSummaryMaxLines {
			more := len(lines) - planSummaryMaxLines
			lines = append(lines[:planSummaryMaxLines], termui.Styled(termui.Dim, fmt.Sprintf("…还有 %d 行", more)))
		}
		body = "计划摘要：\n" + strings.Join(lines, "\n")
	}
	for _, section := range []struct{ key, title string }{{"files", "涉及文件"}, {"decisions", "关键决策"}, {"checks", "验收标准"}} {
		items := planExitList(input[section.key])
		if len(items) == 0 {
			continue
		}
		more := 0
		if len(items) > planListMaxItems {
			more, items = len(items)-planListMaxItems, items[:planListMaxItems]
		}
		body += "\n" + section.title + "："
		for _, item := range items {
			body += "\n  • " + render.VisibleControls(textutil.ClipRunes(item, planListItemRunes))
		}
		if more > 0 {
			body += "\n  " + termui.Styled(termui.Dim, fmt.Sprintf("…还有 %d 项", more))
		}
	}
	return body
}

// planExitList reads a string list the model put in the exit_plan_mode input.
func planExitList(value any) []string {
	var out []string
	switch items := value.(type) {
	case []string:
		out = items
	case []any:
		for _, item := range items {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

// denyWithReason prints the denial line and returns the answer. The result
// used to reach only the model, as a tool error.
func denyWithReason(toolName, reasonText string, cancelled bool) engine.PermissionAnswer {
	reasonText = strings.TrimSpace(reasonText)
	denied, carried := "已拒绝 "+toolName, "已拒绝 "+toolName+"，理由已转给模型："
	if toolName == "exit_plan_mode" {
		denied, carried = "继续规划，未退出计划模式", "继续规划，修改意见已转给模型："
	}
	if cancelled || reasonText == "" {
		termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, denied) + "\n")
		return engine.PermissionAnswer{}
	}
	// The reason is echoed, so a control sequence in it is shown as text.
	termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, carried+render.VisibleControls(reasonText)) + "\n")
	return engine.PermissionAnswer{DenyReason: reasonText}
}

// askDenyReason reads one line of reason after an "e" answer. Enter alone
// skips (reason ""); Ctrl+C or a timeout cancels (cancelled true).
func askDenyReason(label, hint string, external func() (<-chan repl.ExternalAnswer, func())) (reason string, cancelled bool) {
	text := termui.PromptContentIndent + termui.Styled(termui.Dim, label) + "\n"
	answer, ok := repl.AskWith(repl.AskSpec{Text: text, Title: "等待授权",
		Preview: []string{hint}, AllowEmpty: true, Hint: hint,
		Timeout: permissionPromptTimeout, External: external})
	if !ok || answer == promptInterrupt {
		return "", true
	}
	return strings.TrimSpace(answer), false
}

// permissionAnswerHintFor is shown for a typed line that does not answer the
// prompt; it names only the keys the prompt offered.
func permissionAnswerHintFor(canRemember, canPersist bool) string {
	parts := []string{"y 允许"}
	if canRemember {
		parts = append(parts, "a 本会话记住")
	}
	if canPersist {
		parts = append(parts, "p 本项目记住")
	}
	parts = append(parts, "n 拒绝", "e 拒绝并说明")
	return "授权提示等待回答：" + strings.Join(parts, " / ")
}

// permissionAnswerAccepted reports whether line is one of the prompt's
// answers: a key word, or a denial followed by a reason ("n 用 switch").
// Anything else is the person's next instruction, not a refusal.
func permissionAnswerAccepted(line string) bool {
	word, _ := splitAnswerWord(line)
	switch word {
	case "y", "yes", "是", "允许", "a", "always", "总是", "p", "permanent", "永久", "e", "explain", "说明":
		return true
	case "n", "no", "否", "拒绝":
		return true
	}
	return false
}

// splitAnswerWord separates the answer word from what follows it, lowercased.
func splitAnswerWord(line string) (word, rest string) {
	line = strings.TrimSpace(line)
	word, rest, _ = strings.Cut(line, " ")
	return strings.ToLower(word), strings.TrimSpace(rest)
}

// permissionDenyReason is the reason typed after a denial word; "" for any
// other answer.
func permissionDenyReason(answer string) string {
	word, rest := splitAnswerWord(answer)
	switch word {
	case "n", "no", "否", "拒绝":
		return rest
	}
	return ""
}

// permissionAnswerExplains reports whether the answer asks to type a reason.
func permissionAnswerExplains(answer string) bool {
	word, _ := splitAnswerWord(answer)
	return word == "e" || word == "explain" || word == "说明"
}

// alwaysAllowScope decides what an "a" answer remembers, and describes it
// for the prompt's 记住范围 line and the confirmation. Shell tools get the
// rules of permission.ShellRememberRules: the routine git group for a
// routine git write (`bash 中 git 常规操作（…）`), otherwise one rule per
// command prefix that needed approval (`bash 中 "go test" 开头的命令`);
// allowing the whole bash tool would let every later command, rm -rf
// included, run unasked. The MCP proxy gets one rule for the server tool
// being called, e.g. `MCP 工具 "github/create_issue"`. Other tools get a
// whole-tool rule. ok is false when a shell command has no prefix that can be
// remembered safely (or an MCP call names no server tool), so "a" is not
// offered at all.
func alwaysAllowScope(toolName string, input map[string]any) (rules []permission.Rule, what string, ok bool) {
	if toolName == "mcp" {
		// The MCP proxy fronts every tool of every connected server; a
		// whole-tool rule allowed all of them, destructive ones included, for
		// the rest of the session. Remember the one server tool instead.
		server, _ := input["serverName"].(string)
		name, _ := input["toolName"].(string)
		if strings.TrimSpace(server) == "" || strings.TrimSpace(name) == "" {
			return nil, "", false
		}
		rule := permission.Rule{ToolPattern: toolName, InputEquals: map[string]string{"serverName": server, "toolName": name}}
		return []permission.Rule{rule}, `MCP 工具 "` + server + "/" + name + `"`, true
	}
	if !permission.IsShellTool(toolName) {
		return []permission.Rule{{ToolPattern: toolName}}, "工具 " + toolName + " 的所有调用", true
	}
	command, _ := input["command"].(string)
	rules, ok = permission.ShellRememberRules(toolName, command, permission.ToolShellKind(toolName))
	if !ok {
		return nil, "", false
	}
	return rules, toolName + " 中 " + describeShellRules(rules), true
}

// describeShellRules names the rules of ShellRememberRules for a reader.
func describeShellRules(rules []permission.Rule) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		switch {
		case r.CommandGroup != "":
			parts = append(parts, permission.GroupLabel(r.CommandGroup))
		case r.CommandPrefix != "":
			parts = append(parts, `"`+r.CommandPrefix+`" 开头的命令`)
		}
	}
	return strings.Join(parts, "、")
}

// permissionAnswerDecision maps a raw answer line to a decision: allow reports
// whether the call is permitted, always requests a session-wide rule for the
// tool, persist additionally asks for that rule to be saved for this project.
// Anything unrecognised denies, so a stray keypress fails closed.
func permissionAnswerDecision(answer string) (allow, always, persist bool) {
	word, _ := splitAnswerWord(answer)
	switch word {
	case "y", "yes", "是", "允许":
		return true, false, false
	case "a", "always", "总是":
		return true, true, false
	case "p", "permanent", "永久":
		return true, true, true
	default:
		return false, false, false
	}
}

// permissionPromptDescription picks the most informative line to show inside the
// approval box: the concrete argument the user is being asked about, falling
// back to the engine's reason when the tool takes no recognisable argument.
func permissionPromptDescription(input map[string]any, reason string) string {
	for _, key := range []string{"command", "file_path", "filePath", "path", "pattern", "query", "url"} {
		if v, ok := input[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return reason
}

// permissionPreviewLines bounds the diff shown in the permission prompt.
const permissionPreviewLines = 40

// permissionDiffPreview is the change a write or edit would make, shown in
// the prompt under the path: the path alone said nothing about what was
// about to be written. "" for other tools or when it cannot be computed.
func permissionDiffPreview(toolName string, input map[string]any) string {
	cwd, _ := os.Getwd()
	d, ok := engine.PreviewFileChange(toolName, input, cwd)
	if !ok {
		return ""
	}
	gutter := "  " + termui.Yellow + "┃" + termui.Reset + " "
	if d.Text == "" {
		return gutter + termui.Styled(termui.Dim, "内容不变") + "\n" + "  " + termui.Yellow + "┃" + termui.Reset + "\n"
	}
	lines := strings.Split(d.Text, "\n")
	more := 0
	if len(lines) > permissionPreviewLines {
		more = len(lines) - permissionPreviewLines
		lines = lines[:permissionPreviewLines]
	}
	var sb strings.Builder
	sb.WriteString(gutter + termui.Styled(termui.Dim, "改动 "+d.Summary()) + "\n")
	// VisibleControls, not StripControls: stripping dropped an escape
	// sequence with the text it swallowed (an OSC left open across lines hid
	// them all), so the user approved content they had not been shown.
	for _, l := range strings.Split(render.ColorDiff(render.VisibleControls(strings.Join(lines, "\n")), false), "\n") {
		sb.WriteString(gutter + strings.ReplaceAll(l, "\t", "    ") + "\n")
	}
	if more > 0 {
		sb.WriteString(gutter + termui.Styled(termui.Dim, fmt.Sprintf("… 还有 %d 行", more)) + "\n")
	}
	sb.WriteString("  " + termui.Yellow + "┃" + termui.Reset + "\n")
	return sb.String()
}

// policiesFileForDisplay names the policies file a "[p]" rule was written to:
// policies.json in the config directory, which COVE_CONFIG_DIR moves away
// from ~/.cove.
func policiesFileForDisplay() string {
	if p, err := engine.PolicyFilePath(); err == nil {
		return p
	}
	return "policies.json"
}

// PolicyLoadError forwards Engine.PolicyLoadError to the commands, so /cd can
// warn when policies.json failed to reload for the new project. It sits here
// with the rest of the permission plumbing rather than beside the other
// replEngineAdapter forwarders in main.go.
func (a replEngineAdapter) PolicyLoadError() error { return a.eng.PolicyLoadError() }
