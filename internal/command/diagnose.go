package command

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/diagnostic"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

type diagnoseCmd struct{}

func NewDiagnoseCmd() Command { return &diagnoseCmd{} }

func (c *diagnoseCmd) Name() string        { return "diagnose" }
func (c *diagnoseCmd) Aliases() []string   { return []string{"diag"} }
func (c *diagnoseCmd) Description() string { return "运行系统诊断，检查并修复常见问题" }
func (c *diagnoseCmd) Help() string {
	return `/diagnose        运行完整系统诊断（含网络检测）
/diagnose quick  仅运行快速检查（跳过网络）
/diagnose errors 查看运行时记录的错误/卡顿及修复建议
/diagnose archive 修复完成后归档错误日志，开始新的记录周期
/diagnose codes  列出所有诊断码，带处置器的标注 (有处置器)
/diagnose trace [N] 查看最近 N 条交互轨迹（模型调用、工具调用、压缩，默认 30 条）`
}

func (c *diagnoseCmd) Execute(ctx context.Context, input Input) (Output, error) {
	cfg := input.Config
	checker := diagnostic.NewChecker(cfg)

	mode := "full"
	if len(input.Args) > 0 {
		mode = input.Args[0]
	}

	switch mode {
	case "codes":
		return c.listCodes()
	case "trace":
		return c.showTrace(input.Args[1:])
	case "errors", "log", "recent":
		return c.showRuntimeErrors()
	case "archive", "fixed", "clear":
		return c.archiveRuntimeLog()
	case "quick":
		report := checker.RunQuick()
		return Output{Message: report.Format() + webSearchHint(cfg)}, nil
	default:
		report := checker.RunAll(ctx)
		msg := report.Format()
		// Append a runtime-error reminder so recurring hangs/failures from this
		// session (and previous ones) surface alongside the static checks.
		msg += webSearchHint(cfg)
		msg += c.runtimeReminder()
		return Output{Message: msg}, nil
	}
}

// showRuntimeErrors lists problems recorded while the agent was running,
// merged with persisted events from previous runs: one line per coded
// problem and model (or per uncoded message), with count, latest time, the
// catalogue's hint and what a remedy did.
func (c *diagnoseCmd) showRuntimeErrors() (Output, error) {
	// Both the persisted log (earlier runs) and this session's buffer, once
	// each: the start-up hint points here for the log's problems, so the
	// log must be shown even when this session already recorded something.
	events := diagnostic.WithoutAcks(diagnostic.MergeEvents(diagnostic.LoadRuntimeLog(), diagnostic.RecentRuntime()))
	if len(events) == 0 {
		return Output{Message: "\x1b[32m✓ 没有记录到运行时错误或卡顿。\x1b[0m\n"}, nil
	}
	summaries := diagnostic.SummarizeRuntime(events)
	const reset = "\x1b[0m"
	var sb strings.Builder
	fmt.Fprintf(&sb, "\x1b[1m运行时问题记录\x1b[0m (共 %d 条，按严重程度排序)\n\n", len(events))
	for _, s := range summaries {
		count := ""
		if s.Count > 1 {
			count = fmt.Sprintf(" \x1b[2m×%d\x1b[0m", s.Count)
		}
		last := ""
		if !s.Last.IsZero() {
			last = fmt.Sprintf("   \x1b[2m最近 %s\x1b[0m", s.Last.Format("01-02 15:04"))
		}
		code := ""
		if s.Code != "" {
			code = string(s.Code) + " "
		}
		fmt.Fprintf(&sb, " %s[%s]%s %s%s%s%s\n", s.Severity.Color(), s.Severity.String(), reset, code, s.Message, count, last)
		if s.Code != "" {
			ctx := ""
			if s.Model != "" {
				ctx = "模型 " + s.Model
			}
			if s.Detail != "" {
				if ctx != "" {
					ctx += " · "
				}
				ctx += textutil.ClipRunes(s.Detail, 100)
			}
			if ctx != "" {
				fmt.Fprintf(&sb, "    \x1b[2m%s\x1b[0m\n", ctx)
			}
		}
		if s.Recovery != "" {
			fmt.Fprintf(&sb, "    \x1b[33m💡 %s\x1b[0m\n", s.Recovery)
		}
		for _, a := range s.Applied {
			fmt.Fprintf(&sb, "    \x1b[32m✓ 已处置：%s\x1b[0m\n", a)
		}
	}
	sb.WriteString("\n\x1b[2m日志文件: ~/.cove/errors.log · 处理完可用 /diagnose archive 归档\x1b[0m\n")
	// Seen: the start-up hint now only reports problems recorded after this.
	diagnostic.AckRuntimeLog()
	return Output{Message: sb.String()}, nil
}

