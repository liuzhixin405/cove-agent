package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

// handleHistoryClear is "/history clear [all] [confirm]": without confirm it
// asks through ask (the interactive confirmation box, which calls onYes when
// the person confirms) when one is given,
// otherwise says how many sessions would go and how to confirm by typing;
// with confirm it deletes them. The session in use is never deleted (it would only be
// written back at the end of the turn). "/history clean" repairs files and
// deletes nothing, which is what people kept mistaking for this.
func handleHistoryClear(eng *engine.Engine, all, confirm bool, ask func(scope string, n int, onYes func())) {
	store := eng.Store()
	if store == nil {
		termui.PrintSafe("会话存储不可用\n")
		return
	}
	cwd := currentProjectDir()
	scope := "当前项目"
	cmd := "/history clear confirm"
	if all {
		scope = "所有项目"
		cmd = "/history clear all confirm"
	}
	if !confirm {
		n := len(historyClearTargets(store, cwd, all, eng.SessionID()))
		if n == 0 {
			termui.PrintSafe("%s没有可删除的历史会话。\n", scope)
			return
		}
		if ask == nil {
			termui.PrintSafe("将删除%s的 %d 个历史会话（当前会话除外），删除后不可恢复。\n确认请输入: %s\n", scope, n, cmd)
			return
		}
		ask(scope, n, func() { historyClearNow(eng, store, cwd, all, scope) })
		return
	}
	historyClearNow(eng, store, cwd, all, scope)
}

// historyClearNow deletes the sessions and reports the count.
func historyClearNow(eng *engine.Engine, store *session.Store, cwd string, all bool, scope string) {
	deleted, err := historyClearIn(store, cwd, all, eng.SessionID())
	if err != nil {
		termui.PrintSafe("已删除 %d 个会话，另有失败: %v\n", deleted, err)
		return
	}
	termui.PrintSafe("已删除 %d 个会话（%s）。\n", deleted, scope)
}

// historyClearTargets lists the sessions /history clear would delete: every
// stored session of the project (or of every project with all), including
// the ones the list hides, except the session in use.
func historyClearTargets(store *session.Store, cwd string, all bool, keepID string) []session.Record {
	records, _ := store.List()
	if !all {
		records = session.FilterByProject(records, cwd)
	}
	out := make([]session.Record, 0, len(records))
	for _, r := range records {
		if keepID != "" && r.ID == keepID {
			continue
		}
		out = append(out, r)
	}
	return out
}

// historyClearIn deletes what historyClearTargets lists and returns how many
// were deleted; errors from individual deletions are joined and the rest
// still go.
func historyClearIn(store *session.Store, cwd string, all bool, keepID string) (int, error) {
	deleted := 0
	var errs []error
	for _, r := range historyClearTargets(store, cwd, all, keepID) {
		if err := store.Delete(r.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}

// handleHistoryDelete is "/history [all] delete <编号|id>": one session, by
// the number the matching list showed or by ID. The session in use cannot
// be deleted this way.
func handleHistoryDelete(input string, eng *engine.Engine, all bool) {
	store := eng.Store()
	if store == nil {
		termui.PrintSafe("会话存储不可用\n")
		return
	}
	input = strings.TrimSpace(input)
	if input == "" {
		termui.PrintSafe("用法: /history delete <编号|会话ID>\n")
		return
	}
	records, _ := listHistoryRecords(store, currentProjectDir(), all)
	id := input
	var idx int
	if _, err := fmt.Sscanf(input, "%d", &idx); err == nil {
		if idx < 1 || idx > len(records) {
			termui.PrintSafe("没有编号为 %d 的会话，输入 /history 查看列表。\n", idx)
			return
		}
		id = records[idx-1].ID
	}
	if id == eng.SessionID() {
		termui.PrintSafe("不能删除当前正在使用的会话。\n")
		return
	}
	if err := store.Delete(id); err != nil {
		termui.PrintSafe("删除会话失败: %v\n", err)
		return
	}
	termui.PrintSafe("已删除会话 %s。\n", id)
}

// parseHistoryClear recognises the argument forms of /history clear:
// "clear", "clear confirm", "clear all", "clear all confirm" (and
// "all clear [confirm]"). ok is false for anything else.
func parseHistoryClear(arg string) (all, confirm, ok bool) {
	fields := strings.Fields(strings.ToLower(arg))
	if len(fields) == 0 || len(fields) > 3 {
		return false, false, false
	}
	sawClear := false
	for _, f := range fields {
		switch f {
		case "clear":
			if sawClear {
				return false, false, false
			}
			sawClear = true
		case "all":
			all = true
		case "confirm":
			confirm = true
		default:
			return false, false, false
		}
	}
	if !sawClear {
		return false, false, false
	}
	return all, confirm, true
}
