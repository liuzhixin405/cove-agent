package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/memory"
	"github.com/liuzhixin405/cove-agent/internal/skills"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// reviewMinTurns is how many turns must end, the current one included,
// since the last review that ran before the next one may start.
const reviewMinTurns = 3

// SetNonInteractive marks the engine as serving a process that exits right
// after its answer (cove -p): the turn-end skill review is skipped, since it
// would be abandoned at exit after its paid request was sent.
func (e *Engine) SetNonInteractive(v bool) {
	e.bgMu.Lock()
	e.nonInteractive = v
	e.bgMu.Unlock()
}

// reviewMessages returns the history snapshot the turn-end review should
// look at, or nil when it should not run: a non-interactive engine, no
// skill manager, too little conversation, fewer than reviewMinTurns turns or
// 4 messages since the last review that ran, a turn that ran no tool that is
// not read-only (a lookup teaches no workflow), or a review still running.
// It is called once per turn end and counts the turn. The review itself runs
// on the turn's background goroutine (runBackgroundWork) after the part
// WaitBackground waits for.
func (e *Engine) reviewMessages() []api.Message {
	e.bgMu.Lock()
	defer e.bgMu.Unlock()
	if e.nonInteractive || e.skillMgr == nil {
		return nil
	}
	e.turnsSinceReview++
	if len(e.messages) < 6 {
		return nil // not enough conversation to review
	}
	// Throttle: only run if at least 4 new messages since the last review.
	if e.reviewRunning || len(e.messages)-e.lastReviewMsgCount < 4 {
		return nil
	}
	if e.turnsSinceReview < reviewMinTurns || !e.turnUsedWork {
		return nil
	}
	e.reviewRunning = true
	// A snapshot, so the background goroutine does not read e.messages
	// concurrently with a later turn appending to it.
	return append([]api.Message(nil), e.messages...)
}

// rebaseReviewThrottle moves the review throttle with a compaction that
// shrank the history from before to after messages. lastReviewMsgCount is a
// message count of the old history; only /new and /resume reset it, so once
// a compaction left fewer messages than it, len(messages)-lastReviewMsgCount
// stayed below the threshold and skill review was off for the rest of the
// session. The messages not yet reviewed are kept in the count, and a review
// still running on the old history does not move the throttle when it ends
// (conversationGen).
func (e *Engine) rebaseReviewThrottle(before, after int) {
	e.bgMu.Lock()
	defer e.bgMu.Unlock()
	e.lastReviewMsgCount -= before - after
	if e.lastReviewMsgCount < 0 {
		e.lastReviewMsgCount = 0
	}
	e.conversationGen++
}

// reviewTimeout bounds one background review request.
const reviewTimeout = 30 * time.Second

// reviewResult names the skills a review saved: new ones, and learned
// skills whose auto-generated file it rewrote.
type reviewResult struct {
	added, updated []string
}

// runReview asks the background model for reusable workflows in msgs and
// saves them. The throttle (lastReviewMsgCount) only moves when the review
// really ran: a skipped or failed one is tried again after the next turn.
// sessionID is the session the reviewed turn belonged to, captured with the
// snapshot (see applyReview).
func (e *Engine) runReview(msgs []api.Message, sessionID string) reviewResult {
	defer func() {
		e.bgMu.Lock()
		e.reviewRunning = false
		e.bgMu.Unlock()
	}()
	e.bgMu.Lock()
	gen := e.conversationGen
	e.bgMu.Unlock()
	if e.costTracker != nil && e.costTracker.OverBudget() {
		return reviewResult{}
	}
	snapshot := buildReviewSnapshot(msgs)
	if snapshot == "" {
		return reviewResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), reviewTimeout)
	defer cancel()
	resp, err := e.backgroundProvider().Chat(ctx, e.reviewRequest(snapshot))
	if err != nil {
		log.Warnf("background review failed: %v", err)
		return reviewResult{}
	}
	e.bgMu.Lock()
	// A review outlives the wait of /new and /resume; the conversation it
	// counted is gone by then and its count would throttle the next one.
	if e.conversationGen == gen {
		e.lastReviewMsgCount = len(msgs)
		e.turnsSinceReview = 0
	}
	e.bgMu.Unlock()
	return e.applyReview(resp.Content, sessionID)
}