// archiveRuntimeLog archives the current error log and starts a fresh cycle,
// to be run after the reported problems have been fixed.
func (c *diagnoseCmd) archiveRuntimeLog() (Output, error) {
	dest, err := diagnostic.ArchiveRuntimeLog()
	if err != nil {
		return Output{Message: fmt.Sprintf("\x1b[31m归档失败: %s\x1b[0m\n", err.Error())}, nil
	}
	diagnostic.ResetRemedyState()
	if dest == "" {
		return Output{Message: "\x1b[32m✓ 当前没有需要归档的错误日志，已开始新的记录周期。\x1b[0m\n"}, nil
	}
	return Output{Message: fmt.Sprintf("\x1b[32m✓ 错误日志已归档至 %s，已开始新的记录周期。\x1b[0m\n", dest)}, nil
}

// runtimeReminder returns a short reminder block when there are recorded
// runtime problems, or an empty string otherwise.
func (c *diagnoseCmd) runtimeReminder() string {
	events := diagnostic.RecentRuntime()
	if len(events) == 0 {
		return ""
	}
	summaries := diagnostic.SummarizeRuntime(events)
	if len(summaries) == 0 {
		return ""
	}
	const reset = "\x1b[0m"
	msg := "\n\x1b[1m运行时问题提醒\x1b[0m (本次会话，详见 /diagnose errors)\n"
	shown := 0
	for _, s := range summaries {
		if shown >= 5 {
			break
		}
		count := ""
		if s.Count > 1 {
			count = fmt.Sprintf(" ×%d", s.Count)
		}
		code := ""
		if s.Code != "" {
			code = string(s.Code) + " "
		}
		msg += fmt.Sprintf("  %s%s%s %s%s%s\n", s.Severity.Color(), s.Severity.String(), reset, code, s.Message, count)
		shown++
	}
	return msg
}

func (c *diagnoseCmd) listCodes() (Output, error) {
	all := diagnostic.AllErrors()

	// Group by category
	groups := map[diagnostic.Category][]diagnostic.ErrorCode{}
	for code, def := range all {
		groups[def.Category] = append(groups[def.Category], code)
	}

	var msg string
	msg += "\x1b[1m已注册的错误代码:\x1b[0m\n\n"

	categories := []diagnostic.Category{
		diagnostic.CatConfig,
		diagnostic.CatNetwork,
		diagnostic.CatAPI,
		diagnostic.CatPermission,
		diagnostic.CatTool,
		diagnostic.CatEngine,
		diagnostic.CatSession,
		diagnostic.CatFileSystem,
	}

	for _, cat := range categories {
		codes, ok := groups[cat]
		if !ok {
			continue
		}
		// groups was filled from a map, so without sorting the order changed
		// on every call.
		sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
		msg += fmt.Sprintf("  \x1b[36m[%s]\x1b[0m\n", cat)
		for _, code := range codes {
			def := all[code]
			fixable := ""
			if def.Remedy != nil {
				fixable = " \x1b[32m(有处置器)\x1b[0m"
			}
			msg += fmt.Sprintf("    %s  %s%s\n", code, def.Message, fixable)
		}
		msg += "\n"
	}

	return Output{Message: msg}, nil
}

// webSearchHint is the /diagnose line about the websearch backend: empty when
// a search API is configured or DuckDuckGo was chosen explicitly.
func webSearchHint(cfg *config.Config) string {
	var s tool.WebSearchSettings
	if cfg != nil && cfg.WebSearch != nil {
		s = tool.WebSearchSettings{Provider: cfg.WebSearch.Provider, APIKey: cfg.WebSearch.APIKey}
	}
	provider, _ := tool.ResolveWebSearchBackend(s)
	if provider != "duckduckgo" {
		return ""
	}
	switch p := strings.ToLower(strings.TrimSpace(s.Provider)); p {
	case "duckduckgo", "ddg":
		return ""
	case "tavily", "brave":
		return fmt.Sprintf("\n\x1b[33m⚠ web_search.provider 为 %s，但没有 api_key（也没有对应环境变量），websearch 已回退到 DuckDuckGo 抓取。\x1b[0m\n", p)
	}
	return "\n\x1b[2mℹ websearch 未配置搜索 API，当前使用 DuckDuckGo 抓取（结果有限）。可在 config.json 设置 web_search.provider（tavily|brave）与 api_key，或设置 TAVILY_API_KEY / BRAVE_API_KEY。\x1b[0m\n"
}
