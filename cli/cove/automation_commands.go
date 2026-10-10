package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/automation"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

const automationHelp = `/automations list                        列出维护任务
/automations add <spec.json> [session]   添加任务（session 表示只属于当前会话）
/automations run <id>                    立即运行一次
/automations tick                        运行所有到期任务一次
/automations event <名称> <唯一键>        触发事件（按键去重）
/automations remove <id>                 删除任务（已有结果保留）
Spec JSON：{"version":1,"spec":{"id":"maintenance","prompt":"修一个小的测试失败","enabled":true,"every_seconds":3600,"event":"tests-failed","timeout_seconds":300,"budget_usd":1,"max_turns":8,"retries":1,"verify":[["go","test","./internal/automation"]]}}
任务需显式开启，在临时 Git worktree 里基于已提交的 HEAD 运行，可能调用模型；没有 Git 仓库会拒绝。tick 只跑一次到期任务，定时请用系统计划任务调用 cove --automation tick <项目目录>。事件触发是显式的并按键去重；会话范围的任务只能由原会话触发。worktree 不是系统或网络沙箱。不会自动应用、合并或推送。`

const inboxHelp = `/inbox list                              列出维护结果
/inbox show <结果ID>                     查看命令、退出码、输出、基准版本、补丁与评审状态
/inbox patch <结果ID> [目标.patch]        打印补丁，或导出到项目目录之外的文件
/inbox review <结果ID> accepted|rejected 记录评审结论（accepted 也不会应用或合并）
请把补丁导出到项目根目录之外，再自行检查与应用。过期的 worker 在评审前保持“不确定”状态。`

type automationSpecFile struct {
	Version int             `json:"version"`
	Spec    automation.Spec `json:"spec"`
}

