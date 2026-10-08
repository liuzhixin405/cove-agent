package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/browser"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/delegate"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

// AcceptanceCheck separates executable evidence from unverified requirements.
type AcceptanceCheck struct {
	Criterion string        `json:"criterion"`
	Command   string        `json:"command,omitempty"`
	Status    string        `json:"status"`
	Reason    string        `json:"reason,omitempty"`
	Result    *VerifyResult `json:"result,omitempty"`
}

// AcceptanceReport is a task-scoped record, not a model's completion claim.
type AcceptanceReport struct {
	Browser    []browser.Report              `json:"browser,omitempty"`
	Regression []delegate.RegressionEvidence `json:"regression,omitempty"`
	Version    int                           `json:"version"`
	ID         string                        `json:"id"`
	SessionID  string                        `json:"session_id"`
	Cwd        string                        `json:"cwd"`
	Request    string                        `json:"request"`
	StartedAt  time.Time                     `json:"started_at"`
	FinishedAt time.Time                     `json:"finished_at,omitempty"`
	Outcome    string                        `json:"outcome"`
	Reason     string                        `json:"reason,omitempty"`
	Attempts   int                           `json:"attempts"`
	Checks     []AcceptanceCheck             `json:"checks"`
}

func (e *Engine) acceptanceCwd() (string, error) {
	if e.verifyGate != nil && e.verifyGate.workDir != "" {
		return session.NormalizeProjectDir(e.verifyGate.workDir), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return session.NormalizeProjectDir(cwd), nil
}

func acceptancePath(cwd, sessionID string) (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(cwd + "\x00" + sessionID))
	return filepath.Join(dir, "acceptance", hex.EncodeToString(sum[:])+".json"), nil
}

func (e *Engine) beginAcceptance(msg api.Message) {
	cwd, err := e.acceptanceCwd()
	if err != nil {
		e.engineOutput("acceptance scope: " + err.Error())
		return
	}
	report := &AcceptanceReport{Version: 1, ID: rand.Text(), SessionID: e.SessionID(), Cwd: cwd, Request: msg.Content, StartedAt: time.Now(), Outcome: "running", Reason: "尚未执行校验"}
	if e.verifyGate != nil {
		for _, cmd := range e.verifyGate.commands {
			report.Checks = append(report.Checks, AcceptanceCheck{Criterion: cmd, Command: cmd, Status: "unverified", Reason: "尚未执行"})
		}
	}
	e.acceptanceMu.Lock()
	e.acceptance = report
	e.acceptanceMu.Unlock()
}

func (e *Engine) recordAcceptanceResults(commands []string, results []VerifyResult) {
	e.acceptanceMu.Lock()
	defer e.acceptanceMu.Unlock()
	if e.acceptance == nil {
		return
	}
	report := e.acceptance
	report.Attempts++
	report.Checks = nil
	report.Reason = ""
	for index, cmd := range commands {
		check := AcceptanceCheck{Criterion: cmd, Command: cmd, Status: "unverified", Reason: "未执行（前序失败、超时或任务被取消）"}
		if index < len(results) {
			result := results[index]
			result.Output = truncateTail(result.Output, 4000)
			check.Result = &result
			switch {
			case result.TimedOut:
				check.Reason = "校验超时，无法判定"
			case result.Passed:
				check.Status, check.Reason = "passed", "校验命令成功"
				if result.Skipped {
					check.Reason = "复用本轮最后一次修改后的成功证据"
				}
			default:
				check.Status, check.Reason = "failed", "校验命令失败"
			}
		}
		report.Checks = append(report.Checks, check)
	}
}

func (e *Engine) skipAcceptance(reason string) {
	e.acceptanceMu.Lock()
	defer e.acceptanceMu.Unlock()
	if e.acceptance != nil {
		e.acceptance.Reason = reason
		for index := range e.acceptance.Checks {
			e.acceptance.Checks[index].Status = "unverified"
			e.acceptance.Checks[index].Result = nil
			e.acceptance.Checks[index].Reason = reason
		}
	}
}

