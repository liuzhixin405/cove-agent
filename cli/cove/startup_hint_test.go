package main

import (
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/diagnostic"
)

// The start-up line says what went wrong and what to do, on its own: the
// old "错误日志里有 N 类未处理的问题" sent the person to a command to learn
// anything. The most recent problem leads; the rest is a count.
func TestStartupHintNamesTheProblemAndItsRemedy(t *testing.T) {
	open := []diagnostic.OpenProblem{
		{Code: "E2001", Model: "anthropic.claude-haiku-4-5", Count: 3, Last: time.Date(2026, 10, 21, 15, 7, 0, 0, time.Local),
			Message: "API 服务不可达", Recovery: "检查网络连接和代理设置，确认 base_url 正确。"},
		{Code: "E2005", Model: "x", Count: 1, Message: "请求参数错误", Recovery: "检查模型名"},
	}
	got := startupUnresolvedHint(open)
	for _, want := range []string{"上次运行时 API 服务不可达", "E2001", "anthropic.claude-haiku-4-5", "3 次", "10-21 15:07",
		"检查网络连接和代理设置，确认 base_url 正确。", "另有 1 类", "/diagnose errors", "/diagnose archive"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "正确。。") || strings.Contains(got, "未处理的问题") {
		t.Errorf("hint has a doubled full stop or the old wording:\n%s", got)
	}
	one := startupUnresolvedHint(open[1:])
	if strings.Contains(one, "另有") || strings.Contains(one, " 次") {
		t.Errorf("single problem, count 1, must not say 另有 or N 次:\n%s", one)
	}
}