func automationStore(project string) (*automation.Store, error) {
	if project == "" {
		var err error
		project, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	dir, err = automation.ExternalPath(project, dir)
	if err != nil {
		return nil, fmt.Errorf("维护任务的状态/配置目录必须在原项目之外: %w", err)
	}
	return automation.Open(filepath.Join(dir, "automations"), project)
}

func automationRunner(cfg *config.Config) (automation.Runner, error) {
	if cfg == nil {
		return nil, errors.New("维护任务需要已明确加载的模型配置")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return automation.CommandRunner{Executable: executable, Prepare: func(worktree string, budget float64, spec automation.Spec) ([]string, func(), error) {
		dir, err := os.MkdirTemp("", "cove-automation-config-")
		if err != nil {
			return nil, nil, err
		}
		cleanup := func() { _ = os.RemoveAll(dir) }
		isolated := *cfg
		isolated.MaxBudgetUsd = budget
		isolated.MaxIterations = spec.MaxTurns
		isolated.PermissionMode = "auto"
		isolated.ActiveProfile, isolated.Profiles = "", nil
		isolated.MCPServers = nil
		isolated.ExperimentalTools = false
		isolated.MemoryEmbedding = nil
		isolated.DoneVerifyCommands = nil
		disabled := false
		isolated.DoneVerifyAuto, isolated.DoneVerifyTests = &disabled, &disabled
		isolated.DoneSelfReview, isolated.DoneCheck = "off", "off"
		// MarshalRaw keeps the API key: json.Marshal would write the masked
		// "sk-r****7890" form, which the worker's Load clears, so every
		// maintenance run failed authentication.
		data, err := isolated.MarshalRaw()
		if err == nil {
			err = fsatomic.WriteFile(filepath.Join(dir, "config.json"), data, 0600)
		}
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		return []string{"COVE_CONFIG_DIR=" + dir, "COVE_AUTOMATION_CHILD=1"}, cleanup, nil
	}}, nil
}

func (fe *frontend) automationCommands() []command.Command {
	makeCommand := func(name, desc, help string, hints []string) command.Command {
		return &feCmd{name: name, desc: desc, help: help, category: catTasks, hints: hints,
			mutates: func(args []string) bool {
				if len(args) == 0 {
					return false
				}
				if name == "inbox" {
					return args[0] == "review" || (args[0] == "patch" && len(args) > 2)
				}
				switch args[0] {
				case "add", "remove", "run", "tick", "event":
					return true
				default:
					return false
				}
			},
			run: func(ctx context.Context, in command.Input) bool {
				message, err := executeAutomationCommand(ctx, name, in, nil)
				if err != nil {
					message = "维护任务: " + err.Error()
				}
				if fe != nil && fe.print != nil {
					fe.print(message)
				}
				return true
			}}
	}
	return []command.Command{
		makeCommand("automations", "显式维护任务：定时扫描、事件去重、worktree 隔离执行", automationHelp, []string{"add", "list", "run", "tick", "event", "remove", "help"}),
		makeCommand("inbox", "查看维护任务结果与补丁（从不自动应用）", inboxHelp, []string{"list", "show", "patch", "review", "help"}),
	}
}

func executeAutomationCommand(ctx context.Context, name string, in command.Input, runner automation.Runner) (string, error) {
	args := in.Args
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help") {
		if name == "inbox" {
			return inboxHelp, nil
		}
		return automationHelp, nil
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	if os.Getenv("COVE_AUTOMATION_CHILD") == "1" {
		return "", errors.New("维护 worker 内禁止嵌套维护命令")
	}
	store, err := automationStore(in.Cwd)
	if err != nil {
		return "", err
	}
	// Expired claims (a crashed worker) become uncertain on every command,
	// as documented, not only when a run/tick/event happens to go through
	// the executor: /inbox list and /automations remove see the real state.
	if err := store.Recover(time.Now().UTC()); err != nil {
		return "", err
	}
	state, err := store.Read()
	if err != nil {
		return "", err
	}
	sessionID := ""
	if source, ok := in.Engine.(command.StatusSource); ok {
		sessionID = source.SessionID()
	}
	if name == "inbox" {
		return executeInbox(store, state, args)
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return "", errors.New(automationHelp)
		}
		return automationJSON(state.Specs)
	case "add":
		if len(args) < 2 || len(args) > 3 || (len(args) == 3 && args[2] != "session") {
			return "", errors.New(automationHelp)
		}
		path := args[1]
		if !filepath.IsAbs(path) {
			path = filepath.Join(state.Project, path)
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()
		decoder := json.NewDecoder(io.LimitReader(file, 1024*1024))
		decoder.DisallowUnknownFields()
		var envelope automationSpecFile
		if err := decoder.Decode(&envelope); err != nil {
			return "", err
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return "", errors.New("spec 文件必须只包含一个 JSON 对象")
		}
		if envelope.Version != automation.Version {
			return "", errors.New("不支持的维护任务 spec 版本")
		}
		if len(args) == 3 {
			if sessionID == "" {
				return "", errors.New("session 范围需要一个活动会话")
			}
			envelope.Spec.SessionID = sessionID
		} else if envelope.Spec.SessionID != "" && envelope.Spec.SessionID != sessionID {
			return "", errors.New("spec 的 session_id 与当前会话不符")
		}
		if err := store.Add(envelope.Spec, time.Now().UTC()); err != nil {
			return "", err
		}
		return "已添加 " + envelope.Spec.ID + "；未启动 worker。状态文件: " + store.Path(), nil
	case "remove":
		if len(args) != 2 {
			return "", errors.New(automationHelp)
		}
		if err := store.Remove(args[1]); err != nil {
			return "", err
		}
		return "已删除 " + args[1] + "；inbox 结果保留", nil
	case "run", "tick", "event":
		if (args[0] == "run" && len(args) != 2) || (args[0] == "tick" && len(args) != 1) || (args[0] == "event" && len(args) != 3) {
			return "", errors.New(automationHelp)
		}
		if runner == nil {
			runner, err = automationRunner(in.Config)
			if err != nil {
				return "", err
			}
		}
		executor := automation.Executor{Store: store, Runner: runner}
		var data any
		switch args[0] {
		case "run":
			data, err = executor.Run(ctx, args[1], sessionID)
		case "tick":
			data, err = executor.RunDue(ctx, time.Now().UTC(), sessionID)
		case "event":
			data, err = executor.Trigger(ctx, args[1], args[2], sessionID)
		}
		message, marshalErr := automationJSON(data)
		if err != nil {
			// The result JSON carries patches and logs of up to 4 MiB each;
			// name the result IDs, do not print the whole thing as an error.
			return "", errors.Join(err, marshalErr, fmt.Errorf("结果已保存 %s；用 /inbox show <结果ID> 查看", automationResultIDs(data)))
		}
		return message, marshalErr
	default:
		return "", errors.New(automationHelp)
	}
}

func executeInbox(store *automation.Store, state automation.Snapshot, args []string) (string, error) {
	if args[0] == "list" && len(args) == 1 {
		type summary struct {
			ID, TaskID, State, Review string
			Started                   time.Time
		}
		items := make([]summary, 0, len(state.Results))
		for _, result := range state.Results {
			items = append(items, summary{result.ID, result.TaskID, result.State, result.Review, result.Started})
		}
		return automationJSON(items)
	}
	if len(args) < 2 {
		return "", errors.New(inboxHelp)
	}
	for _, result := range state.Results {
		if result.ID != args[1] {
			continue
		}
		switch args[0] {
		case "show":
			if len(args) != 2 {
				return "", errors.New(inboxHelp)
			}
			return automationJSON(result)
		case "patch":
			if len(args) < 2 || len(args) > 3 {
				return "", errors.New(inboxHelp)
			}
			if len(args) == 2 {
				return result.Patch, nil
			}
			destination := args[2]
			if !filepath.IsAbs(destination) {
				destination = filepath.Join(state.Project, destination)
			}
			path, err := filepath.Abs(destination)
			if err != nil {
				return "", err
			}
			parent, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil {
				return "", err
			}
			path = filepath.Join(parent, filepath.Base(path))
			if _, err := automation.ExternalPath(state.Project, path); err != nil {
				return "", fmt.Errorf("补丁请导出到原项目之外，不自动写入项目根目录: %w", err)
			}
			configDir, err := config.ConfigDir()
			if err != nil {
				return "", err
			}
			if _, err := automation.ExternalPath(configDir, path); err != nil {
				return "", fmt.Errorf("补丁请导出到配置目录之外: %w", err)
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return "", err
			}
			_, writeErr := file.WriteString(result.Patch)
			syncErr := file.Sync()
			closeErr := file.Close()
			if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
				return "", err
			}
			return "补丁已导出到 " + path + "；未应用", nil
		case "review":
			if len(args) != 3 {
				return "", errors.New(inboxHelp)
			}
			if err := store.Review(result.ID, args[2]); err != nil {
				return "", err
			}
			return "评审已记录: " + args[2] + "；未应用也未合并", nil
		default:
			return "", errors.New(inboxHelp)
		}
	}
	return "", fmt.Errorf("未知的 inbox 结果 %q", args[1])
}

// automationResultIDs names the persisted results of a run/tick/event so an
// error line can point at them without printing their patches and logs.
func automationResultIDs(data any) string {
	var ids []string
	switch value := data.(type) {
	case automation.Result:
		ids = append(ids, value.ID)
	case []automation.Result:
		for _, result := range value {
			ids = append(ids, result.ID)
		}
	}
	if len(ids) == 0 {
		return "（无）"
	}
	return strings.Join(ids, ", ")
}

func automationJSON(value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	return string(data), err
}

func loadAutomationCLIConfig(project string) (cfg *config.Config, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	previous, present := os.LookupEnv("COVE_CONFIG_DIR")
	if err := os.Setenv("COVE_CONFIG_DIR", dir); err != nil {
		return nil, err
	}
	defer func() {
		if present {
			err = errors.Join(err, os.Setenv("COVE_CONFIG_DIR", previous))
		} else {
			err = errors.Join(err, os.Unsetenv("COVE_CONFIG_DIR"))
		}
	}()
	if err := os.Chdir(project); err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.Chdir(cwd)) }()
	return config.LoadWithProfile("")
}

