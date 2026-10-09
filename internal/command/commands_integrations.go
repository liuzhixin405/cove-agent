package command

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/mcp"
	"github.com/liuzhixin405/cove-agent/internal/plugin"
	"github.com/liuzhixin405/cove-agent/internal/skills"
)

func (c *McpCmd) Name() string        { return "mcp" }
func (c *McpCmd) Aliases() []string   { return nil }
func (c *McpCmd) Description() string { return "管理 MCP 服务器" }
func (c *McpCmd) Help() string        { return "/mcp [list|connect|disconnect|read] - 管理 MCP 服务器" }
func (c *McpCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.MCPPool == nil {
		return Output{Message: "MCP 连接池不可用"}, nil
	}
	action := "list"
	if len(in.Args) > 0 {
		action = in.Args[0]
	}
	switch action {
	case "list":
		servers := in.MCPPool.AllServers()
		if len(servers) == 0 {
			return Output{Message: "暂无已连接的 MCP 服务器"}, nil
		}
		var sb strings.Builder
		for _, s := range servers {
			typeName := s.Config.Type
			if typeName == "" {
				typeName = "stdio"
			}
			fmt.Fprintf(&sb, "- %s [%s] connected=%t tools=%d resources=%d\n", s.Name, typeName, s.Connected, len(s.Tools), len(s.Resources))
			// Connection errors are only logged at debug level, so this is
			// the one place the user can see why a server is down.
			if s.Err != "" {
				fmt.Fprintf(&sb, "  错误: %s\n", s.Err)
			}
		}
		return Output{Message: sb.String()}, nil
	case "disconnect":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /mcp disconnect <名称|all>"}, nil
		}
		if in.Args[1] == "all" {
			in.MCPPool.DisconnectAll()
			return Output{Message: "已断开所有 MCP 服务器"}, nil
		}
		// Disconnect ignores unknown names; without this check a typo was
		// reported as a successful disconnect.
		known := false
		for _, s := range in.MCPPool.AllServers() {
			if s.Name == in.Args[1] {
				known = true
				break
			}
		}
		if !known {
			return Output{Message: fmt.Sprintf("未找到 MCP 服务器 '%s'（/mcp list 查看）", in.Args[1])}, nil
		}
		in.MCPPool.Disconnect(in.Args[1])
		return Output{Message: fmt.Sprintf("已断开 MCP 服务器: %s", in.Args[1])}, nil
	case "connect":
		if in.Config == nil {
			return Output{Message: "配置不可用"}, nil
		}
		if len(in.Args) < 2 {
			return Output{Message: "用法: /mcp connect <名称>"}, nil
		}
		cfg, ok := in.Config.MCPServers[in.Args[1]]
		if !ok {
			return Output{Message: fmt.Sprintf("配置中未找到 MCP 服务器 '%s'", in.Args[1])}, nil
		}
		if err := in.MCPPool.Connect(ctx, in.Args[1], mcp.ServerConfig(cfg)); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("已连接 MCP 服务器: %s", in.Args[1])}, nil
	case "read":
		if len(in.Args) < 3 {
			return Output{Message: "用法: /mcp read <服务器> <资源URI>"}, nil
		}
		result, err := in.MCPPool.ReadResource(ctx, in.Args[1], in.Args[2])
		if err != nil {
			return Output{}, err
		}
		if result == nil || len(result.Contents) == 0 {
			return Output{Message: fmt.Sprintf("MCP 资源无内容: %s", in.Args[2])}, nil
		}
		var sb strings.Builder
		for _, block := range result.Contents {
			if block.Text != "" {
				sb.WriteString(block.Text)
				if !strings.HasSuffix(block.Text, "\n") {
					sb.WriteString("\n")
				}
				continue
			}
			if block.Data != "" {
				sb.WriteString(block.Data)
				if !strings.HasSuffix(block.Data, "\n") {
					sb.WriteString("\n")
				}
			}
		}
		text := strings.TrimSpace(sb.String())
		if text == "" {
			return Output{Message: fmt.Sprintf("MCP 资源 %s 返回了 %d 个内容块", in.Args[2], len(result.Contents))}, nil
		}
		return Output{Message: text}, nil
	default:
		return Output{Message: "用法: /mcp [list|connect|disconnect|read]"}, nil
	}
}

