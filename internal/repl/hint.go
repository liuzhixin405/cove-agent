package repl

// A transient hint in the status row: "已清空，Ctrl+Z 恢复", "竞跑完成，
// /race show r3". It stays until the next submitted line.

// hintText is guarded by consoleMu.
var hintText string

// SetHint shows text in the status row above the input until the next line
// is submitted; "" clears it now.
func SetHint(text string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	hintText = text
	if activeReader != nil && activeReader.reading {
		activeReader.redrawLocked(activeReader.renderBuf, activeReader.renderCursor)
	}
}

func currentHintLocked() string { return hintText }

func clearHintLocked() { hintText = "" }
