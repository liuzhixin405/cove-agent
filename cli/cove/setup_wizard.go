package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/render"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

// setupVerifyTimeout bounds the verification request; a variable for tests.
var setupVerifyTimeout = 20 * time.Second

type setupProvider struct{ Name, Label, Note string }

// setupProviders is the wizard's list, domestic providers first since they
// are what most people here configure.
var setupProviders = []setupProvider{
	{"deepseek", "DeepSeek", "默认模型 deepseek-v4-pro"},
	{"anthropic", "Anthropic / Claude", "默认模型 claude-sonnet-4-20250514"},
	{"openai", "OpenAI", "默认模型 gpt-4o"},
	{"glm", "智谱 GLM", "open.bigmodel.cn"},
	{"kimi", "Kimi / Moonshot", "api.moonshot.cn"},
	{"qwen", "通义千问 Qwen", "dashscope 兼容模式"},
	{"doubao", "豆包 / 火山方舟", "ark.cn-beijing.volces.com"},
	{"openrouter", "OpenRouter", "多模型网关"},
	{"siliconflow", "硅基流动", "api.siliconflow.cn"},
	{"openai-compatible", "其他 OpenAI 兼容接口", "保留当前 base_url，之后可用 /base-url 调整"},
}

const setupSkipped = "已跳过配置，之后可输入 /setup 再来。"

// runSetupWizard configures a provider and key in three steps when the run
// has no key: pick a provider, enter the key (masked), verify with one tiny
// request, then save. It returns whether a key was saved. The person can
// leave at any step; the REPL then starts as before. The missing key used
// to be reported only when the first message was sent, and that message was
// dropped.
func runSetupWizard(fe *frontend, reader *repl.LineReader, cfg *config.Config, eng *engine.Engine) bool {
	fe.print("欢迎使用 cove。还没有配置 API key，三步完成：选供应商 → 输入 key → 自动验证。任意一步直接回车可跳过。")
	for {
		name, ok := setupPickProvider(reader, cfg.EffectiveProvider().Name)
		if !ok {
			fe.print(setupSkipped)
			return false
		}
		for {
			key, ok := setupReadKey(reader, name)
			if !ok {
				fe.print(setupSkipped)
				return false
			}
			switch setupVerifyAndSave(fe, reader, cfg, eng, name, key) {
			case setupSaved:
				return true
			case setupSkip:
				fe.print(setupSkipped)
				return false
			case setupChangeProvider:
				// back to the provider list
			case setupRetryKey:
				continue
			}
			break
		}
	}
}

// setupPickProvider shows the provider list (the choice panel when the
// terminal has one, a numbered list otherwise) and returns the chosen name.
func setupPickProvider(reader *repl.LineReader, current string) (string, bool) {
	choices := make([]repl.Choice, 0, len(setupProviders))
	for i, p := range setupProviders {
		choices = append(choices, repl.Choice{Value: p.Name, Label: fmt.Sprintf("%d. %s", i+1, p.Label), Description: p.Note})
	}
	if repl.ChoicesAvailable() && reader.ShowChoices("选择模型供应商", choices) {
		termui.PrintAbove("选择模型供应商：上下键选择后回车，或输入编号/名称；直接回车跳过。\n")
	} else {
		termui.PrintAbove("选择模型供应商（输入编号或名称，直接回车跳过）：\n")
		for i, p := range setupProviders {
			termui.PrintAbove(fmt.Sprintf("  %2d. %-24s %s\n", i+1, p.Label, p.Note))
		}
	}
	line, err := reader.ReadLine()
	if err != nil {
		return "", false
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(setupProviders) {
		return setupProviders[n-1].Name, true
	}
	if api.IsKnownProvider(line) {
		return api.NormalizeProviderName(line), true
	}
	termui.PrintAbove("不认识的供应商：" + line + "\n")
	return "", false
}

// setupReadKey reads a key masked. When the provider's environment variable
// already holds one, Enter alone takes it; otherwise Enter alone leaves the
// wizard.
func setupReadKey(reader *repl.LineReader, provider string) (string, bool) {
	fromEnv := ""
	for _, env := range api.ProviderEnvCandidates(provider) {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			fromEnv = v
			termui.PrintAbove("检测到环境变量 " + env + " 已有值：直接回车使用它，或输入新的 key。\n")
			break
		}
	}
	prompt := "API key（输入不回显，直接回车跳过）: "
	if fromEnv != "" {
		prompt = "API key（输入不回显，回车使用环境变量）: "
	}
	key, err := reader.ReadSecret(prompt)
	if err != nil {
		return "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = fromEnv
	}
	return key, key != ""
}