func (c *PluginCmd) Name() string        { return "plugin" }
func (c *PluginCmd) Aliases() []string   { return nil }
func (c *PluginCmd) Description() string { return "管理插件" }
func (c *PluginCmd) Help() string {
	return "/plugin [list|install|search|refresh|update|enable|disable|uninstall] - 管理插件"
}
func (c *PluginCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.PluginManager == nil {
		return Output{Message: "插件管理器不可用"}, nil
	}
	action := "list"
	if len(in.Args) > 0 {
		action = in.Args[0]
	}
	switch action {
	case "list":
		if refresher, ok := in.PluginManager.(interface{ Refresh() }); ok {
			refresher.Refresh()
		}
		plugins := in.PluginManager.AllPlugins()
		if len(plugins) == 0 {
			dir := "~/.cove/plugins"
			if withDir, ok := in.PluginManager.(interface{ Dir() string }); ok {
				dir = withDir.Dir()
			}
			return Output{Message: fmt.Sprintf("暂无已安装插件\n插件目录: %s\n安装: /plugin install <名称> [url]", dir)}, nil
		}
		sort.Slice(plugins, func(i, j int) bool { return plugins[i].Manifest.Name < plugins[j].Manifest.Name })
		var sb strings.Builder
		sb.WriteString("已安装插件:\n")
		for _, p := range plugins {
			fmt.Fprintf(&sb, "- %s %s (%s)", p.Manifest.Name, p.Manifest.Version, pluginStateLabel(p.State))
			if p.Error != "" {
				fmt.Fprintf(&sb, " - %s", p.Error)
			}
			sb.WriteString("\n")
		}
		return Output{Message: sb.String()}, nil
	case "install":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /plugin install <名称|git-url>"}, nil
		}
		name := in.Args[1]
		url := ""
		if len(in.Args) > 2 {
			url = in.Args[2]
		}
		if isPluginGitURL(name) {
			url = name
			name = pluginNameFromURL(url)
		} else if at := strings.IndexByte(name, '@'); at > 0 {
			name = name[:at]
		}

		if url == "" {
			type marketInstaller interface {
				MarketplaceInstall(name string) error
			}
			if mi, ok := in.PluginManager.(marketInstaller); ok {
				var err error
				if contextual, ok := in.PluginManager.(interface {
					MarketplaceInstallContext(context.Context, string) error
				}); ok {
					err = contextual.MarketplaceInstallContext(ctx, name)
				} else {
					err = mi.MarketplaceInstall(name)
				}
				if err == nil {
					return Output{Message: fmt.Sprintf("✓ 已从 marketplace 安装: %s", name)}, nil
				}
				return Output{Message: fmt.Sprintf(
					"无法从 marketplace 安装 %q: %v\n如果你知道插件仓库地址，用: /plugin install %s <git-url>\n或先刷新索引: /plugin refresh",
					name, err, name)}, nil
			}
		}

		var err error
		if contextual, ok := in.PluginManager.(interface {
			InstallContext(context.Context, string, string) error
		}); ok {
			err = contextual.InstallContext(ctx, name, url)
		} else {
			err = in.PluginManager.Install(name, url)
		}
		if err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("已安装插件: %s", name)}, nil
	case "enable":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /plugin enable <名称>"}, nil
		}
		if err := in.PluginManager.Enable(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("已启用插件: %s", in.Args[1])}, nil
	case "disable":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /plugin disable <名称>"}, nil
		}
		if err := in.PluginManager.Disable(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("已禁用插件: %s", in.Args[1])}, nil
	case "uninstall", "remove", "rm":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /plugin uninstall <名称>"}, nil
		}
		if err := in.PluginManager.Uninstall(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("已卸载插件: %s", in.Args[1])}, nil
	case "search", "find":
		query := ""
		if len(in.Args) > 1 {
			query = strings.Join(in.Args[1:], " ")
		}
		type marketSearcher interface {
			MarketplaceSearch(query string) string
		}
		if ms, ok := in.PluginManager.(marketSearcher); ok {
			return Output{Message: ms.MarketplaceSearch(query)}, nil
		}
		return Output{Message: "marketplace 不可用"}, nil
	case "refresh":
		type marketRefresher interface {
			MarketplaceRefresh() error
		}
		if mr, ok := in.PluginManager.(marketRefresher); ok {
			var err error
			if contextual, ok := in.PluginManager.(interface {
				MarketplaceRefreshContext(context.Context) error
			}); ok {
				err = contextual.MarketplaceRefreshContext(ctx)
			} else {
				err = mr.MarketplaceRefresh()
			}
			if err != nil {
				return Output{Message: fmt.Sprintf("刷新marketplace索引时部分失败: %v", err)}, nil
			}
			return Output{Message: "✓ marketplace 索引已更新"}, nil
		}
		return Output{Message: "marketplace 不可用"}, nil
	case "update":
		name := ""
		if len(in.Args) > 1 {
			name = in.Args[1]
		}
		type marketUpdater interface {
			MarketplaceUpdate(name string) (string, error)
		}
		if mu, ok := in.PluginManager.(marketUpdater); ok {
			var msg string
			var err error
			if contextual, ok := in.PluginManager.(interface {
				MarketplaceUpdateContext(context.Context, string) (string, error)
			}); ok {
				msg, err = contextual.MarketplaceUpdateContext(ctx, name)
			} else {
				msg, err = mu.MarketplaceUpdate(name)
			}
			if err != nil {
				return Output{}, err
			}
			return Output{Message: msg}, nil
		}
		return Output{Message: "marketplace 不可用"}, nil
	default:
		return Output{Message: "用法: /plugin [list|install|search|refresh|update|enable|disable|uninstall]"}, nil
	}
}