func (e *Engine) finishAcceptance(err error) {
	e.acceptanceMu.Lock()
	if e.acceptance == nil {
		e.acceptanceMu.Unlock()
		return
	}
	e.acceptance.FinishedAt = time.Now()
	e.acceptance.Outcome = "finished"
	if err != nil {
		e.acceptance.Outcome, e.acceptance.Reason = "interrupted", err.Error()
	}
	if len(e.acceptance.Checks) == 0 && len(e.acceptance.Regression) == 0 && len(e.acceptance.Browser) == 0 && err == nil {
		e.acceptance.Reason = "没有可执行的校验证据，不能据此宣称验收通过"
	}
	report := cloneAcceptance(e.acceptance)
	saveErr := saveAcceptance(report)
	e.acceptanceMu.Unlock()
	if saveErr != nil {
		e.engineOutput("验收报告保存失败: " + saveErr.Error())
	}
	if len(report.Checks) > 0 || len(report.Regression) > 0 || len(report.Browser) > 0 {
		passed, failed, unverified := 0, 0, 0
		for _, check := range report.Checks {
			switch check.Status {
			case "passed":
				passed++
			case "failed":
				failed++
			default:
				unverified++
			}
		}
		for _, evidence := range report.Regression {
			switch evidence.Status {
			case "passed":
				passed++
			case "failed":
				failed++
			default:
				unverified++
			}
		}
		for _, evidence := range report.Browser {
			switch evidence.Status {
			case browser.StatusPass:
				passed++
			case browser.StatusFail:
				failed++
			default:
				unverified++
			}
		}
		e.engineOutput(fmt.Sprintf("验收: 通过 %d，失败 %d，未验证 %d；/acceptance 查看证据。", passed, failed, unverified))
	}
}

func cloneAcceptance(report *AcceptanceReport) *AcceptanceReport {
	copyReport := *report
	copyReport.Browser = append([]browser.Report(nil), report.Browser...)
	for index := range copyReport.Browser {
		copyReport.Browser[index] = cloneBrowserReport(report.Browser[index])
	}
	copyReport.Regression = append([]delegate.RegressionEvidence(nil), report.Regression...)
	for index := range copyReport.Regression {
		copyReport.Regression[index].Tests = append([]string(nil), report.Regression[index].Tests...)
		copyReport.Regression[index].Files = append([]string(nil), report.Regression[index].Files...)
	}
	copyReport.Checks = append([]AcceptanceCheck(nil), report.Checks...)
	for index := range copyReport.Checks {
		if result := copyReport.Checks[index].Result; result != nil {
			copyResult := *result
			copyReport.Checks[index].Result = &copyResult
		}
	}
	return &copyReport
}

