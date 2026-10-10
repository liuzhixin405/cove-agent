package repl

import (
	"strings"
	"time"
	"unicode"
)

const panelMaxRows = 8
const panelRefreshInterval = 2 * time.Second

func (lr *LineReader) SetInteractionState(state string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	lr.interactionState = state
	if lr.reading && lr.statusLineLocked() != lr.lastStatus {
		lr.redrawLocked(lr.renderBuf, lr.renderCursor)
	}
}

func (lr *LineReader) statusLineLocked() string {
	var parts []string
	if lr.interactionState == "" {
		if lr.inputStatus != "" {
			parts = append(parts, lr.inputStatus)
		}
		if h := currentHintLocked(); h != "" {
			parts = append(parts, h)
		}
		return cleanStatus(strings.Join(parts, " | "))
	}
	if permInputCh != nil {
		message := "等待你回答"
		switch permTitle {
		case "等待授权":
			message = "等待你确认授权"
		case "等待确认":
			message = "等待你确认"
		}
		parts = append(parts, message)
		if permHint != "" {
			parts = append(parts, permHint)
		}
	} else {
		switch {
		case streamingActive || lr.interactionState == "执行中":
			parts = append(parts, "正在处理任务")
		case lr.interactionState == "已停止":
			parts = append(parts, "任务已停止")
		case lr.interactionState != "空闲":
			parts = append(parts, lr.interactionState)
		}
		if len(lr.choices) > 0 {
			message := "选择候选项"
			if lr.choiceSearch {
				message = "选择历史会话"
			} else if strings.HasPrefix(strings.TrimSpace(string(lr.renderBuf)), "/") {
				message = "选择命令"
			}
			parts = append(parts, message)
		} else if lr.panelOpen && lr.panelFocused {
			parts = append(parts, "正在查看 Agent 面板")
		}
	}
	if lr.inputStatus != "" {
		parts = append(parts, lr.inputStatus)
	}
	if h := currentHintLocked(); h != "" {
		parts = append(parts, h)
	}
	return cleanStatus(strings.Join(parts, " | "))
}

// cleanStatus replaces control characters so a hint cannot break the row.
func cleanStatus(s string) string {
	return strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return ' '
		}
		return value
	}, s)
}

func (lr *LineReader) SetPanelSource(source func(int) ([]string, int)) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	lr.panelSource = source
}

func (lr *LineReader) SetPanelWake(wake <-chan struct{}) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	lr.panelWake = wake
}

func (lr *LineReader) RefreshPanel() {
	lr.refreshPanel(time.Now(), false)
}

func (lr *LineReader) refreshPanel(now time.Time, force bool) {
	consoleMu.Lock()
	source, open, index := lr.panelSource, lr.panelOpen, lr.panelIndex
	if !open || source == nil || (!force && now.Sub(lr.panelRefreshed) < panelRefreshInterval) {
		consoleMu.Unlock()
		return
	}
	lr.panelRefreshed = now
	consoleMu.Unlock()
	lines, count := source(index)
	if count == 0 {
		index = 0
	}
	if count > 0 && index >= count {
		index = count - 1
		lines, count = source(index)
	}
	if len(lines) > panelMaxRows {
		lines = lines[:panelMaxRows]
	}
	clean := make([]string, len(lines))
	for position, line := range lines {
		clean[position] = strings.Map(func(value rune) rune {
			if unicode.IsControl(value) {
				return ' '
			}
			return value
		}, line)
	}
	consoleMu.Lock()
	defer consoleMu.Unlock()
	lr.panelIndex, lr.panelCount, lr.panelLines = index, count, clean
	if lr.reading {
		lr.redrawLocked(lr.renderBuf, lr.renderCursor)
	}
}

func (lr *LineReader) togglePanel() bool {
	consoleMu.Lock()
	if lr.panelSource == nil {
		consoleMu.Unlock()
		return false
	}
	lr.panelOpen = !lr.panelOpen
	lr.panelFocused = false
	if !lr.panelOpen {
		lr.panelLines = nil
	}
	consoleMu.Unlock()
	lr.refreshPanel(time.Now(), true)
	return true
}

func (lr *LineReader) closePanel() bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if !lr.panelOpen {
		return false
	}
	lr.panelOpen, lr.panelFocused, lr.panelLines = false, false, nil
	return true
}

func (lr *LineReader) togglePanelFocus() bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if !lr.panelOpen || permInputCh != nil || len(lr.choices) > 0 {
		return false
	}
	lr.panelFocused = !lr.panelFocused
	return true
}

func (lr *LineReader) panelHasFocus() bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	return lr.panelOpen && lr.panelFocused && permInputCh == nil
}

func (lr *LineReader) movePanel(delta int) bool {
	consoleMu.Lock()
	if !lr.panelOpen {
		consoleMu.Unlock()
		return false
	}
	lr.panelIndex += delta
	if lr.panelIndex < 0 {
		lr.panelIndex = 0
	}
	if lr.panelCount > 0 && lr.panelIndex >= lr.panelCount {
		lr.panelIndex = lr.panelCount - 1
	}
	consoleMu.Unlock()
	lr.refreshPanel(time.Now(), true)
	return true
}

func (lr *LineReader) panelRows(height int) int {
	count := len(lr.displayRowsLocked())
	if count > height-5 {
		count = height - 5
	}
	if count < 0 {
		return 0
	}
	return count
}
