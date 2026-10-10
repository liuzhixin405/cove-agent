package main

import (
	"strconv"
	"strings"
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/render"
)

// blockStore keeps the recent tool blocks of the session so "/x <id>" can
// show the output a collapsed block hides. The engine always sent the full
// output along with the summary, but the front end dropped it (and the
// block's ID) on the floor, so the ▸ marker promised an expansion that did
// not exist.
type blockStore struct {
	mu     sync.Mutex
	blocks []render.Block // oldest first, at most blockStoreMax
}

// blockStoreMax bounds the blocks kept; older ones can no longer be expanded.
const blockStoreMax = 300

var sessionBlocks = &blockStore{}

func (s *blockStore) add(b render.Block) {
	if !b.Expandable() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks = append(s.blocks, b)
	if over := len(s.blocks) - blockStoreMax; over > 0 {
		s.blocks = append([]render.Block(nil), s.blocks[over:]...)
	}
}

// get returns the block with id, or the latest one for "".
func (s *blockStore) get(id string) (render.Block, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.blocks) == 0 {
		return render.Block{}, false
	}
	if id == "" {
		return s.blocks[len(s.blocks)-1], true
	}
	for i := len(s.blocks) - 1; i >= 0; i-- {
		if s.blocks[i].ID == id {
			return s.blocks[i], true
		}
	}
	return render.Block{}, false
}

// keybindingHelp is /keys: the input line's shortcuts, which /help (a list
// of commands) never mentioned.
const keybindingHelp = `输入快捷键
  Enter            发送          Ctrl+J / Alt+Enter / 行尾 \ + Enter   换行
  ↑ ↓              历史记录（提问时在选项间切换）    Ctrl+R   搜索历史
  ← →  Home End    移动光标      Ctrl+← →  Alt+B/F   按词移动
  Ctrl+A / Ctrl+E  行首 / 行尾   Ctrl+U / Ctrl+K     删到行首 / 行尾
  Ctrl+W / Alt+⌫   删除前一个词（输入为空时 Alt+⌫ 移除最后一个附件）
  Tab              补全命令、参数、@路径；输入为空时 Tab / Shift+Tab 切换 Agent 面板焦点
  Alt+M            展开/收起 Agent 面板          Alt+V   粘贴剪贴板图片（Windows）
  Esc              清空输入（Ctrl+Z 恢复）；输入为空且任务运行中时 2 秒内连按两次中断任务（COVE_ESC_INTERRUPT=0 关闭）
  Ctrl+C           中断当前任务  Ctrl+D              空行时退出
  Ctrl+L           清屏          Ctrl+Z              恢复被 Esc 清空的输入
授权/上限/提问提示：直接按选项键（y a p n、c s、1-9）即可，无需回车；输入 e 并回车可再输入一句拒绝理由
工具输出：标题后的 #N 可用 /x N 展开，/x 展开最近一个，/x N all 显示全部
命令列表：/help；/help <命令> 查看单个命令的用法`

// expandDefaultLines is how much of an output /x shows without "all".
const expandDefaultLines = 200

// expandCommand is /x [id] [all]: the full output of a tool block.
func expandCommand(args []string) string {
	id, all := "", false
	for _, a := range args {
		a = strings.TrimPrefix(strings.TrimSpace(a), "#")
		switch {
		case a == "all" || a == "全部":
			all = true
		case a != "":
			if _, err := strconv.Atoi(a); err != nil {
				return "用法: /x [编号] [all] —— 编号是工具块标题后的 #N，省略则展开最近一个"
			}
			id = a
		}
	}
	b, ok := sessionBlocks.get(id)
	if !ok {
		if id == "" {
			return "还没有可展开的工具输出"
		}
		return "找不到 #" + id + "（只保留最近 " + strconv.Itoa(blockStoreMax) + " 个工具块）"
	}
	limit := expandDefaultLines
	if all {
		limit = 0
	}
	out := engine.RenderExpanded(b, limit)
	if !all && strings.Count(b.Full, "\n") >= expandDefaultLines {
		out += "\n    \x1b[2m/x " + b.ID + " all 显示全部\x1b[0m"
	}
	return out
}