func saveAcceptance(report *AcceptanceReport) error {
	path, err := acceptancePath(report.Cwd, report.SessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return fsatomic.WriteFile(path, data, 0600)
}

// LastAcceptance returns a detached snapshot of this session's latest task.
func (e *Engine) LastAcceptance() (*AcceptanceReport, error) {
	cwd, err := e.acceptanceCwd()
	if err != nil {
		return nil, err
	}
	sessionID := e.SessionID()
	e.acceptanceMu.Lock()
	defer e.acceptanceMu.Unlock()
	return e.lastAcceptanceLocked(cwd, sessionID)
}

func (e *Engine) lastAcceptanceLocked(cwd, sessionID string) (*AcceptanceReport, error) {
	if e.acceptance != nil && e.acceptance.SessionID == sessionID && e.acceptance.Cwd == cwd {
		report := cloneAcceptance(e.acceptance)
		return report, nil
	}
	path, err := acceptancePath(cwd, sessionID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var report AcceptanceReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	if report.Version != 1 || report.Cwd != cwd || report.SessionID != sessionID {
		return nil, fmt.Errorf("acceptance report scope or version mismatch")
	}
	return &report, nil
}

func cloneBrowserReport(report browser.Report) browser.Report {
	report.Steps = append([]browser.StepEvidence(nil), report.Steps...)
	report.Artifacts = append([]browser.Artifact(nil), report.Artifacts...)
	return report
}

func (e *Engine) RecordBrowserVerification(evidence browser.Report) error {
	cwd, err := e.acceptanceCwd()
	if err != nil {
		return err
	}
	sessionID := e.SessionID()
	e.acceptanceMu.Lock()
	defer e.acceptanceMu.Unlock()
	active := e.acceptance != nil && e.acceptance.Cwd == cwd && e.acceptance.SessionID == sessionID && e.acceptance.Outcome == "running"
	report, err := e.lastAcceptanceLocked(cwd, sessionID)
	if err != nil {
		return err
	}
	if report == nil {
		report = &AcceptanceReport{Version: 1, SessionID: sessionID, Cwd: cwd}
	}
	if !active {
		report.ID, report.Request = rand.Text(), "browser verification"
		report.StartedAt, report.FinishedAt = time.Now(), time.Now()
		report.Outcome, report.Reason = "finished", ""
	}
	evidence = cloneBrowserReport(evidence)
	if evidence.Status == browser.StatusPass && !browserPassingEvidence(evidence) {
		evidence.Status, evidence.Reason = browser.StatusUnverified, "browser pass requires assertions and desktop/mobile screenshot evidence"
	}
	if evidence.Status != browser.StatusPass && evidence.Status != browser.StatusFail && evidence.Status != browser.StatusUnverified {
		evidence.Status, evidence.Reason = browser.StatusUnverified, "unknown browser evidence status"
	}
	if len(report.Browser) >= 16 {
		report.Browser = report.Browser[len(report.Browser)-15:]
	}
	report.Browser = append(report.Browser, evidence)
	if err := saveAcceptance(report); err != nil {
		return fmt.Errorf("persist browser acceptance: %w", err)
	}
	e.acceptance = report
	return nil
}

func browserPassingEvidence(report browser.Report) bool {
	if report.Version != 1 || report.Assertions <= 0 || report.EvidenceDir == "" || len(report.Steps) > 2*browser.MaxWorkflowSteps || len(report.Artifacts) != 2 {
		return false
	}
	assertions := 0
	asserted := map[string]bool{}
	for _, step := range report.Steps {
		if step.Status != browser.StatusPass || (step.Viewport != "desktop" && step.Viewport != "mobile") {
			return false
		}
		if strings.HasPrefix(step.Action, "assert_") {
			assertions++
			asserted[step.Viewport] = true
		}
	}
	screenshots := map[string]bool{}
	for _, artifact := range report.Artifacts {
		hash, err := hex.DecodeString(artifact.SHA256)
		if err != nil || len(hash) != sha256.Size || artifact.Bytes <= 0 || artifact.Bytes > browser.MaxScreenshotBytes || artifact.Name != artifact.Viewport+".png" {
			return false
		}
		if artifact.Viewport == "desktop" && artifact.Width == 1280 && artifact.Height == 800 || artifact.Viewport == "mobile" && artifact.Width == 390 && artifact.Height == 844 {
			screenshots[artifact.Viewport] = true
		}
	}
	return assertions == report.Assertions && asserted["desktop"] && asserted["mobile"] && screenshots["desktop"] && screenshots["mobile"]
}

// Summary renders evidence status without asserting subjective completion.
func (report *AcceptanceReport) Summary() string {
	var text strings.Builder
	outcome := map[string]string{"running": "执行中", "finished": "执行结束", "interrupted": "执行中断"}[report.Outcome]
	fmt.Fprintf(&text, "任务验收 %s（会话 %s，%s）\n%s\n", report.ID, report.SessionID, outcome, report.Request)
	if report.Reason != "" {
		fmt.Fprintf(&text, "%s\n", report.Reason)
	}
	for index, check := range report.Checks {
		status := map[string]string{"passed": "通过", "failed": "失败", "unverified": "未验证"}[check.Status]
		fmt.Fprintf(&text, "%d. [%s] %s: %s\n", index+1, status, check.Criterion, check.Reason)
		if result := check.Result; result != nil && !result.Skipped {
			fmt.Fprintf(&text, "   exit=%d，耗时=%s\n", result.ExitCode, result.Duration.Round(time.Millisecond))
			if result.Output != "" {
				fmt.Fprintf(&text, "%s\n", result.Output)
			}
		}
	}
	for _, evidence := range report.Regression {
		if evidence.Package == "" {
			fmt.Fprintf(&text, "独立验证者 [未验证]: %s\n", evidence.Reason)
			continue
		}
		fmt.Fprintf(&text, "反向回归 [%s] %s / %s: %s\n基线: %s；当前: %s\n旧实现 exit=%d；新实现 exit=%d；测试: %s\n", evidence.Status, evidence.Package, evidence.Pattern, evidence.Reason, evidence.Baseline, evidence.Current, evidence.Before.ExitCode, evidence.After.ExitCode, strings.Join(evidence.Tests, ", "))
		if evidence.Status != "passed" {
			if evidence.Before.Diagnostics != "" {
				fmt.Fprintf(&text, "旧实现诊断:\n%s\n", evidence.Before.Diagnostics)
			}
			if evidence.After.Diagnostics != "" {
				fmt.Fprintf(&text, "新实现诊断:\n%s\n", evidence.After.Diagnostics)
			}
		}
	}
	for _, evidence := range report.Browser {
		fmt.Fprintf(&text, "Browser [%s]: %s; assertions=%d; screenshots=%d; evidence=%s\n", evidence.Status, evidence.Reason, evidence.Assertions, len(evidence.Artifacts), evidence.EvidenceDir)
		for _, step := range evidence.Steps {
			fmt.Fprintf(&text, "  %s step=%d %s [%s]\n", step.Viewport, step.Index, step.Action, step.Status)
		}
		for _, artifact := range evidence.Artifacts {
			fmt.Fprintf(&text, "  %s %s %dx%d bytes=%d sha256=%s\n", artifact.Viewport, artifact.Name, artifact.Width, artifact.Height, artifact.Bytes, artifact.SHA256)
		}
	}
	return strings.TrimSpace(text.String())
}

func (e *Engine) recordRegressionEvidence(evidence delegate.RegressionEvidence) {
	e.acceptanceMu.Lock()
	defer e.acceptanceMu.Unlock()
	if e.acceptance != nil {
		if len(e.acceptance.Regression) >= 16 {
			e.acceptance.Regression = e.acceptance.Regression[1:]
		}
		e.acceptance.Regression = append(e.acceptance.Regression, evidence)
	}
}

func (e *Engine) invalidateAcceptance(command string) {
	cwd, err := e.acceptanceCwd()
	if err != nil {
		e.engineOutput("acceptance invalidation scope: " + err.Error())
		return
	}
	sessionID := e.SessionID()
	e.acceptanceMu.Lock()
	defer func() {
		e.acceptanceMu.Unlock()
		if err != nil {
			e.engineOutput("acceptance invalidation: " + err.Error())
		}
	}()
	report, err := e.lastAcceptanceLocked(cwd, sessionID)
	if err != nil || report == nil {
		return
	}
	e.acceptance = report
	invalidateBrowserReports(e.acceptance, "workspace may have changed; verify again")
	for index := range e.acceptance.Regression {
		if e.acceptance.Regression[index].Status == "passed" {
			e.acceptance.Regression[index].Status, e.acceptance.Regression[index].Reason = "unverified", "workspace may have changed; verify again"
		}
	}
	for index := range e.acceptance.Checks {
		check := &e.acceptance.Checks[index]
		if check.Status == "passed" && (command == "" || normalizeCommand(check.Command) == command) {
			check.Status, check.Result = "unverified", nil
			check.Reason = "校验后工作区可能已修改或出现新失败证据，需要重新校验"
		}
	}
	err = saveAcceptance(cloneAcceptance(e.acceptance))
}

func invalidateBrowserReports(report *AcceptanceReport, reason string) {
	for index := range report.Browser {
		evidence := &report.Browser[index]
		if evidence.Status == browser.StatusPass {
			evidence.Status, evidence.Reason = browser.StatusUnverified, reason
		}
	}
}

func (e *Engine) invalidateSavedAcceptance() {
	cwd, err := e.acceptanceCwd()
	if err != nil {
		e.engineOutput("回滚后验收证据读取失败: " + err.Error())
		return
	}
	sessionID := e.SessionID()
	e.acceptanceMu.Lock()
	var failure string
	defer func() {
		e.acceptanceMu.Unlock()
		if failure != "" {
			e.engineOutput(failure)
		}
	}()
	report, err := e.lastAcceptanceLocked(cwd, sessionID)
	if err != nil {
		failure = "回滚后验收证据读取失败: " + err.Error()
		return
	}
	if report == nil {
		return
	}
	invalidateBrowserReports(report, "workspace was rolled back; verify again")
	for index := range report.Regression {
		if report.Regression[index].Status == "passed" {
			report.Regression[index].Status, report.Regression[index].Reason = "unverified", "workspace was rolled back; verify again"
		}
	}
	for index := range report.Checks {
		if report.Checks[index].Status == "passed" {
			report.Checks[index].Status, report.Checks[index].Result = "unverified", nil
			report.Checks[index].Reason = "工作区已回滚，需要重新校验"
		}
	}
	e.acceptance = cloneAcceptance(report)
	if err := saveAcceptance(report); err != nil {
		failure = "回滚后验收证据保存失败: " + err.Error()
	}
}