// setupKeyFromEnv reports whether key is what the provider's environment
// variable holds: then it is used for the session but not written to the
// config file, which would copy a secret the person keeps elsewhere.
func setupKeyFromEnv(provider, key string) bool {
	for _, env := range api.ProviderEnvCandidates(provider) {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" && v == key {
			return true
		}
	}
	return false
}

type setupOutcome int

const (
	setupSaved setupOutcome = iota
	setupRetryKey
	setupChangeProvider
	setupSkip
)

// setupVerifyAndSave applies provider and key to the running engine, sends
// one tiny request, and saves on success. On failure the config is restored
// and the person chooses what to do next.
func setupVerifyAndSave(fe *frontend, reader *repl.LineReader, cfg *config.Config, eng *engine.Engine, provider, key string) setupOutcome {
	snapshot := *cfg
	err := applyProviderConfigChange(cfg, eng, func() error {
		old := cfg.EffectiveProvider().Name
		if provider != "openai-compatible" && api.NormalizeProviderName(old) != provider {
			cfg.Provider.BaseURL = ""
		}
		cfg.Provider.Name = provider
		cfg.Provider.APIKey = key
		cfg.Model = providerSwitchModel(old, provider, cfg.Model)
		return nil
	})
	if err == nil {
		fe.print("正在验证…")
		ctx, cancel := context.WithTimeout(context.Background(), setupVerifyTimeout)
		defer cancel()
		_, err = eng.GenerateOnce(ctx, "You are a connectivity check. Answer with one word.", "回复 ok")
	}
	if err == nil {
		path := "config.json"
		if dir, dirErr := config.ConfigDir(); dirErr == nil {
			path = filepath.Join(dir, "config.json")
		}
		toSave := *cfg
		note := ""
		if setupKeyFromEnv(provider, key) {
			toSave.Provider.APIKey = ""
			note = "；key 来自环境变量，未写入配置文件"
		}
		if saveErr := config.Save(&toSave); saveErr != nil {
			fe.print("验证通过，但配置保存失败（仅本次会话生效）: " + saveErr.Error())
			return setupSaved
		}
		fe.print(fmt.Sprintf("验证通过，已保存到 %s（供应商 %s，模型 %s%s）。可用 /provider、/model、/base-url 调整。", path, cfg.Provider.Name, cfg.Model, note))
		return setupSaved
	}
	// Back to what the session had: the engine reloads the old provider
	// through the same path (a no-op when nothing changed).
	restored := snapshot
	if applyProviderConfigChange(cfg, eng, func() error { *cfg = restored; return nil }) != nil {
		// Reloading a provider without a key can fail; the config still goes
		// back (the loop refuses to send without a key), and the engine keeps
		// the failed provider until the next successful change.
		*cfg = restored
	}
	fe.print("验证失败: " + render.VisibleControls(err.Error()))
	// The wizard runs before the input loop, so nothing relays answers to a
	// waiting prompt (repl.AskWith would block forever): read the line here.
	termui.PrintAbove(termui.PromptContentIndent + termui.Styled(termui.Bold, "[r] 重新输入 key   [p] 换供应商   [s] 先跳过") + "\n")
	answer, err := reader.ReadLine()
	if err != nil {
		return setupSkip
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "r":
		return setupRetryKey
	case "p":
		return setupChangeProvider
	}
	return setupSkip
}
