package engine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/safepath"
)

type ImplementationPlan struct {
	Summary   string   `json:"summary"`
	Files     []string `json:"files"`
	Decisions []string `json:"decisions"`
	Checks    []string `json:"checks"`
	Decision  string   `json:"decision"`
}

type WorkflowSnapshot struct {
	Version    int                 `json:"version"`
	SessionID  string              `json:"session_id"`
	RequestKey string              `json:"request_key,omitempty"`
	Error      string              `json:"-"`
	Mode       string              `json:"mode"`
	TaskID     string              `json:"task_id,omitempty"`
	Pending    bool                `json:"pending"`
	Active     bool                `json:"active"`
	Plan       *ImplementationPlan `json:"plan,omitempty"`
}

type reviewWorkflow struct {
	mu       sync.Mutex
	state    WorkflowSnapshot
	loadedID string
	tracked  bool
}

func workflowPath(sessionID string) (string, error) {
	root, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	if sessionID == "" {
		return "", fmt.Errorf("没有活动会话")
	}
	return filepath.Join(root, "workflows", fmt.Sprintf("%x.json", sha256.Sum256([]byte(sessionID)))), nil
}

func (e *Engine) loadWorkflowLocked() error {
	id := e.SessionID()
	if e.workflow.loadedID == id && id != "" {
		return nil
	}
	path, err := workflowPath(id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	state := WorkflowSnapshot{Version: 1, SessionID: id, Mode: "direct"}
	tracked := false
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读取流程状态失败: %w", err)
	}
	if err == nil {
		if len(data) > 1<<20 || json.Unmarshal(data, &state) != nil || state.Version != 1 || state.SessionID != id || (state.Mode != "direct" && state.Mode != "review" && state.Mode != "once") || (state.Pending && !state.Active) {
			return fmt.Errorf("流程状态损坏，拒绝在未知审批状态下实现")
		}
		tracked = true
	}
	e.workflow.state, e.workflow.loadedID, e.workflow.tracked = state, id, tracked
	return nil
}

