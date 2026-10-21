package command

import (
	"context"
	"fmt"
	"github.com/liuzhixin405/cove-agent/internal/cost"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

func (c *CompactCmd) Name() string                { return "compact" }
func (c *CompactCmd) MutatesEngine([]string) bool { return true }
func (c *CompactCmd) Aliases() []string           { return nil }
func (c *CompactCmd) Description() string         { return "压缩对话历史" }
func (c *CompactCmd) Help() string {
	return "/compact - 总结早期消息以释放上下文窗口"
}
func (c *CompactCmd) Execute(ctx context.Context, in Input) (Output, error) {
	return Output{Message: "请从 REPL 内置路径使用 /compact。"}, nil
}

func (c *CostCmd) Name() string        { return "cost" }
func (c *CostCmd) Aliases() []string   { return nil }
func (c *CostCmd) Description() string { return "查看用量和费用" }
func (c *CostCmd) Help() string {
	return `/cost                 本次会话用量，以及本月按供应商/模型汇总的调用数、token 和费用
/cost 2026-09         指定月份；/cost 30d、/cost 24h、/cost today 近一段时间；/cost all 全部
/cost [范围] export   把该范围的逐次调用导出为 CSV（~/.cove/usage/）`
}
func (c *CostCmd) Execute(ctx context.Context, in Input) (Output, error) {
	var sb strings.Builder
	if in.Engine != nil {
		sb.WriteString("本次会话: " + in.Engine.CostTracker().Summary() + "\n")
	}
	report, err := cost.RenderUsage(in.Args, time.Now())
	sb.WriteString(report)
	if err != nil {
		sb.WriteString("\x1b[33m" + err.Error() + "\x1b[0m\n")
	}
	return Output{Message: sb.String()}, nil
}

func (c *ResumeCmd) Name() string                     { return "resume" }
func (c *ResumeCmd) MutatesEngine(args []string) bool { return len(args) > 0 }
func (c *ResumeCmd) Aliases() []string                { return nil }
func (c *ResumeCmd) Description() string              { return "恢复已保存的会话" }
func (c *ResumeCmd) Help() string {
	return "/resume [session-id|all] - 列出当前项目目录的会话，或按 ID 恢复会话（可恢复其他项目的会话，会给出提示）；/resume all 列出所有项目的会话"
}
func (c *ResumeCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.SessionStore == nil {
		return Output{Message: "会话存储不可用"}, nil
	}
	if len(in.Args) == 0 || isAllSessionsArg(in.Args) {
		records, hidden, err := listSessionsForView(in.SessionStore, in.Cwd, len(in.Args) > 0)
		if err != nil {
			return Output{}, err
		}
		var sb strings.Builder
		if len(records) == 0 {
			sb.WriteString("暂无已保存的会话\n")
		} else {
			sb.WriteString("已保存的会话:\n")
			for _, r := range records {
				fmt.Fprintf(&sb, "- %s  %s  (%d tokens)\n", r.ID, r.Title, r.TokensIn+r.TokensOut)
			}
		}
		writeHiddenSessionsHint(&sb, hidden, "/resume all")
		return Output{Message: sb.String()}, nil
	}
	r, err := in.SessionStore.Load(in.Args[0])
	if err != nil {
		return Output{}, err
	}
	// An explicit ID is honored whatever project it came from; the warning
	// is what tells the user the conversation is about another codebase.
	warning := session.ProjectMismatchWarning(r, in.Cwd)
	// Continue the saved session under its own ID when the engine can; loading
	// only its messages saved every resume as a new copy, and the original
	// never grew.
	if s, ok := in.Engine.(sessionResumer); ok {
		s.ResumeSession(r)
	} else if in.Engine != nil {
		in.Engine.LoadMessages(r.Messages)
	}
	return Output{Message: warning + fmt.Sprintf("已恢复: %s (%d 条消息, %d tokens)", r.Title, len(r.Messages), r.TokensIn+r.TokensOut)}, nil
}

// isAllSessionsArg reports whether args ask for every project's sessions
// rather than just the current project's.
func isAllSessionsArg(args []string) bool {
	return len(args) == 1 && strings.EqualFold(args[0], "all")
}

// listSessionsForView returns the sessions a list command shows: the current
// project's by default, every saved session when all is set. hidden counts
// the sessions the project view left out, so the list can say they exist.
func listSessionsForView(store SessionStore, cwd string, all bool) (records []session.Record, hidden int, err error) {
	records, err = store.List()
	if err != nil || all {
		return records, 0, err
	}
	project := session.FilterByProject(records, cwd)
	return project, len(records) - len(project), nil
}

// writeHiddenSessionsHint tells the user where the other projects' (and
// legacy) sessions went, so the per-project default never looks like lost
// history.
func writeHiddenSessionsHint(sb *strings.Builder, hidden int, allCmd string) {
	if hidden > 0 {
		fmt.Fprintf(sb, "\n另有 %d 个其他项目或旧版本的会话未显示，使用 %s 查看全部。\n", hidden, allCmd)
	}
}

func (c *HistoryCmd) Name() string        { return "history" }
func (c *HistoryCmd) Aliases() []string   { return nil }
func (c *HistoryCmd) Description() string { return "查看或选回历史会话" }
func (c *HistoryCmd) Help() string {
	return "/history [all] - 列出当前项目目录的历史会话；/history all 列出所有项目的会话（含未记录目录的旧版会话）"
}
func (c *HistoryCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.SessionStore == nil {
		return Output{Message: "会话存储不可用"}, nil
	}
	all := isAllSessionsArg(in.Args)
	records, hidden, err := listSessionsForView(in.SessionStore, in.Cwd, all)
	if err != nil {
		return Output{}, err
	}
	if len(records) == 0 {
		var sb strings.Builder
		sb.WriteString("暂无已保存的会话\n")
		writeHiddenSessionsHint(&sb, hidden, "/history all")
		return Output{Message: sb.String()}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "历史记录 (%d 个会话):\n\n", len(records))
	limit := 20
	if len(records) < limit {
		limit = len(records)
	}
	for i, r := range records[:limit] {
		msgCount := r.MessageCount
		if msgCount == 0 && len(r.Messages) > 0 {
			msgCount = len(r.Messages)
		}
		title := r.Title
		if title == "New session" || title == "" {
			// Use preview from first user message
			preview := ""
			for _, m := range r.Messages {
				if m.Role == "user" && m.Content != "" {
					content := strings.ReplaceAll(m.Content, "\n", " ")
					if len(content) > 50 {
						content = textutil.ClipRunes(content, 53)
					}
					preview = content
					break
				}
			}
			if preview != "" {
				title = preview
			}
		}
		if len(title) > 50 {
			title = textutil.ClipRunes(title, 53)
		}
		fmt.Fprintf(&sb, "  %d. [%s] %s  (%d 条)\n", i+1, r.UpdatedAt.Format("01-02 15:04"), title, msgCount)
	}
	if len(records) > limit {
		fmt.Fprintf(&sb, "\n  ... 还有 %d 条。使用 Ctrl+R 查看全部。\n", len(records)-limit)
	}
	writeHiddenSessionsHint(&sb, hidden, "/history all")
	sb.WriteString("\n提示: 在 TUI 界面中使用 Ctrl+R 可唤起选择浮层直接选回历史会话。\n")
	return Output{Message: sb.String()}, nil
}
