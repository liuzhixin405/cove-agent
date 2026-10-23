package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/automation"
	"github.com/liuzhixin405/cove-agent/internal/changeset"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/render"
	"github.com/liuzhixin405/cove-agent/internal/textmode"
)

const changesHelp = `/changes [list] [race|inbox|turn]      列出变更集：竞跑候选、维护结果、本轮改动
/changes show <ID>                     来源、基线、补丁摘要、验证记录、评审状态与能否应用
/changes diff <ID> [文件...]           打印补丁（可只看指定文件）
/changes export <ID> <目标.patch>      导出补丁到项目与配置目录之外
/changes apply <ID> [confirm]          校验通过后应用到工作区；headless 需带 confirm
/changes review <ID> accepted|rejected 记录评审结论（仅维护结果；不会应用）
ID：race:<运行ID>/<a|b>、inbox:<结果ID>、turn（本轮改动，只读，用 /undo 回退）。
apply 依次校验：可应用、工作区干净且项目/分支/提交未漂移、最后一次尝试的验证全部通过、补丁大小与哈希一致。`

var changesKinds = map[string]changeset.Kind{
	"race":  changeset.KindRace,
	"inbox": changeset.KindMaintenance,
	"turn":  changeset.KindTurn,
}

func changesKindLabel(kind changeset.Kind) string {
	switch kind {
	case changeset.KindRace:
		return "竞跑候选"
	case changeset.KindMaintenance:
		return "维护结果"
	case changeset.KindTurn:
		return "本轮改动"
	}
	return string(kind)
}

func (fe *frontend) changesCommands() []command.Command {
	return []command.Command{&feCmd{
		name: "changes", desc: "统一查看、导出与应用变更集（竞跑候选、维护结果、本轮改动）", help: changesHelp,
		category: catTasks, hints: []string{"list", "show", "diff", "export", "apply", "review"},
		mutates: func(args []string) bool {
			return len(args) > 0 && (args[0] == "apply" || args[0] == "review")
		},
		run: func(ctx context.Context, in command.Input) bool {
			if err := fe.runChanges(ctx, in); err != nil {
				fe.commandFailed = true
				fe.print("变更集: " + err.Error())
			}
			return true
		},
	}}
}

func (fe *frontend) runChanges(ctx context.Context, in command.Input) error {
	args := in.Args
	if len(args) == 0 {
		args = []string{"list"}
	}
	if args[0] == "help" || args[0] == "--help" {
		fe.print(changesHelp)
		return nil
	}
	project := in.Cwd
	if project == "" {
		var err error
		if project, err = os.Getwd(); err != nil {
			return err
		}
	}
	sources := fe.changesetSources()
	if _, ok := changesKinds[args[0]]; ok {
		args = append([]string{"list"}, args...)
	}
	if args[0] == "list" {
		if len(args) > 2 {
			return errors.New("用法: /changes list [race|inbox|turn]")
		}
		return fe.changesList(ctx, project, sources, args[1:])
	}
	if len(args) < 2 {
		return errors.New("缺少变更集 ID；输入 /changes 查看用法")
	}
	id := args[1]
	kind, local, err := parseChangesetID(id)
	if err != nil {
		return err
	}
	source, ok := sources[kind]
	if !ok {
		return fmt.Errorf("变更集来源 %s 不可用", kind)
	}
	switch args[0] {
	case "show":
		if len(args) != 2 {
			return errors.New("用法: /changes show <ID>")
		}
		cs, patch, err := loadChangeset(ctx, source, project, local)
		if err != nil {
			return err
		}
		lines := append(changesSummaryLines(cs, patch), changesDetailLines(cs)...)
		if kind == changeset.KindTurn {
			lines = append(lines, fe.turnAcceptanceLines()...)
		}
		fe.print(strings.Join(lines, "\n"))
		return nil
	case "diff":
		_, patch, err := loadChangeset(ctx, source, project, local)
		if err != nil {
			return err
		}
		text := filterPatch(patch, args[2:])
		if len(text) == 0 {
			fe.print("补丁为空")
			return nil
		}
		plain := !fe.interactive() || textmode.NoColor()
		fe.print(strings.TrimRight(render.ColorDiff(render.VisibleControls(string(text)), plain), "\n"))
		return nil
	case "export":
		if len(args) != 3 {
			return errors.New("用法: /changes export <ID> <目标.patch>")
		}
		_, patch, err := loadChangeset(ctx, source, project, local)
		if err != nil {
			return err
		}
		message, err := exportPatchFile(project, args[2], patch)
		if err != nil {
			return err
		}
		fe.print(message)
		return nil
	case "review":
		if len(args) != 3 || (args[2] != "accepted" && args[2] != "rejected") {
			return errors.New("用法: /changes review <ID> accepted|rejected")
		}
		if err := source.Review(ctx, project, local, args[2]); err != nil {
			return err
		}
		fe.print("评审已记录: " + args[2] + "；未应用")
		return nil
	case "apply":
		if len(args) > 3 || (len(args) == 3 && args[2] != "confirm") {
			return errors.New("用法: /changes apply <ID> [confirm]")
		}
		return fe.changesApply(ctx, project, id, kind, local, source, len(args) == 3)
	}
	return errors.New("未知子命令；输入 /changes 查看用法")
}

