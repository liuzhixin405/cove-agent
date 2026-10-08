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
	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
		return askToolPermission(eng, toolName, input, reason)
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
// (engine.PolicyFilePath) for this project, anything else denies.
func askToolPermission(eng permissionRuleAdder, toolName string, input map[string]any, reason string) bool {
	return askToolPermissionExternal(eng, toolName, input, reason, nil)
}

func askToolPermissionExternal(eng permissionRuleAdder, toolName string, input map[string]any, reason string, external func() (<-chan repl.ExternalAnswer, func())) bool {
	if !replInteractive {
		// Nothing is reading answer lines (e.g. a -p one-shot run), so deny
		// rather than block on a prompt that cannot be answered.
		return false
	}

	rules, what, canRemember := alwaysAllowScope(toolName, input)
	persister, canPersist := eng.(permissionRulePersister)
	canPersist = canPersist && canRemember
	// Short labels on the answer line; what an "a"/"p" would remember is
	// stated once, on its own line below, instead of being repeated inside
	// both options. The line used to read [a] 本次会话总是允许 "cd"、"git
	// push"、"echo"、"git log" 开头的命令 [p] 永久允许 "cd"、"git push"、… and
	// had to be read in full before answering.
	options := "[y] 允许"
	keys := "yn"
	if canRemember {
		options += "   [a] 本会话记住"
		keys += "a"
	}
	if canPersist {
		options += "   [p] 本项目记住"
		keys += "p"
	}
	options += "   [n] 拒绝"
	// The answer line starts at the prompt's content column, the scope line
	// one step further in, so the block reads as one indented unit.
	text := termui.PermissionPrompt(toolName, permissionPromptDescription(input, reason)) +
		permissionDiffPreview(toolName, input) +
		termui.PromptContentIndent + termui.Styled(termui.Bold, options)
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

	answer, ok := repl.AskWith(repl.AskSpec{Text: text, Accepts: permissionAnswerAccepted, Hint: permissionAnswerHint,
		Timeout: permissionPromptTimeout, Keys: keys, External: external})
	if !ok {
		termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, "授权超时，已拒绝 "+toolName) + "\n")
		return false
	}
	if answer == promptInterrupt {
		// Ctrl+C: a denial, not an answer the user typed.
		return false
	}

	allow, always, persist := permissionAnswerDecision(answer)
	switch {
	case allow && always && !canRemember:
		// "a"/"p" was typed although it was not offered: honour the allow, but
		// remembering a wider scope than the user saw would be a surprise.
		termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, "此命令无法按前缀记住，仅允许本次") + "\n")
		return true
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
			return true
		}
		termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, "已记住 "+what+"（项目 "+root+"，已写入 "+policiesFileForDisplay()+"）") + "\n")
		return true
	case allow && always:
		// The engine consults its own manager (e.perm), so the rules have to be
		// added there; they apply for this session only and are not persisted.
		for _, r := range rules {
			eng.AddPermissionRule(permission.DAllow, r)
		}
		termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, "已记住 "+what+"（本次会话）") + "\n")
		return true
	case allow:
		return true
	default:
		// The result used to reach only the model, as a tool error.
		termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Dim, "已拒绝 "+toolName) + "\n")
		return false
	}
}

// permissionAnswerHint is shown for a typed line that does not answer the
// prompt.
const permissionAnswerHint = "授权提示等待回答：y 允许 / a 本会话记住 / p 本项目记住 / n 拒绝"

// permissionAnswerAccepted reports whether line is one of the prompt's
// answers. Anything else is the person's next instruction, not a refusal.
func permissionAnswerAccepted(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "是", "允许", "a", "always", "总是", "p", "permanent", "永久", "n", "no", "否", "拒绝":
		return true
	}
	return false
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
	switch strings.ToLower(strings.TrimSpace(answer)) {
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
