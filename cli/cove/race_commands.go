package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/race"
)

type RaceCommandOptions struct {
	Directory        string
	Runner           race.Runner
	LifecycleContext context.Context
	OnSelect         func()
	// OnFinish receives each run's final report (see race.Service.OnFinish).
	OnFinish func(*race.Report)
	// Profile is the --profile this process runs with; candidates inherit it.
	Profile string
}

type RaceCommand struct {
	options RaceCommandOptions
	mu      sync.Mutex
	service *race.Service
	closed  bool
}

func NewRaceCommand(options RaceCommandOptions) *RaceCommand { return &RaceCommand{options: options} }

func (fe *frontend) raceCommands() []command.Command {
	options := RaceCommandOptions{Profile: profileName}
	if fe != nil && fe.eng != nil {
		options.OnSelect = fe.eng.InvalidateWorkspaceEvidence
	}
	if fe != nil && fe.interactive() {
		options.OnFinish = func(r *race.Report) { notify(raceFinishedText(r)) }
	}
	return []command.Command{NewRaceCommand(options)}
}

func (c *RaceCommand) Name() string      { return "race" }
func (c *RaceCommand) Aliases() []string { return nil }
func (c *RaceCommand) Description() string {
	return "在两个 Git worktree 中运行不同方案，以同一验证器比较并显式选择"
}
func (c *RaceCommand) Category() string   { return catTasks }
func (c *RaceCommand) ArgHints() []string { return []string{"run", "list", "show", "select", "cancel"} }
func (c *RaceCommand) MutatesEngine(args []string) bool {
	return len(args) > 0 && (args[0] == "run" || args[0] == "select")
}
func (c *RaceCommand) Help() string {
	return `/race run <spec.json>        用两个 worktree 分别实现两套方案，再用同一验证器比较
/race list                   列出本项目的竞跑记录
/race show <运行ID>          查看生成/验证状态、耗时、费用与补丁摘要
/race select <运行ID> <a|b>  把通过验证的候选补丁应用到工作区（唯一会改动工作区的子命令）
/race cancel <运行ID>        取消本进程发起的竞跑

spec.json 示例：
{"version":1,"prompts":["实现方案 A","实现方案 B"],"verify":[["go","test","./internal/example"]],"total_budget_usd":2,"candidate_budget_usd":1,"total_timeout_seconds":300,"candidate_timeout_seconds":240,"verify_timeout_seconds":60}

要求：干净的本地 Git 项目根目录、处于分支上、没有未跟踪或被忽略的文件。
run 立即返回 ID；show 报告状态；只有 select 会在校验补丁哈希、项目、分支、提交与工作区干净后应用补丁。
报告与补丁存放在配置目录下，不进入源码；完成或取消后 worktree 会被删除。
默认 runner：当前 cove 可执行文件 -p <prompt> --no-tui --profile race-budget --max-turns 12。
预算用隔离 profile 的 max_budget_usd 控制，不是 --budget 参数；每个候选分配 min(candidate_budget_usd, total_budget_usd/2)。
引擎在两次请求之间检查花费，供应商计费可能略超；没有结构化凭据的费用记为 null/unverified。
验证器是显式的 argv 命令（不经 shell），只看退出码，不看模型自评。
worktree 只隔离 Git 改动，不是安全沙箱；prompt 与验证命令必须可信。
cancel 只对本进程发起的竞跑有效；退出时必须调用 Close，清理有 20 秒宽限。
select 前会重新校验，但不是针对外部编辑器的原子操作。`
}

func (c *RaceCommand) initialize() (*race.Service, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("竞跑命令已关闭")
	}
	if c.service != nil {
		return c.service, nil
	}
	directory, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	reports := c.options.Directory
	if reports == "" {
		reports = filepath.Join(directory, "race")
	}
	runner := c.options.Runner
	if runner == nil {
		runner = race.CLIRunner{ConfigDirectory: directory, Profile: c.options.Profile}
	}
	c.service = &race.Service{Directory: reports, Runner: runner, OnSelect: c.options.OnSelect, OnFinish: c.options.OnFinish}
	return c.service, nil
}

func (c *RaceCommand) Close() error {
	c.mu.Lock()
	c.closed = true
	service := c.service
	c.mu.Unlock()
	if service != nil {
		return service.Close()
	}
	return nil
}

func (c *RaceCommand) Execute(ctx context.Context, input command.Input) (command.Output, error) {
	args := input.Args
	if len(args) == 0 {
		return command.Output{Message: c.Help()}, nil
	}
	valid := false
	switch args[0] {
	case "list":
		valid = len(args) == 1
	case "run", "show", "cancel":
		valid = len(args) == 2
	case "select":
		valid = len(args) == 3
	}
	if !valid {
		return command.Output{}, errors.New("参数无效；输入 /race 查看用法")
	}
	service, err := c.initialize()
	if err != nil {
		return command.Output{}, err
	}
	project := input.Cwd
	if project == "" {
		project, err = os.Getwd()
		if err != nil {
			return command.Output{}, err
		}
	}
	var data any
	switch args[0] {
	case "run":
		path := args[1]
		if !filepath.IsAbs(path) {
			path = filepath.Join(project, path)
		}
		spec, err := race.ReadSpec(path)
		if err != nil {
			return command.Output{}, err
		}
		lifecycle := c.options.LifecycleContext
		if lifecycle == nil {
			lifecycle = context.Background()
		}
		id, err := service.Start(lifecycle, project, spec)
		if err != nil {
			return command.Output{}, err
		}
		return command.Output{Message: fmt.Sprintf("竞跑 %s 已启动；/race show %s 查看进度，/race cancel %s 取消；未应用任何补丁", id, id, id), Data: id}, nil
	case "list":
		data, err = service.List()
	case "show":
		data, err = service.Load(args[1])
	case "select":
		data, err = service.Select(ctx, project, args[1], args[2])
	case "cancel":
		if err := service.Cancel(args[1]); err != nil {
			return command.Output{}, err
		}
		return command.Output{Message: "已请求取消竞跑；worker 结束后清理 worktree"}, nil
	}
	if err != nil {
		return command.Output{}, err
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return command.Output{}, err
	}
	return command.Output{Message: string(encoded)}, nil
}
