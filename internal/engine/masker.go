package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/token"
)

// MaskingResult holds metrics about an output masking operation.
type MaskingResult struct {
	NewHistory  []api.Message
	MaskedCount int
	TokensSaved int
}

// ToolOutputMasker implements the Hybrid Backward Scanned FIFO algorithm
// to prevent large tool outputs from consuming the context window.
// Recent outputs (within protectionThreshold) are preserved; older ones
// are written to disk and replaced with placeholder markers.
type ToolOutputMasker struct {
	enabled              bool
	protectionThreshold  int             // tokens to protect from end (default 50000)
	minPrunableThreshold int             // min prunable before masking triggers (default 30000)
	outputDir            string          // ~/.cove/tool-outputs/
	exemptTools          map[string]bool // tools whose output is never masked
	// protectionScale multiplies protectionThreshold for larger context
	// windows; values below 1 are treated as 1.
	protectionScale float64
}

// NewToolOutputMasker creates a masker with sensible defaults.
func NewToolOutputMasker() *ToolOutputMasker {
	home, _ := os.UserHomeDir()
	return &ToolOutputMasker{
		enabled:              true,
		protectionThreshold:  50000,
		minPrunableThreshold: 30000,
		outputDir:            filepath.Join(home, ".cove", "tool-outputs"),
		exemptTools: map[string]bool{
			"question":       true,
			"todowrite":      true,
			"plan_mode":      true,
			"exit_plan_mode": true,
		},
	}
}

// Mask runs the Hybrid Backward Scanned FIFO algorithm on the message history.
// It scans from the end, protects the most recent ~protectionThreshold tokens,
// then masks older tool outputs that exceed minPrunableThreshold.
//
// Before that it collapses repeated identical tool results into stubs (see
// dedupeRepeatedResults). Rewriting history costs the prompt cache, so the
// stubs are applied only when disk masking rewrites history this round anyway
// or they save at least dedupeMinSavedTokens on their own. Applied stubs
// count towards MaskedCount and TokensSaved so the caller invalidates caches
// exactly as for disk masking.
func (m *ToolOutputMasker) Mask(history []api.Message, toolNames []string) (*MaskingResult, []api.Message) {
	if !m.enabled || len(history) == 0 {
		return &MaskingResult{}, history
	}
	deduped, dedupedCount, dedupeSaved := m.dedupeRepeatedResults(history)
	if dedupedCount > 0 && dedupeSaved < dedupeMinSavedTokens {
		// Not worth a rewrite by itself: go ahead only if disk masking is
		// rewriting history anyway.
		res, out := m.maskOldOutputs(history)
		if res.MaskedCount == 0 {
			return res, out
		}
	}
	if dedupedCount == 0 {
		return m.maskOldOutputs(history)
	}
	res, out := m.maskOldOutputs(deduped)
	res.MaskedCount += dedupedCount
	res.TokensSaved += dedupeSaved
	res.NewHistory = out
	return res, out
}

// dedupeMinSavedTokens is the least a dedupe pass must save to rewrite
// history on its own (when disk masking leaves history untouched).
const dedupeMinSavedTokens = 2000

// protectRecentResults is how many of the latest tool messages dedupe never
// touches: the model is most likely still reasoning about them.
const protectRecentResults = 4

// dedupeMinBytes is the smallest tool result worth replacing with a stub;
// shorter ones cost about as much as the stub itself.
const dedupeMinBytes = 512

// dedupeRepeatedResults replaces the second and later occurrences of an
// identical (sha256) tool result of at least dedupeMinBytes with a short stub
// naming the tool_call_id of the first occurrence (message indices shift on
// compaction; call IDs do not). The latest protectRecentResults
// tool messages are left alone, message count and tool_call_id are
// preserved, and the input slice is never mutated. Stubs are shorter than
// dedupeMinBytes, so a second pass is a no-op.
func (m *ToolOutputMasker) dedupeRepeatedResults(history []api.Message) ([]api.Message, int, int) {
	var toolIdx []int
	for i, msg := range history {
		if msg.Role == "tool" {
			toolIdx = append(toolIdx, i)
		}
	}
	limit := len(toolIdx) - protectRecentResults
	if limit < 2 {
		return history, 0, 0
	}
	protectFrom := toolIdx[limit]

	first := make(map[[32]byte]string)
	var out []api.Message
	count, saved := 0, 0
	for _, i := range toolIdx {
		msg := history[i]
		if len(msg.Content) < dedupeMinBytes || m.isExempt(msg.Name) ||
			strings.HasPrefix(msg.Content, maskedPrefix) {
			continue
		}
		sum := sha256.Sum256([]byte(msg.Content))
		orig, seen := first[sum]
		if !seen {
			first[sum] = msg.ToolCallID
			if first[sum] == "" {
				first[sum] = fmt.Sprintf("#%d", i)
			}
			continue
		}
		if i >= protectFrom {
			continue
		}
		if out == nil {
			out = make([]api.Message, len(history))
			copy(out, history)
		}
		stub := fmt.Sprintf("[identical to earlier tool result for call %s (%d bytes); content omitted]", orig, len(msg.Content))
		saved += token.Estimate(msg.Content) - token.Estimate(stub)
		out[i].Content = stub
		count++
	}
	if out == nil {
		return history, 0, 0
	}
	return out, count, saved
}

// dedupeStubRe matches a stub dedupeRepeatedResults wrote and captures the
// call it names.
var dedupeStubRe = regexp.MustCompile(`^\[identical to earlier tool result for call (\S+) \(\d+ bytes\); content omitted\]$`)

