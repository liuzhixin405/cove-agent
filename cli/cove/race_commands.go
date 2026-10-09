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
	return `/race run <spec.json>
/race list
/race show <runID>
/race select <runID> <a|b>
/race cancel <runID>

spec.json example:
{"version":1,"prompts":["Implement solution A","Implement alternative B"],"verify":[["go","test","./internal/example"]],"total_budget_usd":2,"candidate_budget_usd":1,"total_timeout_seconds":300,"candidate_timeout_seconds":240,"verify_timeout_seconds":60}

Requires a clean local Git project root on a branch, including no untracked/ignored files.
run returns an ID immediately; show reports generation/verifier status, duration, cost and patch bytes/hash.
Only select applies a passing candidate after exact patch hash, project, branch, commit and clean-state checks.
Reports/patches are stored under the config directory, never in source. Worktrees are removed after completion/cancellation.
Default runner: current cove executable -p <prompt> --no-tui --profile race-budget --max-turns 12.
Budget uses an isolated config max_budget_usd/profile, NOT an unsupported --budget flag.
Total allocation is min(candidate_budget_usd,total_budget_usd/2) for each candidate.
The engine checks spend between requests; provider billing may overshoot. Cost without structured evidence is null/unverified.
Verifiers are explicit argv commands (no implicit shell); only their exit status decides correctness, never model self-scoring.
Worktrees are isolation of Git changes, NOT a security sandbox. Prompts and verifier commands must be trusted.
cancel works for runs owned by this process. Shutdown must call Close; cleanup has a bounded 20-second grace.
Selection revalidates before git apply; this is NOT an atomic compare-and-swap against external editors.`
}

func (c *RaceCommand) initialize() (*race.Service, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("race command is closed")
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
	c.service = &race.Service{Directory: reports, Runner: runner, OnSelect: c.options.OnSelect}
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
		return command.Output{}, errors.New("invalid race arguments; use /race for help")
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
		return command.Output{Message: fmt.Sprintf("race %s started; /race show %s; /race cancel %s; no patch applied", id, id, id), Data: id}, nil
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
		return command.Output{Message: "race cancellation requested; workers will be reaped before worktree cleanup"}, nil
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
