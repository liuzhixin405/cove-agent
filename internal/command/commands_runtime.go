package command

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/diagnostic"
	"github.com/liuzhixin405/cove-agent/internal/permission"
)

func (c *DoctorCmd) Name() string        { return "doctor" }
func (c *DoctorCmd) Aliases() []string   { return nil }
func (c *DoctorCmd) Description() string { return "系统诊断" }
func (c *DoctorCmd) Help() string {
	return "/doctor - 检查 git、ripgrep、供应商与 API key 配置"
}
func (c *DoctorCmd) Execute(ctx context.Context, in Input) (Output, error) {
	var sb strings.Builder
	sb.WriteString("=== 系统诊断 ===\n")
	fmt.Fprintf(&sb, "目录: %s\n", in.Cwd)
	if g, err := exec.LookPath("git"); err == nil {
		fmt.Fprintf(&sb, "Git: %s\n", g)
	} else {
		sb.WriteString("Git: 未找到\n")
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		fmt.Fprintf(&sb, "Ripgrep: %s\n", rg)
	} else {
		sb.WriteString("Ripgrep: 未找到\n")
	}
	// The help has always promised a config check; a missing API key is the
	// most common first-run problem, and the key itself is never printed.
	if cfg := in.Config; cfg != nil {
		pc := cfg.EffectiveProvider()
		fmt.Fprintf(&sb, "供应商: %s  模型: %s\n", pc.Name, cfg.Model)
		if pc.BaseURL != "" {
			fmt.Fprintf(&sb, "接口地址: %s\n", pc.BaseURL)
		}
		if pc.APIKey != "" {
			sb.WriteString("API key: 已设置\n")
		} else if src := config.AWSCredentialsSource(pc); src != "" {
			fmt.Fprintf(&sb, "认证: AWS SigV4（凭证来源 %s）\n", src)
		} else {
			sb.WriteString("API key: 未设置（用 /api-key 或环境变量设置）\n")
		}
	}
	fmt.Fprintf(&sb, "时间: %s\n", time.Now().Format(time.RFC3339))
	// Background learning and the policies file: cheap, and otherwise only
	// visible in /diagnose.
	sb.WriteString(diagnostic.BackgroundSummary())
	return Output{Message: sb.String()}, nil
}

func (c *ConfigCmd) Name() string        { return "config" }
func (c *ConfigCmd) Aliases() []string   { return nil }
func (c *ConfigCmd) Description() string { return "查看或修改配置" }
func (c *ConfigCmd) Help() string        { return "/config [键] [值] - 查看/设置配置" }
func (c *ConfigCmd) Execute(ctx context.Context, in Input) (Output, error) {
	cfg := in.Config
	if cfg == nil {
		return Output{Message: "配置不可用"}, nil
	}
	if len(in.Args) == 0 || in.Args[0] == "show" {
		return Output{Message: renderConfig(cfg)}, nil
	}
	key := strings.ToLower(in.Args[0])
	if len(in.Args) == 1 {
		return Output{Message: fmt.Sprintf("%s = %s", key, configValue(cfg, key))}, nil
	}
	value := strings.Join(in.Args[1:], " ")
	if err := applyConfigValue(cfg, key, value); err != nil {
		return Output{}, err
	}
	if key == "model" {
		cfg.Model = config.ResolveModelForProvider(cfg.Model, cfg.Provider.Name)
	}
	if in.SaveConfig != nil {
		if err := in.SaveConfig(cfg); err != nil {
			return Output{}, err
		}
	}
	msg := fmt.Sprintf("已保存 %s = %s", key, configValue(cfg, key))
	applied, err := applyConfigLive(in, key)
	if err != nil {
		return Output{}, fmt.Errorf("%s；但应用到当前会话失败: %w", msg, err)
	}
	if !applied {
		msg += "（重启 cove 后生效）"
	}
	return Output{Message: msg}, nil
}

// applyConfigLive pushes a /config change into the running session. It used
// to only save the file, so "/config model x" answered 已保存 while the
// session kept talking to the old model until a restart (/model applies at
// once). applied is false when the engine behind the view cannot take the
// change; keys that are only read at request time count as applied.
func applyConfigLive(in Input, key string) (applied bool, err error) {
	cfg := in.Config
	switch key {
	case "model", "provider", "api_key", "api-key", "base_url", "base-url":
		r, ok := in.Engine.(providerReloader)
		if !ok {
			return false, nil
		}
		pc := cfg.EffectiveProvider()
		if err := r.ReloadProvider(pc.Name, cfg.Model, pc.BaseURL, pc.APIKey); err != nil {
			return false, err
		}
		return true, nil
	case "mode", "permission_mode", "permission-mode":
		mode := permission.Mode(cfg.PermissionMode)
		s, ok := in.Engine.(permissionModeSetter)
		if ok {
			s.SetPermissionMode(mode)
		} else if in.PermissionManager != nil {
			in.PermissionManager.SetMode(mode)
		}
		return ok, nil
	case "budget", "max_budget_usd", "max-budget-usd":
		s, ok := in.Engine.(budgetSetter)
		if ok {
			s.SetMaxBudget(cfg.MaxBudgetUsd)
		}
		return ok, nil
	case "system", "system_prompt", "system-prompt":
		s, ok := in.Engine.(instructionsSetter)
		return ok && s.SetCustomInstructions(cfg.SystemPrompt), nil
	default:
		return true, nil
	}
}
