package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/cost"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/permission"
)

func handleBuiltinConfigCommand(input string, cfg *config.Config, eng *engine.Engine) bool {
	switch {
	case input == "/config":
		showConfig()
		return true
	case input == "/profile" || strings.HasPrefix(input, "/profile "):
		handleProfileCommand(input, cfg, eng)
		return true
	case strings.HasPrefix(input, "/model "):
		if err := applyProviderConfigChange(cfg, eng, func() error {
			cfg.Model = config.ResolveModelForProvider(strings.TrimPrefix(input, "/model "), cfg.Provider.Name)
			return nil
		}, func() error { return config.Save(cfg) }); err != nil {
			outf("模型更新失败: %v\n", err)
			return true
		}
		outf("模型: %s（已保存）\n", cfg.Model)
		return true
	case strings.HasPrefix(input, "/provider "):
		providerName := strings.TrimSpace(strings.TrimPrefix(input, "/provider "))
		if !api.IsKnownProvider(providerName) {
			outf("无效的供应商: %s\n", providerName)
			outln(providerHelpLine())
			return true
		}
		if err := applyProviderConfigChange(cfg, eng, func() error {
			oldProvider := cfg.EffectiveProvider().Name
			cfg.Provider.Name = providerName
			cfg.Model = providerSwitchModel(oldProvider, providerName, cfg.Model)
			return nil
		}, func() error { return config.Save(cfg) }); err != nil {
			outf("供应商更新失败: %v\n", err)
			return true
		}
		outf("供应商: %s，模型: %s（已保存）\n", cfg.Provider.Name, cfg.Model)
		return true
	case strings.HasPrefix(input, "/api-key "):
		if err := applyProviderConfigChange(cfg, eng, func() error {
			cfg.Provider.APIKey = strings.TrimSpace(strings.TrimPrefix(input, "/api-key "))
			return nil
		}, func() error { return config.Save(cfg) }); err != nil {
			outf("API 密钥更新失败: %v\n", err)
			return true
		}
		outln("API 密钥已保存")
		return true
	case strings.HasPrefix(input, "/base-url "):
		if err := applyProviderConfigChange(cfg, eng, func() error {
			cfg.Provider.BaseURL = strings.TrimSpace(strings.TrimPrefix(input, "/base-url "))
			return nil
		}, func() error { return config.Save(cfg) }); err != nil {
			outf("Base URL 更新失败: %v\n", err)
			return true
		}
		outln("Base URL 已保存")
		return true
	case strings.HasPrefix(input, "/mode "):
		m := permission.Mode(strings.TrimPrefix(input, "/mode "))
		if permission.ValidMode(m) {
			eng.SetPermissionMode(m)
			cfg.PermissionMode = string(m)
			saveConfigOrSay(cfg)
			outf("模式: %s\n", m)
		} else {
			outf("无效模式。可选: %s\n", permission.Modes())
		}
		return true
	case input == "/budget" || strings.HasPrefix(input, "/budget "):
		handleBudgetCommand(input, cfg, eng)
		return true
	case input == "/cost":
		outln("本次会话:", eng.CostTracker().Summary())
		ch := cost.NewCostHistory()
		if len(ch.Records) > 0 {
			outf("近 24小时: $%.4f | 近 7天: $%.4f | 总计: $%.4f (%d 个会话)\n",
				ch.Last24Hours(), ch.Last7Days(), ch.TotalAllTime(), len(ch.Records))
		}
		return true
	default:
		return false
	}
}

// handleBudgetCommand changes the spend cap of this session only; the
// configured max_budget_usd changes with "/budget save". Every /budget used
// to rewrite the global config.json, so raising the cap for one expensive
// task raised it for every later session.
func handleBudgetCommand(input string, cfg *config.Config, eng *engine.Engine) {
	arg := strings.TrimSpace(strings.TrimPrefix(input, "/budget"))
	current := sessionBudget(cfg, eng)
	switch {
	case arg == "":
		// Bare "/budget" used to fall through to "未找到命令 /budget".
		if current > 0 {
			outf("当前预算: $%.2f（本会话）\n", current)
		} else {
			outln("当前未设置预算上限")
		}
		if cfg.MaxBudgetUsd != current {
			outf("配置中的预算: %s\n", budgetLabel(cfg.MaxBudgetUsd))
		}
		outln(budgetUsage)
		return
	case strings.EqualFold(arg, "save"):
		cfg.MaxBudgetUsd = current
		saveConfigOrSay(cfg)
		outf("已把预算 %s 写入配置\n", budgetLabel(current))
		return
	case strings.EqualFold(arg, "off"):
		setSessionBudget(0, eng)
		outln("已取消本会话的预算上限（/budget save 可写入配置）")
		return
	case strings.EqualFold(arg, "auto"):
		b := current
		if eng != nil {
			if tr := eng.CostTracker(); tr != nil {
				if suggested := tr.SuggestedBudget(); suggested > b {
					b = suggested
				}
			}
		}
		if b > 0 {
			setSessionBudget(b, eng)
			outf("本会话预算已自动调整到: $%.2f（/budget save 可写入配置）\n", b)
		}
		return
	}
	// An unparsable or non-positive amount used to be ignored without a word,
	// so the user believed a cap was set.
	b, err := strconv.ParseFloat(strings.TrimPrefix(arg, "$"), 64)
	if err != nil || b <= 0 {
		outf("无效预算: %s\n%s\n", arg, budgetUsage)
		return
	}
	setSessionBudget(b, eng)
	outf("本会话预算: $%.2f（/budget save 可写入配置）\n", b)
}