func (c *SkillsCmd) Name() string        { return "skills" }
func (c *SkillsCmd) Aliases() []string   { return []string{"skill"} }
func (c *SkillsCmd) Description() string { return "查看可用技能" }
func (c *SkillsCmd) Help() string {
	return "/skills [list|名称|export 名称] - 列出技能（标注来源）、查看技能内容，或导出内置技能到 ~/.cove/skills 以便修改"
}

// skillSourceLabel names where a skill was loaded from.
func skillSourceLabel(source string) string {
	switch source {
	case skills.SourceBuiltin:
		return "[内置]"
	case skills.SourcePlugin:
		return "[插件]"
	case skills.SourceProject:
		return "[项目]"
	default:
		return "[用户]"
	}
}

func (c *SkillsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.SkillManager == nil {
		return Output{Message: "技能管理器不可用"}, nil
	}
	if len(in.Args) == 0 || in.Args[0] == "list" {
		all := in.SkillManager.All()
		if len(all) == 0 {
			return Output{Message: "暂无已加载的技能"}, nil
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
		var sb strings.Builder
		sb.WriteString("技能列表（同名时 项目 > 用户 > 插件 > 内置）:\n")
		for _, s := range all {
			fmt.Fprintf(&sb, "- %-24s %-6s %s\n", s.Name, skillSourceLabel(s.Source), s.Description)
		}
		return Output{Message: sb.String()}, nil
	}
	if in.Args[0] == "export" {
		if len(in.Args) < 2 {
			return Output{Message: "用法: /skills export <内置技能名>"}, nil
		}
		path, err := skills.ExportBuiltin(in.Args[1])
		if err != nil {
			return Output{Message: fmt.Sprintf("导出失败: %v", err)}, nil
		}
		return Output{Message: fmt.Sprintf("已导出到 %s\n重启后该副本会覆盖内置版本；它不再随 cove 升级更新，删除这个文件即可恢复内置版本。", path)}, nil
	}
	name := in.Args[0]
	s, ok := in.SkillManager.Get(name)
	if !ok {
		return Output{Message: fmt.Sprintf("未找到技能 '%s'", name)}, nil
	}
	return Output{Message: fmt.Sprintf("# %s\n\n%s", s.Name, s.Prompt)}, nil
}

// isPluginGitURL reports whether a /plugin install argument is a git URL
// rather than a marketplace name. Only https:// and git@ used to count, so an
// http:// or ssh:// URL was looked up in the marketplace as a plugin name.
func isPluginGitURL(s string) bool {
	for _, prefix := range []string{"https://", "http://", "ssh://", "git@"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// pluginNameFromURL is the last path element of a git URL without ".git".
// A trailing slash used to leave an empty name, and the scp form
// git@host:repo.git (no slash) kept the host in it.
func pluginNameFromURL(url string) string {
	s := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func pluginStateLabel(s plugin.State) string {
	switch s {
	case plugin.Enabled:
		return "已启用"
	case plugin.Disabled:
		return "已禁用"
	case plugin.Error:
		return "错误"
	default:
		return "未知"
	}
}