func RunAutomationCLI(ctx context.Context, args []string, stdout, stderr io.Writer) (handled bool, exitCode int) {
	if len(args) == 0 || (args[0] != "--automation" && args[0] != "--automation-inbox") {
		return false, 0
	}
	name := "automations"
	if args[0] == "--automation-inbox" {
		name = "inbox"
	}
	if len(args) < 3 {
		fmt.Fprintln(stderr, "用法: cove --automation <list|tick|add|run|event|remove> <项目目录> [参数]；cove --automation-inbox <list|show|patch|review> <项目目录> [参数]")
		return true, 2
	}
	if os.Getenv("COVE_AUTOMATION_CHILD") == "1" {
		fmt.Fprintln(stderr, "维护 worker 内禁止嵌套维护命令")
		return true, 1
	}
	in := command.Input{Cwd: args[2], Args: append([]string{args[1]}, args[3:]...)}
	if name == "automations" && args[1] == "add" && len(in.Args) >= 2 {
		path, err := filepath.Abs(in.Args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return true, 1
		}
		in.Args[1] = path
	}
	if name == "inbox" && args[1] == "patch" && len(in.Args) == 3 {
		path, err := filepath.Abs(in.Args[2])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return true, 1
		}
		in.Args[2] = path
	}
	if name == "automations" && (args[1] == "run" || args[1] == "tick" || args[1] == "event") {
		if _, err := automationStore(in.Cwd); err != nil {
			fmt.Fprintln(stderr, err)
			return true, 1
		}
		cfg, err := loadAutomationCLIConfig(in.Cwd)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return true, 1
		}
		in.Config = cfg
	}
	message, err := executeAutomationCommand(ctx, name, in, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return true, 1
	}
	fmt.Fprintln(stdout, message)
	return true, 0
}