const reviewPrompt = `你是一个对话回顾助手。分析以下对话片段，判断是否有值得记住的内容。

只在出现可复用的工作流程（解决特定问题的步骤）时输出，每个一行：
SKILL: <技能名> | <一句话描述何时使用> | <简要步骤>

用户偏好、项目事实由记忆提取负责，这里不要输出。

不要输出：
- 一次性的任务细节
- 已经很显然的事实
- 代码本身（太长）

如果没有值得记住的，只输出 NONE。`

// backgroundMaxTokens bounds the answer of a background bookkeeping request.
// Reasoning models (deepseek-v4-pro) spend part of max_tokens on thinking
// before they answer; the old 300 often left no room for the answer at all.
const backgroundMaxTokens = 4000

// reviewRequest builds the background review request for a snapshot. It runs
// on the background (fast) model, like extraction and consolidation; it used
// to run on the premium model for a job that only writes one-line notes.
func (e *Engine) reviewRequest(snapshot string) api.ChatRequest {
	e.bgMu.Lock()
	model := e.backgroundModel
	e.bgMu.Unlock()
	if model == "" {
		model = e.config.Model
	}
	return api.ChatRequest{
		Model:      model,
		SystemBase: reviewPrompt,
		Messages:   []api.Message{{Role: "user", Content: snapshot}},
		MaxTokens:  backgroundMaxTokens,
	}
}

// applyReview saves the SKILL lines of a review answer and returns what it
// saved. MEMORY lines are ignored: the per-turn memory extraction saves
// memories, and the review used to save the same facts a second time under
// other names.
//
// sessionID is what a saved skill names as its source_session. It is passed
// in rather than read from e.session: the review runs after the part of the
// background work WaitBackground covers, so /new or /resume could enter
// another session meanwhile. applyReview read e.SessionID() unsynchronized
// (a data race with enterSession) and could credit the skill to the new
// session.
func (e *Engine) applyReview(output, sessionID string) reviewResult {
	var res reviewResult
	output = strings.TrimSpace(output)
	if output == "NONE" || output == "" || e.skillMgr == nil {
		return res
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "SKILL:") {
			continue
		}
		parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "SKILL:")), "|")
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		desc, content := "", strings.TrimSpace(strings.Join(parts[1:], "|"))
		if len(parts) >= 3 {
			desc = strings.TrimSpace(parts[1])
			content = strings.TrimSpace(strings.Join(parts[2:], "|"))
		}
		if name == "" || content == "" {
			continue
		}
		if desc == "" {
			desc = textutil.ClipRunes(content, 80)
		}
		// A learned skill is shown to the model in later turns just like a
		// memory, so it gets the same injection screening.
		if err := memory.ScreenContent(name + "\n" + desc + "\n" + content); err != nil {
			log.Warnf("background review skill %q refused: %v", name, err)
			continue
		}
		// Never shadow a user, project, plugin or built-in skill.
		if existing, ok := e.skillMgr.Get(name); ok && !isAutoSkillPath(existing.FilePath) {
			log.Warnf("background review: skill %q already exists (%s), not replaced", name, existing.FilePath)
			continue
		}
		sk := skills.Skill{Name: name, Description: desc, Prompt: content}
		path, updated, err := writeAutoSkill(sk, sessionID, time.Now())
		if err != nil {
			log.Warnf("background review: saving skill %q: %v", name, err)
			continue
		}
		sk.FilePath, sk.Directory, sk.Source = path, filepath.Dir(path), skills.SourceUser
		e.skillMgr.Register(sk)
		if updated {
			res.updated = append(res.updated, name)
		} else {
			res.added = append(res.added, name)
		}
		log.Debugf("background review saved skill: %s", name)
		e.debugOutput(fmt.Sprintf("  \x1b[2mlearned skill: %s\x1b[0m\n", name))
	}
	return res
}