func (e *Engine) saveWorkflowLocked() error {
	path, err := workflowPath(e.workflow.state.SessionID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(e.workflow.state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return fsatomic.WriteFile(path, data, 0600)
}

func (e *Engine) SetWorkflowMode(mode string) error {
	if mode != "direct" && mode != "review" && mode != "once" {
		return fmt.Errorf("可选流程: direct、review、once")
	}
	e.workflow.mu.Lock()
	defer e.workflow.mu.Unlock()
	if err := e.loadWorkflowLocked(); err != nil {
		return err
	}
	previous := e.workflow.state
	e.workflow.state.Mode = mode
	if mode == "direct" {
		e.workflow.state.Active, e.workflow.state.Pending = false, false
	}
	if err := e.saveWorkflowLocked(); err != nil {
		e.workflow.state = previous
		return err
	}
	e.workflow.tracked = true
	return nil
}

func (e *Engine) WorkflowStatus() WorkflowSnapshot {
	e.workflow.mu.Lock()
	defer e.workflow.mu.Unlock()
	if err := e.loadWorkflowLocked(); err != nil {
		return WorkflowSnapshot{Mode: "direct", Error: err.Error()}
	}
	state := e.workflow.state
	if state.Mode == "" {
		state.Mode = "direct"
	}
	if state.Plan != nil {
		plan := *state.Plan
		plan.Files = append([]string(nil), plan.Files...)
		plan.Decisions = append([]string(nil), plan.Decisions...)
		plan.Checks = append([]string(nil), plan.Checks...)
		state.Plan = &plan
	}
	return state
}

func (e *Engine) beginReviewWorkflow(resuming bool, messages ...api.Message) error {
	e.workflow.mu.Lock()
	defer e.workflow.mu.Unlock()
	if err := e.loadWorkflowLocked(); err != nil {
		return err
	}
	requestKey := ""
	if len(messages) > 0 {
		data, err := json.Marshal(messages[0])
		if err != nil {
			return err
		}
		requestKey = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	if e.workflow.state.TaskID != "" && ((resuming && (requestKey == "" || requestKey == e.workflow.state.RequestKey)) || (e.workflow.state.Pending && requestKey != "" && requestKey == e.workflow.state.RequestKey)) {
		return nil
	}
	mode := e.workflow.state.Mode
	active := mode == "review" || mode == "once"
	if mode == "once" || mode == "" {
		mode = "direct"
	}
	previous := e.workflow.state
	e.workflow.state = WorkflowSnapshot{Version: 1, SessionID: e.SessionID(), RequestKey: requestKey, Mode: mode, TaskID: time.Now().UTC().Format("20060102T150405.000000000"), Active: active, Pending: active}
	if e.workflow.tracked {
		if err := e.saveWorkflowLocked(); err != nil {
			e.workflow.state = previous
			return err
		}
	}
	return nil
}

func (e *Engine) reviewPending() bool {
	e.workflow.mu.Lock()
	defer e.workflow.mu.Unlock()
	return e.workflow.state.Pending
}

func reviewPlanList(input map[string]any, key string) []string {
	var values []string
	switch items := input[key].(type) {
	case []string:
		values = append(values, items...)
	case []any:
		for _, item := range items {
			value, ok := item.(string)
			if !ok {
				return nil
			}
			values = append(values, value)
		}
	}
	for index := range values {
		values[index] = strings.TrimSpace(values[index])
		if values[index] == "" || len(values[index]) > 2000 {
			return nil
		}
	}
	if len(values) > 40 {
		return nil
	}
	return values
}

func reviewPlan(input map[string]any) (*ImplementationPlan, error) {
	summary, _ := input["summary"].(string)
	plan := &ImplementationPlan{Summary: strings.TrimSpace(summary), Files: reviewPlanList(input, "files"), Decisions: reviewPlanList(input, "decisions"), Checks: reviewPlanList(input, "checks"), Decision: "pending"}
	if plan.Summary == "" || len(plan.Summary) > 12000 || len(plan.Files) == 0 || len(plan.Decisions) == 0 || len(plan.Checks) == 0 {
		return nil, permissionDenied("exit_plan_mode", "先提交完整实现方案：summary、files（涉及文件）、decisions（关键决策）、checks（验收标准），未确认前不得实现")
	}
	return plan, nil
}

func (e *Engine) recordReviewPlan(plan *ImplementationPlan) error {
	e.workflow.mu.Lock()
	defer e.workflow.mu.Unlock()
	previous := e.workflow.state
	e.workflow.state.Plan = plan
	if err := e.saveWorkflowLocked(); err != nil {
		e.workflow.state = previous
		return err
	}
	return nil
}

func (e *Engine) recordReviewDecision(plan *ImplementationPlan, approved bool) error {
	e.workflow.mu.Lock()
	defer e.workflow.mu.Unlock()
	previous := e.workflow.state
	plan.Decision = "rejected"
	if approved {
		plan.Decision = "approved"
		e.workflow.state.Pending = false
	}
	e.workflow.state.Plan = plan
	if err := e.saveWorkflowLocked(); err != nil {
		e.workflow.state = previous
		return err
	}
	return nil
}

func reviewFileSnapshots(cwd string, files []string) (map[string]string, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	states := make(map[string]string)
	for _, file := range files {
		if !safepath.Within(cwd, file) {
			return nil, fmt.Errorf("方案文件必须在当前项目内: %s", file)
		}
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			states[file] = "missing"
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > 16<<20 {
			return nil, fmt.Errorf("方案必须列出具体文件（不超过 16 MiB）: %s", file)
		}
		reader, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		count, readErr := io.Copy(hash, io.LimitReader(reader, (16<<20)+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || count > 16<<20 {
			return nil, fmt.Errorf("无法稳定读取方案文件: %s", file)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		states[file] = fmt.Sprintf("%s:%x:%d:%d:%d", resolved, hash.Sum(nil), info.Size(), info.ModTime().UnixNano(), info.Mode())
	}
	return states, nil
}

const reviewWorkflowInstructions = `
本任务启用先确认后实现流程。只读调查现有实现，提出实现方案：目标、涉及文件、关键决策（依赖、接口兼容性等）、验收标准。未获得用户明确批准前不得写文件、执行会写入的命令或委托实现。
方案准备好后调用 exit_plan_mode，传入 summary、files 字符串数组、decisions 字符串数组和 checks 字符串数组，由引擎向用户确认。不要把普通回答、工具 allow 规则或之前任务的许可当作本任务批准。用户拒绝后可根据反馈调整方案，不能自行开始实现。批准后遵守原有工具权限，不得把 AI 自述当作测试证据。
`
