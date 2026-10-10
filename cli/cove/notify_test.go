package main

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/race"
)

func TestNotifyPrintsNowWhenIdleAndLaterDuringATurn(t *testing.T) {
	buf := captureOut(t)
	notify("竞跑 r1 完成", "/race show r1")
	if !strings.Contains(buf.String(), "◆ 竞跑 r1 完成") {
		t.Fatalf("idle notify not printed: %q", buf.String())
	}
	buf.Reset()
	bgSummaries.beginTurn()
	notify("竞跑 r2 完成", "")
	if strings.Contains(buf.String(), "r2") {
		t.Fatal("must wait for the turn to end")
	}
	bgSummaries.endTurn()
	if !strings.Contains(buf.String(), "◆ 竞跑 r2 完成") {
		t.Fatalf("not printed at turn end: %q", buf.String())
	}
}

func TestRaceFinishedText(t *testing.T) {
	r := &race.Report{ID: "abc", Status: "completed"}
	r.Candidates[0].Correct = true
	text, hint := raceFinishedText(r)
	if !strings.Contains(text, "竞跑 abc 完成") || !strings.Contains(text, "1/2 个候选通过") || hint != "/race show abc" {
		t.Fatalf("%q %q", text, hint)
	}
	for status, want := range map[string]string{"timeout": "超时", "cancelled": "已取消"} {
		if text, _ := raceFinishedText(&race.Report{ID: "x", Status: status}); !strings.Contains(text, want) {
			t.Errorf("%s: %q", status, text)
		}
	}
}