// autoSkillDirPrefix starts the directory name of every learned skill.
const autoSkillDirPrefix = "auto-"

// isAutoSkillPath reports whether path is a learned skill's SKILL.md.
func isAutoSkillPath(path string) bool {
	return path != "" && strings.HasPrefix(filepath.Base(filepath.Dir(path)), autoSkillDirPrefix)
}

// writeAutoSkill writes a learned skill to
// ~/.cove/skills/auto-<slug>-<hash>/SKILL.md, where the next session loads it
// like any user skill. updated reports that it replaced the file of the same
// learned skill. A file there that is not an auto-generated one for this
// name (the user edited it, or another name maps to it) is left alone and
// reported as an error.
func writeAutoSkill(sk skills.Skill, sessionID string, now time.Time) (path string, updated bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}
	dir := filepath.Join(home, ".cove", "skills", autoSkillDirPrefix+skillSlug(sk.Name))
	path = filepath.Join(dir, "SKILL.md")
	oneLine := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if data, rerr := os.ReadFile(path); rerr == nil {
		name, generated := autoSkillHeader(string(data))
		if !generated || name != oneLine(sk.Name) {
			return "", false, fmt.Errorf("%s exists and is not the auto-generated skill %q; left unchanged", path, sk.Name)
		}
		updated = true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("name: " + oneLine(sk.Name) + "\n")
	sb.WriteString("description: " + oneLine(sk.Description) + "\n")
	sb.WriteString("generated: " + now.Format(time.RFC3339) + "\n")
	sb.WriteString("source_session: " + oneLine(sessionID) + "\n")
	sb.WriteString("---\n")
	sb.WriteString(sk.Prompt + "\n")
	return path, updated, fsatomic.WriteFile(path, []byte(sb.String()), 0o644)
}

// autoSkillHeader reads the frontmatter of a SKILL.md: its name, and whether
// it carries the "generated:" line only writeAutoSkill writes.
func autoSkillHeader(content string) (name string, generated bool) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return "", false
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return "", false
	}
	for _, line := range strings.Split(content[4:4+end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "name":
			name = strings.TrimSpace(v)
		case "generated":
			generated = strings.TrimSpace(v) != ""
		}
	}
	return name, generated
}

// skillSlug is name as a directory name: lower-case ASCII letters and digits
// joined by dashes, then a short hash of the exact name, so two names that
// read the same as a slug ("K8s Deploy", "k8s-deploy") never share a file.
func skillSlug(name string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			dash = false
		} else if !dash && sb.Len() > 0 {
			sb.WriteByte('-')
			dash = true
		}
	}
	slug := strings.TrimRight(sb.String(), "-")
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "-")
	}
	sum := sha256.Sum256([]byte(name))
	if slug == "" {
		return hex.EncodeToString(sum[:3])
	}
	return slug + "-" + hex.EncodeToString(sum[:3])
}

func buildReviewSnapshot(msgs []api.Message) string {
	// Take the last 10 messages (or all if fewer)
	start := 0
	if len(msgs) > 10 {
		start = len(msgs) - 10
	}
	msgs = msgs[start:]

	var sb strings.Builder
	for _, m := range msgs {
		content := keepRunes(m.Content, 200)
		switch m.Role {
		case "user":
			sb.WriteString("用户: " + content + "\n")
		case "assistant":
			sb.WriteString("助手: " + content + "\n")
			for _, tc := range m.ToolCalls {
				if path := toolFilePath(tc); path != "" {
					sb.WriteString("  → " + tc.Name + "(" + path + ")\n")
				} else if cmd, ok := tc.Input["command"].(string); ok {
					sb.WriteString("  → bash(" + keepRunes(cmd, 80) + ")\n")
				}
			}
		case "tool":
			// Tool results are left out, as the memory extractor leaves them
			// out: they are what files, pages and commands said (secrets,
			// injected instructions), not what the user wants remembered.
		}
	}
	return sb.String()
}
