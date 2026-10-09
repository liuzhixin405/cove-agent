package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	ctxt "github.com/liuzhixin405/cove-agent/internal/context"
)

func (c *PermissionsCmd) Name() string        { return "permissions" }
func (c *PermissionsCmd) Aliases() []string   { return nil }
func (c *PermissionsCmd) Description() string { return "查看权限模式" }
func (c *PermissionsCmd) Help() string        { return "/permissions - 查看权限模式" }
func (c *PermissionsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.PermissionManager == nil {
		return Output{Message: "权限管理器不可用"}, nil
	}
	return Output{Message: fmt.Sprintf("权限模式: %s", in.PermissionManager.Mode())}, nil
}

func (c *StatusCmd) Name() string        { return "status" }
func (c *StatusCmd) Aliases() []string   { return nil }
func (c *StatusCmd) Description() string { return "查看代理状态和会话信息" }
func (c *StatusCmd) Help() string        { return "/status - 查看当前代理状态" }
func (c *StatusCmd) Execute(ctx context.Context, in Input) (Output, error) {
	var sb strings.Builder
	sb.WriteString("=== 代理状态 ===\n")
	fmt.Fprintf(&sb, "目录: %s\n", in.Cwd)
	if src, ok := in.Engine.(StatusSource); ok {
		if id := src.SessionID(); id != "" {
			fmt.Fprintf(&sb, "会话: %s\n", id)
		}
		if m := src.Model(); m != "" {
			fmt.Fprintf(&sb, "模型: %s\n", m)
		}
		fmt.Fprintf(&sb, "模式: %s\n", src.PermissionMode())
	}
	messageCount := 0
	costSummary := ""
	if in.Engine != nil {
		messageCount = in.Engine.MessageCount()
		if tracker := in.Engine.CostTracker(); tracker != nil {
			costSummary = strings.TrimSpace(tracker.Summary())
		}
	}
	fmt.Fprintf(&sb, "消息数: %d\n", messageCount)
	if costSummary != "" {
		fmt.Fprintf(&sb, "费用: %s\n", costSummary)
	}
	return Output{Message: sb.String()}, nil
}

func (c *StatsCmd) Name() string        { return "stats" }
func (c *StatsCmd) Aliases() []string   { return nil }
func (c *StatsCmd) Description() string { return "查看会话统计" }
func (c *StatsCmd) Help() string        { return "/stats - 用量、费用、消息数" }
func (c *StatsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Engine == nil {
		return Output{Message: "会话统计不可用"}, nil
	}
	msgCount := in.Engine.MessageCount()
	return Output{Message: fmt.Sprintf("消息数: %d\n费用: %s", msgCount, in.Engine.CostTracker().Summary())}, nil
}

// MutatesEngine: /export walks the live message history, which only the turn
// goroutine may touch while a task runs.
func (c *ExportCmd) MutatesEngine([]string) bool { return true }

func (c *ExportCmd) Name() string        { return "export" }
func (c *ExportCmd) Aliases() []string   { return nil }
func (c *ExportCmd) Description() string { return "导出对话到文件" }
func (c *ExportCmd) Help() string        { return "/export [文件名] - 导出聊天记录" }
func (c *ExportCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Engine == nil {
		return Output{Message: "引擎不可用"}, nil
	}
	filename := filepath.Join(in.Cwd, "conversation.md")
	if len(in.Args) > 0 {
		filename = in.Args[0]
	}
	if !filepath.IsAbs(filename) {
		filename = filepath.Join(in.Cwd, filename)
	}
	var sb strings.Builder
	sb.WriteString("# 对话导出\n\n")
	for _, m := range in.Engine.Messages() {
		fmt.Fprintf(&sb, "**%s**: %s\n\n", m.Role, m.Content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&sb, "- tool: %s %v\n", tc.Name, tc.Input)
		}
		if len(m.ToolCalls) > 0 {
			sb.WriteString("\n")
		}
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return Output{}, err
	}
	if err := os.WriteFile(filename, []byte(sb.String()), 0644); err != nil {
		return Output{}, err
	}
	return Output{Message: fmt.Sprintf("已导出 %d 条消息到 %s", len(in.Engine.Messages()), filename)}, nil
}

func (c *SystemCmd) Name() string        { return "system" }
func (c *SystemCmd) Aliases() []string   { return nil }
func (c *SystemCmd) Description() string { return "查看或设置自定义系统提示词" }
func (c *SystemCmd) Help() string {
	return "/system [提示词] - 查看或设置自定义系统提示词"
}
func (c *SystemCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Config == nil {
		return Output{Message: "配置不可用"}, nil
	}
	if len(in.Args) == 0 {
		if in.Config.SystemPrompt == "" {
			return Output{Message: "未设置自定义系统提示词"}, nil
		}
		return Output{Message: in.Config.SystemPrompt}, nil
	}
	prompt := strings.Join(in.Args, " ")
	in.Config.SystemPrompt = prompt
	if in.SaveConfig != nil {
		if err := in.SaveConfig(in.Config); err != nil {
			return Output{}, err
		}
	}
	// The saved value is added to the built-in system prompt as the user's
	// instructions. This used to call SetSystemOverride, which replaced the
	// whole prompt for the rest of the session: the model lost its role, tool
	// rules and project context, and behaved differently from the next start.
	if s, ok := in.Engine.(instructionsSetter); ok && s.SetCustomInstructions(prompt) {
		return Output{Message: fmt.Sprintf("系统提示词已更新 (%d 字符)", len([]rune(prompt)))}, nil
	}
	return Output{Message: fmt.Sprintf("系统提示词已保存 (%d 字符)，下次启动 cove 时生效", len([]rune(prompt)))}, nil
}

