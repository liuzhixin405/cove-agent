package main

import (
	"encoding/json"
	"fmt"
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
		"thinking_tokens": cfg.ThinkingTokens,
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
	"mcp": catSystem, "plugin": catSystem, "hooks": catSystem, "diagnose": catSystem, "permissions": catSystem,
}

// commandCategory is the /help section of c ("" for the last one, 命令).
func commandCategory(c command.Command) string {
	if cc, ok := c.(command.Categorized); ok && cc.Category() != "" {
		return cc.Category()
	}
	return genericCategories[c.Name()]
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
	outln("启动参数: -p <提示> [--image <路径>] [--file <路径>] | -r <会话ID> | --profile <name> | --record <dir> | --replay <dir> | --no-tui | -d --debug | -v --version | --doctor | --config（完整列表: cove --help）")
	outln("附件输入: 在 REPL 或 -p 文本中可写 @路径，例如：解释这张图 @assets/screen.png")
	outln("图片输入: 支持括号粘贴的终端可拖入图片；Windows 用 Alt+V 粘贴截图；空输入框 Alt+Backspace 移除最后一个附件。")
	outln("命令选择: 输入 / 筛选，上下键选择，Enter 填入；/history 支持搜索和预览。")
	outln("Agent Map: Alt+M 展开/收起，空输入 Tab 或 Shift+Tab 切换焦点，上下键查看，Esc 返回；/agents 查看快照。")
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

func missingAPIKeyMessage(provider string) string {
	provider = api.NormalizeProviderName(strings.TrimSpace(provider))
	if provider == "" {
		provider = "anthropic"
	}
	providerEnvCandidates := api.ProviderEnvCandidates(provider)
	primaryEnv := "LLM_API_KEY"
	if len(providerEnvCandidates) > 0 {
		primaryEnv = providerEnvCandidates[0]
	}
	openAICompatList := "glm, kimi, qwen, doubao, openrouter, siliconflow, groq, together, fireworks, xai, mistral"
	return fmt.Sprintf(
		"No API key configured / 未配置 API key.\n"+
			"先看当前厂商：%s\n\n"+
			"最快的办法：直接在当前 REPL 输入\n"+
			"  /api-key <你的key>\n\n"+
			"如果你用 Claude / Anthropic：设置 %s\n"+
			"如果你用 DeepSeek：设置 DEEPSEEK_API_KEY\n"+
			"如果你用 OpenAI：设置 OPENAI_API_KEY\n\n"+
			"如果你用 GLM / Kimi / Qwen / 豆包 / OpenRouter / 硅基流动 / Groq / Together / Fireworks / xAI / Mistral 这类兼容 OpenAI 的接口：\n"+
			"  1) /provider openai-compatible  （或直接 /provider 对应厂商名）\n"+
			"  2) /base-url <兼容 OpenAI 的接口地址>\n"+
			"  3) /api-key <你的key>\n\n"+
			"例如 GLM：        /provider glm          + GLM_API_KEY / ZHIPU_API_KEY\n"+
			"例如 Kimi：       /provider kimi         + KIMI_API_KEY / MOONSHOT_API_KEY\n"+
			"例如 Qwen：       /provider qwen         + QWEN_API_KEY / DASHSCOPE_API_KEY\n"+
			"例如 豆包：       /provider doubao       + DOUBAO_API_KEY / ARK_API_KEY\n"+
			"例如 OpenRouter： /provider openrouter   + OPENROUTER_API_KEY\n"+
			"例如 硅基流动：   /provider siliconflow + SILICONFLOW_API_KEY\n\n"+
			"当前内置适配 provider：anthropic, deepseek, openai, openai-compatible, %s\n"+
			"也可用通用变量：LLM_API_KEY\n"+
			"设置后执行 /config，确认 api_key_set: true。",
		provider,
		primaryEnv,
		openAICompatList,
	)
}