func (fe *frontend) changesList(ctx context.Context, project string, sources map[changeset.Kind]changeset.Source, filter []string) error {
	kinds := []changeset.Kind{changeset.KindTurn, changeset.KindRace, changeset.KindMaintenance}
	if len(filter) == 1 {
		kind, ok := changesKinds[filter[0]]
		if !ok {
			return errors.New("用法: /changes list [race|inbox|turn]")
		}
		kinds = []changeset.Kind{kind}
	}
	var all []changeset.Changeset
	var lines []string
	for _, kind := range kinds {
		source, ok := sources[kind]
		if !ok {
			continue
		}
		items, err := source.List(ctx, project)
		if err != nil {
			fe.commandFailed = true
			lines = append(lines, fmt.Sprintf("（%s 读取失败: %v）", changesKindLabel(kind), err))
			continue
		}
		all = append(all, items...)
	}
	// Newest first; the current turn always leads.
	sort.SliceStable(all, func(i, j int) bool {
		if (all[i].Kind == changeset.KindTurn) != (all[j].Kind == changeset.KindTurn) {
			return all[i].Kind == changeset.KindTurn
		}
		return all[i].Created.After(all[j].Created)
	})
	if len(all) == 0 && len(lines) == 0 {
		fe.print("没有变更集")
		return nil
	}
	for _, cs := range all {
		applicable := "可应用"
		if !cs.Applicable {
			applicable = "不可应用"
		}
		if cs.Kind == changeset.KindTurn {
			applicable = "未保存为补丁"
		}
		lines = append(lines, fmt.Sprintf("%-28s %s  %-8s %s  %s", cs.ID, changesKindLabel(cs.Kind), cs.Review.Decision, applicable, oneLine(cs.Title, 60)))
	}
	fe.print(strings.Join(lines, "\n"))
	return nil
}

// changesApply asks (interactive) or requires the typed confirm (headless),
// then applies under the project lock. Turn changesets are never applied.
func (fe *frontend) changesApply(ctx context.Context, project, id string, kind changeset.Kind, local string, source changeset.Source, confirmed bool) error {
	if kind == changeset.KindTurn {
		fe.commandFailed = true
		fe.print("不能应用 turn: " + turnNotApplicable)
		return nil
	}
	cs, patch, err := loadChangeset(ctx, source, project, local)
	if err != nil {
		return err
	}
	if !cs.Applicable {
		fe.commandFailed = true
		fe.print("不能应用 " + id + ": " + cs.NotApplicable)
		return nil
	}
	preview := changesSummaryLines(cs, patch)
	if fe.confirmThen("应用 "+id, preview, func() {
		// Bounded like race.Service.Select (30 s). Baseline.Apply detaches its
		// final `git apply` from ctx, so the deadline cannot half-apply.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		fe.changesApplyNow(ctx, project, id, local, source)
	}) {
		return nil
	}
	if !confirmed {
		fe.print(strings.Join(append([]string{"将应用以下变更集；headless 下需输入 /changes apply " + id + " confirm 才会执行（未应用）:"}, preview...), "\n"))
		return nil
	}
	fe.changesApplyNow(ctx, project, id, local, source)
	return nil
}

func (fe *frontend) changesApplyNow(ctx context.Context, project, id, local string, source changeset.Source) {
	configDir, err := config.ConfigDir()
	if err == nil {
		err = changeset.WithProjectLock(configDir, project, func() error {
			return source.Apply(ctx, project, local)
		})
	}
	switch {
	case err == nil:
		fe.print("已应用 " + id + "；/acceptance 旧证据已失效")
	case errors.Is(err, automation.ErrAuditNotSaved):
		fe.print("已应用 " + id + "；/acceptance 旧证据已失效。但应用记录未保存: " + err.Error())
	case errors.Is(err, changeset.ErrLocked):
		fe.commandFailed = true
		fe.print("未应用 " + id + ": 另一个 apply 正在进行，稍后再试")
	default:
		fe.commandFailed = true
		fe.print("未应用 " + id + ": " + err.Error())
	}
}

func loadChangeset(ctx context.Context, source changeset.Source, project, local string) (changeset.Changeset, []byte, error) {
	cs, loader, err := source.Load(ctx, project, local)
	if err != nil {
		return changeset.Changeset{}, nil, err
	}
	var patch []byte
	if loader != nil {
		if patch, err = loader(ctx); err != nil {
			return changeset.Changeset{}, nil, fmt.Errorf("读取补丁失败: %w", err)
		}
	}
	return cs, patch, nil
}

