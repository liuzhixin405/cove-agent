package main

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/delegate"
)

func agentActivityLabel(activity delegate.Activity) string {
	switch activity.Stage {
	case "starting":
		return "启动中"
	case "model":
		return "调用模型"
	case "tool":
		return "执行工具"
	case "waiting":
		return "等待授权或输入"
	case "cancelled":
		return "已取消"
	case "timed_out":
		return "已超时"
	case "finished":
		if activity.Success {
			return "已完成"
		}
		return "未完成"
	default:
		return "等待中"
	}
}

func agentMapLines(entries []delegate.Activity, selected int, running bool, now time.Time) ([]string, int) {
	active := 0
	for _, entry := range entries {
		if entry.Ended.IsZero() {
			active++
		}
	}
	mainState := "空闲"
	if running {
		mainState = "执行中"
	}
	lines := []string{
		fmt.Sprintf("Agent Map | 活跃 %d / 最近 %d | Alt+M 收起", active, len(entries)),
		"主 Agent: " + mainState + " | Tab 切换焦点，Esc 返回",
		"", "", "", "", "", "",
	}
	if len(entries) == 0 {
		lines[2] = "  暂无子 agent 活动"
		return lines, 0
	}
	if selected < 0 {
		selected = 0
	}
	if selected >= len(entries) {
		selected = len(entries) - 1
	}
	start := selected / 3 * 3
	for index := start; index < start+3 && index < len(entries); index++ {
		entry := entries[index]
		marker := " "
		if index == selected {
			marker = ">"
		}
		lines[2+index-start] = fmt.Sprintf(" %s |- %s · %s", marker, agentMapText(entry.TaskID), agentActivityLabel(entry))
	}
	entry := entries[selected]
	duration := agentActivityDuration(entry, now)
	lines[5] = fmt.Sprintf("[%d/%d] %s | %s | %s", selected+1, len(entries), agentMapText(entry.Model), agentActivityLabel(entry), duration)
	lines[6] = "任务: " + agentMapText(entry.Task)
	step := entry.LastStep
	if step == "" {
		step = agentActivityLabel(entry)
	}
	lines[7] = "最近: " + agentMapText(step)
	return lines, len(entries)
}

func agentMapText(text string) string {
	return strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return ' '
		}
		return value
	}, text)
}

func agentActivityDuration(entry delegate.Activity, now time.Time) time.Duration {
	end := entry.Ended
	if end.IsZero() {
		end = now
	}
	duration := end.Sub(entry.Started).Round(time.Second)
	if duration < 0 {
		return 0
	}
	return duration
}

type agentMapCommand struct{ frontend *frontend }

func (fe *frontend) agentMapCommands() []command.Command {
	return []command.Command{&agentMapCommand{frontend: fe}}
}

func (*agentMapCommand) Name() string      { return "agents" }
func (*agentMapCommand) Aliases() []string { return nil }
func (*agentMapCommand) Description() string {
	return "查看 agent 活动快照；Alt+M 展开实时面板"
}
func (*agentMapCommand) Category() string            { return catTasks }
func (*agentMapCommand) MutatesEngine([]string) bool { return false }
func (*agentMapCommand) Help() string {
	return "/agents - 查看最近 agent 状态（运行中也可使用）"
}
func (c *agentMapCommand) Execute(context.Context, command.Input) (command.Output, error) {
	if c.frontend == nil || c.frontend.eng == nil {
		return command.Output{Message: "Agent Map 暂不可用"}, nil
	}
	entries := c.frontend.eng.AgentActivities()
	lines, _ := agentMapLines(entries, len(entries)-1, c.frontend.running(), time.Now())
	output := []string{lines[0], "主 Agent: 空闲"}
	if c.frontend.running() {
		output[1] = "主 Agent: 执行中"
	}
	if len(entries) == 0 {
		output = append(output, "暂无子 agent 活动")
	}
	for _, entry := range entries {
		output = append(output,
			fmt.Sprintf("|- %s | %s | %s | %s", agentMapText(entry.TaskID), agentActivityLabel(entry), agentMapText(entry.Model), agentActivityDuration(entry, time.Now())),
			"   任务: "+agentMapText(entry.Task))
		if entry.LastStep != "" {
			output = append(output, "   最近: "+agentMapText(entry.LastStep))
		}
	}
	return command.Output{Message: strings.Join(output, "\n")}, nil
}
