package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/plugin"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

func showConfig() {
	// The same profile the session runs with (--profile x, else
	// active_profile): config.Load() showed the base file's model and
	// provider, which is not what `cove --profile work --config` asked.
	cfg, err := config.LoadWithProfile(profileName)
	if err != nil || cfg == nil {
		cfg, _ = config.Load()
	}
	pc := cfg.EffectiveProvider()
	data, _ := json.MarshalIndent(map[string]any{
		"version":         Version,
		"model":           cfg.Model,
		"provider":        pc.Name,
		"base_url":        pc.BaseURL,
		"permission_mode": cfg.PermissionMode,
		"max_budget_usd":  cfg.MaxBudgetUsd,
		"debug":           cfg.Debug,
		"api_key_set":     pc.APIKey != "",
		"mcp_servers":     len(cfg.MCPServers),
	}, "", "  ")
	outln(string(data))
}

func providerHelpLine() string {
	return "  /provider <名称>    设置供应商 (anthropic, deepseek, openai, openai-compatible, glm, kimi, qwen, doubao, openrouter, siliconflow, groq, together, fireworks, xai, mistral)"
}

func providerEnvHelpLine() string {
	return "环境变量: LLM_API_KEY | ANTHROPIC_API_KEY | DEEPSEEK_API_KEY | OPENAI_API_KEY | GLM_API_KEY | KIMI_API_KEY | QWEN_API_KEY | OPENROUTER_API_KEY | SILICONFLOW_API_KEY | LLM_BASE_URL"
}

// genericCategories places the generic commands of internal/command in the
// /help sections; the front-end commands declare theirs (command.Categorized).
var genericCategories = map[string]string{
	"ratelimit": catModel, "undo": catSession, "checkpoints": catSession, "memory": catSession,
	"status": catSession, "stats": catSession, "system": catSession,
	"commit": catSession, "review": catSession, "diff": catSession,
	"cd": catSession, "context": catSession, "init": catSession, "dream": catSession,
	"mcp": catSystem, "plugin": catSystem, "hooks": catSystem, "diagnose": catSystem, "permissions": catSystem,
}

// commandCategory is the /help section of c ("" for the last one, 命令).
func commandCategory(c command.Command) string {
	if cc, ok := c.(command.Categorized); ok && cc.Category() != "" {
		return cc.Category()
	}
	return genericCategories[c.Name()]
}

// printCommandHelp is /help <命令>: the command's own help text, its aliases
// and its first-argument hints. An alias works as a name. /help used to
// ignore its argument, while /remote's error told people to read "/help
// remote".
func printCommandHelp(reg *command.Registry, name string) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	c, ok := reg.Find(name)
	if !ok {
		outf("未找到命令 /%s。%s\n", name, closestCommandHint(reg, name))
		return
	}
	outf("/%s  %s\n", c.Name(), c.Description())
	if a := c.Aliases(); len(a) > 0 {
		outln("别名: /" + strings.Join(a, ", /"))
	}
	if h, ok := c.(command.ArgHinter); ok && len(h.ArgHints()) > 0 {
		outln("子命令: " + strings.Join(h.ArgHints(), " | "))
	}
	if help := strings.TrimSpace(c.Help()); help != "" && help != "/"+c.Name()+" - "+c.Description() {
		outln()
		outln(help)
	}
}

// closestCommandHint names registered commands whose name contains what
// was typed (or the other way round), or points at /help.
func closestCommandHint(reg *command.Registry, name string) string {
	var near []string
	if name != "" {
		for _, c := range reg.All() {
			if strings.Contains(c.Name(), name) || strings.Contains(name, c.Name()) {
				near = append(near, "/"+c.Name())
			}
		}
	}
	if len(near) == 0 {
		return "输入 /help 查看全部命令。"
	}
	return "是不是：" + strings.Join(near, "、") + "？"
}