// changesSummaryLines is what a changeset is: source, title, baseline and the
// patch's size, hash and per-file line counts. It never includes patch text.
func changesSummaryLines(cs changeset.Changeset, patch []byte) []string {
	lines := []string{
		"ID: " + cs.ID,
		"来源: " + changesKindLabel(cs.Kind),
		"标题: " + oneLine(cs.Title, 120),
		"基线: " + baselineLabel(cs),
	}
	hash := cs.PatchHash
	if len(hash) > 12 {
		hash = hash[:12]
	}
	if hash == "" {
		hash = "（无）"
	}
	lines = append(lines, fmt.Sprintf("补丁: %d 字节，sha256 %s", cs.PatchBytes, hash))
	if len(patch) == 0 {
		return append(lines, "文件: 无改动")
	}
	stat, err := changeset.Summarize(patch)
	if err != nil {
		return append(lines, "文件: 无法解析补丁（"+err.Error()+"）")
	}
	lines = append(lines, fmt.Sprintf("文件: %d 个，+%d -%d", len(stat.Files), stat.Added, stat.Removed))
	for _, file := range stat.Files {
		if file.Binary {
			lines = append(lines, "  "+file.Path+"  （二进制）")
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s  +%d -%d", file.Path, file.Added, file.Removed))
	}
	return lines
}

// changesDetailLines is the evidence: each check, the review, and whether the
// changeset may be applied.
func changesDetailLines(cs changeset.Changeset) []string {
	var lines []string
	if len(cs.Checks) == 0 {
		lines = append(lines, "验证: 无记录")
	} else {
		lines = append(lines, "验证:")
		for _, check := range cs.Checks {
			argv := strings.Join(check.Argv, " ")
			if argv == "" {
				argv = "（任务本身）"
			}
			attempt := ""
			if check.Attempt > 0 {
				attempt = fmt.Sprintf("第 %d 次 ", check.Attempt)
			}
			lines = append(lines, fmt.Sprintf("  %s%s  exit=%d  %s", attempt, oneLine(argv, 100), check.ExitCode, check.Status))
		}
		if cs.Kind == changeset.KindMaintenance && len(changeset.LastAttemptChecks(cs.Checks)) == 1 {
			lines = append(lines, "  无独立验证命令：只有任务本身的退出码")
		}
	}
	review := "评审: " + cs.Review.Decision
	if cs.Review.At != nil {
		review += "（" + cs.Review.At.Local().Format(time.DateTime) + "）"
	}
	lines = append(lines, review)
	if cs.Applicable {
		lines = append(lines, "可应用: 是")
	} else {
		lines = append(lines, "可应用: 否（"+cs.NotApplicable+"）")
	}
	return lines
}

// turnAcceptanceLines shows the last acceptance report's checks next to the
// turn's diff; it is display only, not changeset evidence.
func (fe *frontend) turnAcceptanceLines() []string {
	if fe.eng == nil {
		return nil
	}
	report, err := fe.eng.LastAcceptance()
	if err != nil || report == nil || len(report.Checks) == 0 {
		return []string{"验收: 无记录（/acceptance 查看）"}
	}
	lines := []string{"验收（/acceptance）:"}
	for _, check := range report.Checks {
		line := "  " + oneLine(check.Criterion, 80)
		if check.Command != "" && check.Command != check.Criterion {
			line += "  " + oneLine(check.Command, 80)
		}
		line += "  " + check.Status
		if check.Reason != "" {
			line += "（" + oneLine(check.Reason, 80) + "）"
		}
		lines = append(lines, line)
	}
	return lines
}

func baselineLabel(cs changeset.Changeset) string {
	branch := strings.TrimPrefix(cs.Baseline.Branch, "refs/heads/")
	commit := cs.Baseline.Commit
	if len(commit) > 8 {
		commit = commit[:8]
	}
	if branch == "" && commit == "" {
		return "（未记录）"
	}
	if branch == "" {
		branch = "（无分支）"
	}
	return branch + "@" + commit
}

func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}

// filterPatch keeps the file sections of patch whose path is one of files
// (all of patch when files is empty).
func filterPatch(patch []byte, files []string) []byte {
	if len(files) == 0 {
		return patch
	}
	want := map[string]bool{}
	for _, file := range files {
		want[filepath.ToSlash(filepath.Clean(file))] = true
	}
	var out bytes.Buffer
	for _, section := range splitPatchSections(patch) {
		stat, err := changeset.Summarize(section)
		if err == nil && len(stat.Files) == 1 && want[stat.Files[0].Path] {
			out.Write(section)
		}
	}
	return out.Bytes()
}

// splitPatchSections cuts patch at each "diff --git " line.
func splitPatchSections(patch []byte) [][]byte {
	var sections [][]byte
	start := -1
	for offset := 0; offset < len(patch); {
		end := bytes.IndexByte(patch[offset:], '\n')
		next := len(patch)
		if end >= 0 {
			next = offset + end + 1
		}
		if bytes.HasPrefix(patch[offset:], []byte("diff --git ")) {
			if start >= 0 {
				sections = append(sections, patch[start:offset])
			}
			start = offset
		}
		offset = next
	}
	if start >= 0 {
		sections = append(sections, patch[start:])
	}
	return sections
}