// restoreDedupedAcross is called before compaction drops, summarizes or
// trims msgs[:boundary]. A dedupe stub at or after boundary whose original
// lies before it would survive while its content does not: the model was
// left with "identical to earlier tool result for call X" and no call X.
// The first such stub gets the original's content back, and later stubs of
// the same original are pointed at it. It works in place and returns how
// many stubs it filled.
func restoreDedupedAcross(msgs []api.Message, boundary int) int {
	if boundary <= 0 || boundary >= len(msgs) {
		return 0
	}
	before := map[string]int{} // tool_call_id -> index, for results before boundary
	for i := 0; i < boundary; i++ {
		if msgs[i].Role == "tool" && msgs[i].ToolCallID != "" {
			before[msgs[i].ToolCallID] = i
		}
	}
	if len(before) == 0 {
		return 0
	}
	moved := map[string]string{} // lost call id -> call id of the stub now holding the content
	filled := 0
	for i := boundary; i < len(msgs); i++ {
		if msgs[i].Role != "tool" {
			continue
		}
		sm := dedupeStubRe.FindStringSubmatch(msgs[i].Content)
		if sm == nil {
			continue
		}
		id := sm[1]
		if holder, ok := moved[id]; ok {
			msgs[i].Content = strings.Replace(msgs[i].Content, "call "+id+" ", "call "+holder+" ", 1)
			continue
		}
		j, ok := before[id]
		if !ok {
			continue
		}
		msgs[i].Content = msgs[j].Content
		filled++
		if msgs[i].ToolCallID != "" {
			moved[id] = msgs[i].ToolCallID
		}
	}
	return filled
}

// maskOldOutputs is the disk-masking pass of Mask.
func (m *ToolOutputMasker) maskOldOutputs(history []api.Message) (*MaskingResult, []api.Message) {
	// ── Pass 1: backward scan to find protection boundary ──
	threshold := m.protectionThreshold
	if m.protectionScale > 1 {
		threshold = int(float64(threshold) * m.protectionScale)
	}
	protected := 0
	cutoffIdx := 0
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		protected += m.msgTokens(msg)
		if protected >= threshold {
			cutoffIdx = i
			break
		}
	}

	// ── Pass 2: scan from 0 to cutoffIdx, count prunable ──
	prunable := 0
	for i := 0; i < cutoffIdx; i++ {
		if m.maskable(history[i]) {
			prunable += token.Estimate(history[i].Content)
		}
	}

	if prunable < m.minPrunableThreshold {
		return &MaskingResult{}, history
	}

	// ── Pass 3: mask prunable tool outputs ──
	if err := os.MkdirAll(m.outputDir, 0700); err != nil {
		log.Warnf("masker: cannot create output dir: %v", err)
		return &MaskingResult{}, history
	}

	tokensSaved := 0
	maskedCount := 0
	// Copy the history (shallow copy is fine; we replace content of specific messages)
	newHistory := make([]api.Message, len(history))
	copy(newHistory, history)

	for i := 0; i < cutoffIdx; i++ {
		if !m.maskable(newHistory[i]) {
			continue
		}

		name := strings.ReplaceAll(newHistory[i].Name, "/", "_")
		// Named by content, not by message index: every session shares this
		// directory, and index-based names ("output_3_read.txt") were
		// overwritten by the next session or after compaction renumbered the
		// history, so a placeholder could point at someone else's output.
		sum := sha256.Sum256([]byte(newHistory[i].Content))
		filename := fmt.Sprintf("output_%s_%s.txt", hex.EncodeToString(sum[:8]), name)
		filePath := filepath.Join(m.outputDir, filename)

		if err := os.WriteFile(filePath, []byte(newHistory[i].Content), 0600); err != nil {
			log.Warnf("masker: write failed: %v", err)
			continue
		}

		n := token.Estimate(newHistory[i].Content)
		tokensSaved += n
		newHistory[i].Content = fmt.Sprintf("%s%s...] %d tokens masked to %s",
			maskedPrefix, strings.ReplaceAll(name, "_", " "), n, filePath)
		maskedCount++
	}

	return &MaskingResult{
		NewHistory:  newHistory,
		MaskedCount: maskedCount,
		TokensSaved: tokensSaved,
	}, newHistory
}

// msgTokens estimates the token count of a message with the shared
// estimator (see countTokens). It used to be bytes/4, which reads Chinese
// text at three quarters of its size.
func (m *ToolOutputMasker) msgTokens(msg api.Message) int {
	return countTokens([]api.Message{msg})
}

// maskedPrefix marks a tool message whose output has already been masked to disk.
const maskedPrefix = "[toolu_vrtx_01Masked"

// maskable reports whether a message is a non-exempt tool output large enough to
// mask, and not already masked (re-masking would replace a real file reference
// with a pointer to the placeholder, breaking the chain and inflating savings).
func (m *ToolOutputMasker) maskable(msg api.Message) bool {
	return msg.Role == "tool" &&
		!m.isExempt(msg.Name) &&
		len(msg.Content) > 100 &&
		!strings.HasPrefix(msg.Content, maskedPrefix)
}

// isExempt checks whether a tool name should never be masked.
func (m *ToolOutputMasker) isExempt(toolName string) bool {
	for _, part := range strings.Split(toolName, "__") {
		if m.exemptTools[part] {
			return true
		}
	}
	return m.exemptTools[toolName]
}
