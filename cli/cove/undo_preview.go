package main

// undoPreviewLines is what the whole-tree /undo confirmation shows: the
// target and the latest checkpoints, so the person sees where the tree is
// about to go before pressing y.
func undoPreviewLines(eng interface{ ListCheckpoints() []string }, hash string) []string {
	target := "最近检查点"
	if hash != "" {
		target = hash
	}
	lines := []string{"目标：" + target, "整树回退会覆盖工作区当前内容；回退前自动备份，可用 /undo <备份ID> 撤销。"}
	if eng == nil {
		return lines
	}
	list := eng.ListCheckpoints()
	if len(list) > 5 {
		list = list[:5]
	}
	if len(list) == 0 {
		return append(lines, "当前没有检查点记录，回退可能失败。")
	}
	lines = append(lines, "最近检查点：")
	for _, l := range list {
		lines = append(lines, "  "+l)
	}
	return lines
}
