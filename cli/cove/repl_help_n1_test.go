package main

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// /history clean stays a command but is no longer advertised in /help.
func TestHelpDoesNotListHistoryClean(t *testing.T) {
	buf := captureOut(t)
	printHelp((&frontend{}).install(registerAllCommands()), tool.NewRegistry(), nil)
	out := buf.String()
	if strings.Contains(out, "/history clean") {
		t.Fatalf("/help still lists /history clean:\n%s", out)
	}
	if !strings.Contains(out, "/history") {
		t.Fatalf("/help lost /history:\n%s", out)
	}
}

func TestCompactReportLine(t *testing.T) {
	cases := []struct {
		r    engine.CompactReport
		want []string
	}{
		{engine.CompactReport{Compressed: true, Summarized: true, BeforeTokens: 9000, AfterTokens: 1200}, []string{"压缩前 9000 tokens", "压缩后 1200 tokens"}},
		{engine.CompactReport{Reason: "对话只有 2 条消息，至少 4 条才能压缩", BeforeTokens: 50}, []string{"未压缩", "至少 4 条"}},
		{engine.CompactReport{Compressed: true, Reason: "摘要生成失败，已改为截断旧历史", BeforeTokens: 9000, AfterTokens: 3000}, []string{"部分压缩", "截断"}},
	}
	for _, c := range cases {
		got := compactReportLine(c.r)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%+v: %q lacks %q", c.r, got, w)
			}
		}
	}
}

func TestHelpForOneCommand(t *testing.T) {
	reg := (&frontend{}).install(registerAllCommands())
	out := captureOut(t)
	printCommandHelp(reg, "race")
	if s := out.String(); !strings.Contains(s, "/race run <spec.json>") || !strings.Contains(s, "run | list | show") {
		t.Fatalf("/help race = %q", s)
	}
	out = captureOut(t)
	printCommandHelp(reg, "/cls")
	if s := out.String(); !strings.Contains(s, "/clear") {
		t.Fatalf("/help with an alias = %q", s)
	}
	out = captureOut(t)
	printCommandHelp(reg, "rac")
	if s := out.String(); !strings.Contains(s, "未找到命令 /rac") || !strings.Contains(s, "/race") {
		t.Fatalf("unknown name must suggest the closest: %q", s)
	}
}

func TestHelpAndKeysLinkEachOther(t *testing.T) {
	out := captureOut(t)
	printHelp((&frontend{}).install(registerAllCommands()), tool.NewRegistry(), nil)
	if !strings.Contains(out.String(), "/help <命令>") || !strings.Contains(out.String(), "/keys") {
		t.Fatalf("help footer: %q", out.String())
	}
	if !strings.Contains(keybindingHelp, "/help") {
		t.Fatal("/keys must point back at /help")
	}
}

// Every command sits in a named /help section; none falls into the default one.
func TestEveryCommandHasACategory(t *testing.T) {
	reg := (&frontend{}).install(registerAllCommands())
	for _, c := range reg.All() {
		if commandCategory(c) == "" {
			t.Errorf("/%s has no /help category", c.Name())
		}
	}
}
