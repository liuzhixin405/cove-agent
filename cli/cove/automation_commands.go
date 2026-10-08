package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/automation"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

const automationHelp = `/automations list
/automations add <spec.json> [session]
/automations run <id>
/automations tick
/automations event <name> <unique-key>
/automations remove <id>
Spec JSON: {"version":1,"spec":{"id":"maintenance","prompt":"Fix a small test failure","enabled":true,"every_seconds":3600,"event":"tests-failed","timeout_seconds":300,"budget_usd":1,"max_turns":8,"retries":1,"verify":[["go","test","./internal/automation"]]}}
Runs are opt-in, use committed HEAD in a temporary Git worktree, and may call the configured model. No Git repository means refusal. Tick runs due tasks once; use your OS scheduler to invoke cove --automation tick <project>. Event triggers are explicit and deduplicated by key. Session-scoped tasks require their originating session. Worktrees are not an OS/network sandbox. No automatic apply, merge, or push.`

const inboxHelp = `/inbox list
/inbox show <result-id>
/inbox patch <result-id> [destination.patch]
/inbox review <result-id> accepted|rejected
Show includes command argv, exit codes, output, base revision, patch and review state. Review persists a decision only; accepted does not apply or merge. Patch without a destination prints the patch. Export outside the project root, then inspect and apply manually. Expired workers remain uncertain until reviewed.`

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
		return nil, fmt.Errorf("automation state/config directory must be outside the original project: %w", err)
	}
	return automation.Open(filepath.Join(dir, "automations"), project)
}

func automationRunner(cfg *config.Config) (automation.Runner, error) {
	if cfg == nil {
		return nil, errors.New("automation requires an explicitly loaded model configuration")
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
		data, err := json.Marshal(isolated)
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
					message = "automation: " + err.Error()
				}
				if fe != nil && fe.print != nil {
					fe.print(message)
				}
				return true
			}}
	}
	return []command.Command{
		makeCommand("automations", "Opt-in isolated maintenance tasks", automationHelp, []string{"add", "list", "run", "tick", "event", "remove", "help"}),
		makeCommand("inbox", "Review persisted automation results (never auto-apply)", inboxHelp, []string{"list", "show", "patch", "review", "help"}),
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
		return "", errors.New("nested automation commands are disabled inside maintenance workers")
	}
	store, err := automationStore(in.Cwd)
	if err != nil {
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
			return "", errors.New("spec must contain exactly one JSON object")
		}
		if envelope.Version != automation.Version {
			return "", errors.New("unsupported automation spec version")
		}
		if len(args) == 3 {
			if sessionID == "" {
				return "", errors.New("session scope requires an active session")
			}
			envelope.Spec.SessionID = sessionID
		} else if envelope.Spec.SessionID != "" && envelope.Spec.SessionID != sessionID {
			return "", errors.New("spec session_id does not match the active session")
		}
		if err := store.Add(envelope.Spec, time.Now().UTC()); err != nil {
			return "", err
		}
		return "Added " + envelope.Spec.ID + "; no worker started. State: " + store.Path(), nil
	case "remove":
		if len(args) != 2 {
			return "", errors.New(automationHelp)
		}
		if err := store.Remove(args[1]); err != nil {
			return "", err
		}
		return "Removed " + args[1] + "; inbox results retained", nil
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
			return "", errors.Join(err, marshalErr, fmt.Errorf("persisted result: %s; inspect /inbox list", message))
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
				return "", fmt.Errorf("export patches outside the original project; no automatic root writes: %w", err)
			}
			configDir, err := config.ConfigDir()
			if err != nil {
				return "", err
			}
			if _, err := automation.ExternalPath(configDir, path); err != nil {
				return "", fmt.Errorf("export patches outside the configuration directory: %w", err)
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
			return "Patch exported to " + path + "; nothing applied", nil
		case "review":
			if len(args) != 3 {
				return "", errors.New(inboxHelp)
			}
			if err := store.Review(result.ID, args[2]); err != nil {
				return "", err
			}
			return "Review recorded: " + args[2] + "; nothing applied or merged", nil
		default:
			return "", errors.New(inboxHelp)
		}
	}
	return "", fmt.Errorf("unknown inbox result %q", args[1])
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
		fmt.Fprintln(stderr, "Usage: cove --automation <list|tick|add|run|event|remove> <project> [arguments]; cove --automation-inbox <list|show|patch|review> <project> [arguments]")
		return true, 2
	}
	if os.Getenv("COVE_AUTOMATION_CHILD") == "1" {
		fmt.Fprintln(stderr, "nested automation commands are disabled inside maintenance workers")
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