func (c *CdCmd) Name() string                { return "cd" }
func (c *CdCmd) MutatesEngine([]string) bool { return true }
func (c *CdCmd) Aliases() []string           { return nil }
func (c *CdCmd) Description() string         { return "切换工作目录" }
func (c *CdCmd) Help() string                { return "/cd <路径> - 切换当前工作目录" }
func (c *CdCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if len(in.Args) == 0 {
		return Output{Message: fmt.Sprintf("当前: %s", in.Cwd)}, nil
	}
	// The line reaches us split on whitespace, so a Windows path such as
	// "D:\My Projects" arrived as two args and only "D:\My" was tried.
	path := strings.Trim(strings.Join(in.Args, " "), `"'`)
	if !filepath.IsAbs(path) {
		path = filepath.Join(in.Cwd, path)
	}
	if err := os.Chdir(path); err != nil {
		return Output{Message: fmt.Sprintf("错误: %v", err)}, nil
	}
	wd, _ := os.Getwd()
	if in.ProjectContext != nil {
		*in.ProjectContext = *ctxt.Collect()
	}
	if s, ok := in.Engine.(workingDirSetter); ok && s.SetWorkingDir(wd) {
		return Output{Message: fmt.Sprintf("已切换到: %s", wd) + policyLoadWarning(in.Engine)}, nil
	}
	return Output{Message: fmt.Sprintf("已切换到: %s\n注意: 检查点 (/undo)、会话所属项目和自动验证仍按启动目录处理；要完整切换项目请在新目录重新启动 cove。", wd)}, nil
}

// policyLoadErrorer reports why the engine's last load of policies.json
// failed (nil when it loaded or does not exist); /cd reloads that file.
type policyLoadErrorer interface {
	PolicyLoadError() error
}

// policyLoadWarning is the line /cd adds when the engine could not reload
// policies.json for the new directory: the rules of the previous load stay in
// effect and the new project's are not applied, so say so instead of letting
// its deny rules silently not apply.
func policyLoadWarning(eng any) string {
	p, ok := eng.(policyLoadErrorer)
	if !ok {
		return ""
	}
	err := p.PolicyLoadError()
	if err == nil {
		return ""
	}
	return "\n警告: 权限规则文件无法加载，新项目的规则未生效，仍沿用切换前已加载的规则（修复文件后再次 /cd 即可重新加载）: " + err.Error()
}

// contextStructureTimeout bounds how long /context waits for the file tree
// and repo map it builds on first use.
const contextStructureTimeout = 5 * time.Second

func (c *ContextCmd) Name() string        { return "context" }
func (c *ContextCmd) Aliases() []string   { return nil }
func (c *ContextCmd) Description() string { return "查看会话上下文 (git/文件/预算)" }
func (c *ContextCmd) Help() string        { return "/context - 查看 git 状态和项目上下文" }
func (c *ContextCmd) Execute(ctx context.Context, in Input) (Output, error) {
	pc := in.ProjectContext
	if pc == nil {
		collected := ctxt.Collect()
		pc = collected
	}
	if pc.IsGitRepo {
		pc.RefreshGit()
	}
	var sb strings.Builder
	sb.WriteString("=== 会话上下文 ===\n")
	fmt.Fprintf(&sb, "目录: %s\n", pc.Cwd)
	fmt.Fprintf(&sb, "平台: %s\n", pc.Platform)
	fmt.Fprintf(&sb, "Shell: %s\n", pc.Shell)
	if pc.IsGitRepo {
		branch, status := pc.GetGitInfo()
		fmt.Fprintf(&sb, "Git: %s (%s)\n", branch, status)
	}
	// How full the model's window is: what decides when history is compacted.
	if cu, ok := in.Engine.(interface{ ContextUsage() (int, int) }); ok {
		if tokens, window := cu.ContextUsage(); window > 0 {
			fmt.Fprintf(&sb, "上下文占用: %d / %d tokens（%d%%）\n", tokens, window, tokens*100/window)
		}
	}
	// Startup no longer builds the file tree and repo map; they are built
	// here on first use, waiting at most contextStructureTimeout.
	tree, repoMap, ok := pc.Structure(contextStructureTimeout)
	if !ok {
		sb.WriteString("\n项目结构与代码大纲仍在生成（超过 5 秒），稍后再运行 /context 查看。\n")
	}
	if tree != "" {
		sb.WriteString("\n项目结构:\n")
		sb.WriteString(tree)
		if !strings.HasSuffix(tree, "\n") {
			sb.WriteString("\n")
		}
	}
	if repoMap != "" {
		sb.WriteString("\n代码大纲地图 (Repo Map):\n")
		sb.WriteString(repoMap)
		if !strings.HasSuffix(repoMap, "\n") {
			sb.WriteString("\n")
		}
	}
	return Output{Message: sb.String()}, nil
}
