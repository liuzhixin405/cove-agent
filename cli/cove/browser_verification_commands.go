package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/browser"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
)

type BrowserVerificationCommandOptions struct {
	EvidenceDir string
	ChromePath  string
	OnReport    func(context.Context, browser.Report) error
}

type BrowserVerificationCommand struct {
	options  BrowserVerificationCommandOptions
	frontend *frontend
}

func NewBrowserVerificationCommand(options BrowserVerificationCommandOptions) *BrowserVerificationCommand {
	return &BrowserVerificationCommand{options: options}
}

func (fe *frontend) browserVerificationCommands() []command.Command {
	return []command.Command{&BrowserVerificationCommand{frontend: fe}}
}

func (c *BrowserVerificationCommand) Name() string      { return "browser-verify" }
func (c *BrowserVerificationCommand) Aliases() []string { return nil }
func (c *BrowserVerificationCommand) Description() string {
	return "Run guarded DOM assertions and persist desktop/mobile screenshot evidence"
}
func (c *BrowserVerificationCommand) Category() string   { return catTasks }
func (c *BrowserVerificationCommand) ArgHints() []string { return []string{"results", "artifacts"} }
func (c *BrowserVerificationCommand) MutatesEngine(args []string) bool {
	return len(args) > 0 && args[0] != "results" && args[0] != "artifacts"
}
func (c *BrowserVerificationCommand) Help() string {
	return `/browser-verify <workflow.json> [--allow-local]
/browser-verify results
/browser-verify artifacts <run-id>
Actions: navigate, click, fill (fixture key), assert_text (contains), assert_visible, assert_url (exact).
Explicit assertions required. Fixtures must be non-secret. Password filling and sensitive screenshots are rejected.
--allow-local is a trusted user opt-in for loopback only, never a workflow JSON setting.
Requires -tags chromedp and installed Chrome/Chromium; otherwise status is unverified.
Reports omit fixture values, URLs and DOM text. Desktop 1280x800/mobile 390x844 replay independently.
Executed outcomes are machine JSON pass/fail/unverified; report.json and PNG artifacts are retained under the config directory.`
}

func (c *BrowserVerificationCommand) Execute(ctx context.Context, input command.Input) (command.Output, error) {
	args := input.Args
	if len(args) == 0 {
		return command.Output{Message: c.Help()}, nil
	}
	allowLocal := false
	switch args[0] {
	case "results":
		if len(args) != 1 {
			return command.Output{}, errors.New("usage: /browser-verify results")
		}
	case "artifacts":
		if len(args) != 2 || !validBrowserRunID(args[1]) {
			return command.Output{}, errors.New("usage: /browser-verify artifacts <run-id>")
		}
	default:
		if len(args) == 2 && args[1] == "--allow-local" {
			allowLocal = true
		} else if len(args) != 1 {
			return command.Output{}, errors.New("usage: /browser-verify <workflow.json> [--allow-local]")
		}
	}
	directory := c.options.EvidenceDir
	if directory == "" {
		root, err := config.ConfigDir()
		if err != nil {
			return command.Output{}, err
		}
		directory = filepath.Join(root, "browser-verification")
	}
	if args[0] == "results" {
		reports, truncated, err := listBrowserReports(directory)
		if err != nil {
			return command.Output{}, err
		}
		return browserCommandOutput(struct {
			Reports   []browser.Report `json:"reports"`
			Truncated bool             `json:"truncated"`
		}{reports, truncated})
	}
	if args[0] == "artifacts" {
		report, err := readBrowserReport(directory, args[1])
		if err != nil {
			return command.Output{}, err
		}
		return browserCommandOutput(report)
	}
	project := input.Cwd
	if project == "" {
		var err error
		project, err = os.Getwd()
		if err != nil {
			return command.Output{}, err
		}
	}
	path := args[0]
	if !filepath.IsAbs(path) {
		path = filepath.Join(project, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return command.Output{}, errors.New("cannot open browser workflow")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > browser.MaxWorkflowBytes {
		return command.Output{}, errors.New("browser workflow must be a bounded regular JSON file")
	}
	workflow, err := browser.DecodeWorkflow(file)
	if err != nil {
		return command.Output{}, err
	}
	backend := browser.New(browser.Config{AllowLocalhost: allowLocal, Timeout: 120 * time.Second})
	report, runErr := backend.Run(ctx, workflow, browser.RunOptions{EvidenceDir: directory, ChromePath: c.options.ChromePath})
	if report.EvidenceDir == "" {
		return command.Output{}, runErr
	}
	if report.Reason == "evidence_persist_failed" {
		return command.Output{}, runErr
	}
	if c.options.OnReport != nil {
		if err := c.options.OnReport(ctx, report); err != nil {
			return command.Output{}, err
		}
	} else {
		var engine any = input.Engine
		if c.frontend != nil && c.frontend.eng != nil {
			engine = c.frontend.eng
		}
		if recorder, ok := engine.(interface{ RecordBrowserVerification(browser.Report) error }); ok {
			if err := recorder.RecordBrowserVerification(report); err != nil {
				return command.Output{}, err
			}
		}
	}
	return browserCommandOutput(report)
}

func browserCommandOutput(value any) (command.Output, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return command.Output{}, err
	}
	return command.Output{Message: string(data), Data: string(data)}, nil
}

func validBrowserRunID(id string) bool {
	if !strings.HasPrefix(id, "browser-") || len(id) <= len("browser-") || len(id) > 80 {
		return false
	}
	for _, character := range id[len("browser-"):] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func readBrowserReport(directory, id string) (browser.Report, error) {
	var report browser.Report
	if !validBrowserRunID(id) {
		return report, errors.New("invalid browser run id")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return report, err
	}
	defer root.Close()
	file, err := root.Open(filepath.Join(id, "report.json"))
	if err != nil {
		return report, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, browser.MaxWorkflowBytes+1))
	if err != nil || len(data) > browser.MaxWorkflowBytes {
		return report, errors.New("browser report exceeds size limit")
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return report, errors.New("invalid browser report")
	}
	return report, nil
}

func listBrowserReports(directory string) ([]browser.Report, bool, error) {
	reports := []browser.Report{}
	folder, err := os.Open(directory)
	if os.IsNotExist(err) {
		return reports, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer folder.Close()
	entries, err := folder.ReadDir(201)
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	truncated := len(entries) > 200
	if truncated {
		entries = entries[:200]
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validBrowserRunID(entry.Name()) {
			continue
		}
		report, err := readBrowserReport(directory, entry.Name())
		if err != nil {
			continue
		}
		reports = append(reports, report)
	}
	sort.Slice(reports, func(first, second int) bool { return reports[first].StartedAt.After(reports[second].StartedAt) })
	if len(reports) > 20 {
		reports, truncated = reports[:20], true
	}
	return reports, truncated, nil
}

var _ command.Command = (*BrowserVerificationCommand)(nil)
