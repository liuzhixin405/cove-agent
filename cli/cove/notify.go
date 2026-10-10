package main

import (
	"fmt"

	"github.com/liuzhixin405/cove-agent/internal/race"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

// notify tells the person a background result arrived: one dim line above
// the input (held until the turn ends when one is printing, like the
// background summary), and hint in the status row until their next line.
// Results used to sit in /race show or /inbox until someone thought to look.
func notify(text, hint string) {
	bgSummaries.deliver("  " + termui.Styled(termui.Dim, "◆ "+text) + "\n")
	if hint != "" {
		repl.SetHint(hint)
	}
}

// raceFinishedText is the notification for a race that ended.
func raceFinishedText(r *race.Report) (text, hint string) {
	hint = "/race show " + r.ID
	switch r.Status {
	case "timeout":
		return fmt.Sprintf("竞跑 %s 超时，%s 查看", r.ID, hint), hint
	case "cancelled":
		return fmt.Sprintf("竞跑 %s 已取消，%s 查看", r.ID, hint), hint
	}
	passed := 0
	for _, c := range r.Candidates {
		if c.Correct {
			passed++
		}
	}
	return fmt.Sprintf("竞跑 %s 完成：%d/2 个候选通过验证，%s 查看，/race select %s <a|b> 应用", r.ID, passed, hint, r.ID), hint
}