// sessionBudget is the cap this session runs under: the engine's, or the
// configured one without an engine.
func sessionBudget(cfg *config.Config, eng *engine.Engine) float64 {
	if eng != nil {
		if tr := eng.CostTracker(); tr != nil {
			return tr.Totals().MaxBudget
		}
	}
	return cfg.MaxBudgetUsd
}

func setSessionBudget(b float64, eng *engine.Engine) {
	if eng != nil {
		eng.SetMaxBudget(b)
	}
}

func budgetLabel(b float64) string {
	if b <= 0 {
		return "无上限"
	}
	return fmt.Sprintf("$%.2f", b)
}

// providerSwitchModel is the model to use after switching from oldProvider to
// newProvider. The old provider's default (or no choice at all) follows the
// switch; /provider deepseek used to keep claude-sonnet-4 and send it to
// DeepSeek. A model the user picked stays: it may be served by the new
// provider too (OpenRouter, compatible gateways).
func providerSwitchModel(oldProvider, newProvider, model string) string {
	m := strings.TrimSpace(model)
	if m == "" || strings.EqualFold(m, "auto") || m == config.DefaultModelForProvider(oldProvider) {
		return config.DefaultModelForProvider(newProvider)
	}
	return m
}

// saveConfigOrSay saves cfg and tells the user when that failed. The /mode and
// /budget paths used to discard the error, and config.Save refuses to
// overwrite a config.json it cannot parse, so a change could vanish on the
// next start while the command had reported success.
func saveConfigOrSay(cfg *config.Config) {
	if err := config.Save(cfg); err != nil {
		outf("配置保存失败（仅本次会话生效）: %v\n", err)
	}
}

const budgetUsage = "用法: /budget <金额，美元，大于 0> | /budget auto | /budget off（以上只改本会话）| /budget save（写入配置）"

func handleProfileCommand(input string, cfg *config.Config, eng *engine.Engine) {
	args := strings.Fields(strings.TrimSpace(strings.TrimPrefix(input, "/profile")))
	if len(args) == 0 || strings.EqualFold(args[0], "list") {
		names := make([]string, 0, len(cfg.Profiles))
		for name := range cfg.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 0 {
			outln("当前没有已保存的 profile。")
			return
		}
		outln("profiles:")
		for _, name := range names {
			mark := " "
			if strings.EqualFold(cfg.ActiveProfile, name) {
				mark = "*"
			}
			outf("  %s %s\n", mark, name)
		}
		return
	}

	sub := strings.ToLower(args[0])
	if len(args) < 2 {
		outln("用法: /profile list | /profile switch <name> | /profile save <name> | /profile delete <name> | /profile show <name>")
		return
	}
	name := strings.TrimSpace(args[1])
	if name == "" {
		outln("profile 名称不能为空")
		return
	}

	switch sub {
	case "switch":
		if cfg.Profiles == nil {
			outf("profile %s 不存在\n", name)
			return
		}
		if _, ok := cfg.Profiles[name]; !ok {
			outf("profile %s 不存在\n", name)
			return
		}
		loaded, err := config.LoadWithProfile(name)
		if err != nil {
			outf("加载 profile 失败: %v\n", err)
			return
		}
		if err := applyProviderConfigChange(cfg, eng, func() error {
			*cfg = *loaded
			cfg.ActiveProfile = name
			return nil
		}, func() error { return config.Save(cfg) }); err != nil {
			outf("应用 profile 失败: %v\n", err)
			return
		}
		if mode := permission.Mode(cfg.PermissionMode); permission.ValidMode(mode) {
			eng.SetPermissionMode(mode)
		}
		eng.SetMaxBudget(cfg.MaxBudgetUsd)
		outf("已切换到 profile: %s\n", name)
	case "save":
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]*config.Profile{}
		}
		// Only the user's own settings: values a project .cove.json set
		// used to be copied into the global profile (SnapshotProfile).
		p := cfg.SnapshotProfile()
		cfg.Profiles[name] = p
		if err := config.Save(cfg); err != nil {
			outf("保存 profile 失败: %v\n", err)
			return
		}
		outf("profile 已保存: %s\n", name)
	case "delete":
		if cfg.Profiles == nil {
			outf("profile %s 不存在\n", name)
			return
		}
		if _, ok := cfg.Profiles[name]; !ok {
			outf("profile %s 不存在\n", name)
			return
		}
		delete(cfg.Profiles, name)
		if strings.EqualFold(cfg.ActiveProfile, name) {
			cfg.ActiveProfile = ""
		}
		if err := config.Save(cfg); err != nil {
			outf("删除 profile 失败: %v\n", err)
			return
		}
		outf("profile 已删除: %s\n", name)
	case "show":
		if cfg.Profiles == nil {
			outf("profile %s 不存在\n", name)
			return
		}
		prof, ok := cfg.Profiles[name]
		if !ok {
			outf("profile %s 不存在\n", name)
			return
		}
		b, err := json.MarshalIndent(prof, "", "  ")
		if err != nil {
			outf("显示 profile 失败: %v\n", err)
			return
		}
		outln(string(b))
	default:
		outln("用法: /profile list | /profile switch <name> | /profile save <name> | /profile delete <name> | /profile show <name>")
	}
}