// printHelp lists every registered command, by section: /help used to be a
// hand-written list next to the registry, and fell behind it.
func printHelp(cmdReg *command.Registry, toolReg *tool.Registry, pluginMgr *plugin.Manager) {
	outln("\n=== cove v" + Version + " ===")
	sections := map[string][]command.Command{}
	for _, c := range cmdReg.All() {
		cat := commandCategory(c)
		sections[cat] = append(sections[cat], c)
	}
	for _, cat := range append(append([]string(nil), helpCategories...), "") {
		cmds := sections[cat]
		if len(cmds) == 0 {
			continue
		}
		title := cat
		if title == "" {
			title = "命令"
		}
		outln("\n" + title + ":")
		for _, c := range cmds {
			name := "/" + c.Name()
			if a := c.Aliases(); len(a) > 0 {
				name += " (/" + strings.Join(a, ", /") + ")"
			}
			outf("  %-18s %s\n", name, c.Description())
		}
		if cat == catModel {
			outln(providerHelpLine())
		}
	}
	if pluginMgr != nil {
		if pcmds := pluginMgr.CommandPrompts(); len(pcmds) > 0 {
			outln("\n插件命令:")
			for name, c := range pcmds {
				outf("  /%-16s %s (%s)\n", name, c.Description, c.Plugin)
			}
		}
	}
	outln("\n工具:")
	for _, t := range toolReg.All() {
		d := t.Def()
		ro := " "
		if d.IsReadOnly {
			ro = "R"
		}
		outf("  [%s] %-12s %s\n", ro, d.Name, truncateDesc(d.Description, 48))
	}
	outln("\n" + providerEnvHelpLine())
	outln("启动参数: -p <提示> [--image <路径>] [--file <路径>] | -r <会话ID> | --profile <name> | --no-tui | -d --debug | -v --version | --doctor | --config（完整列表: cove --help）")
	outln("附件输入: 在 REPL 或 -p 文本中可写 @路径，例如：解释这张图 @assets/screen.png")
	outln("图片输入: 支持括号粘贴的终端可拖入图片；Windows 用 Alt+V 粘贴截图；空输入框 Alt+Backspace 移除最后一个附件。")
	outln("命令选择: 输入 / 筛选，上下键选择，Enter 填入；/history 支持搜索和预览。")
	outln("Agent Map: Alt+M 展开/收起，空输入 Tab 或 Shift+Tab 切换焦点，上下键查看，Esc 返回；/agents 查看快照。")
	outln("用法详情: /help <命令>（别名也可）；输入快捷键: /keys")
	outln()
}

// printTools prints the list of available tools and their descriptions.
func printTools(toolReg *tool.Registry, pluginMgr *plugin.Manager) {
	outln("工具:")
	for _, t := range toolReg.All() {
		d := t.Def()
		ro := " "
		if d.IsReadOnly {
			ro = "R"
		}
		outf("  [%s] %-12s %s\n", ro, d.Name, truncateDesc(d.Description, 48))
	}
	outln()
}

// missingAPIKeyMessage is the three-line notice for a run without a key.
// The multi-provider walkthrough it used to be lives in the setup wizard.
func missingAPIKeyMessage(provider string) string {
	provider = api.NormalizeProviderName(strings.TrimSpace(provider))
	if provider == "" {
		provider = "anthropic"
	}
	primaryEnv := "LLM_API_KEY"
	if envs := api.ProviderEnvCandidates(provider); len(envs) > 0 {
		primaryEnv = envs[0]
	}
	path := "~/.cove/config.json"
	if dir, err := config.ConfigDir(); err == nil {
		path = filepath.Join(dir, "config.json")
	}
	return fmt.Sprintf("未配置 API key（当前供应商：%s）。\n输入 /setup 进入配置向导（选供应商、输入 key、自动验证）。\n也可以设置环境变量 %s，或编辑 %s 后 /restart。", provider, primaryEnv, path)
}
