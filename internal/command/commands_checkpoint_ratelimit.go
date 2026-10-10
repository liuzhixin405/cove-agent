package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/checkpoint"
)

type checkpointEngine interface {
	ListCheckpoints() []string
	RestoreCheckpoint(commitHash string) (backup string, err error)
}

type selectiveCheckpointEngine interface {
	PreviewCheckpointFiles(string, []string) (*checkpoint.FileRestorePlan, error)
	ApplyCheckpointFiles(*checkpoint.FileRestorePlan, string) (string, error)
}

func checkpointScope(eng EngineView) string {
	if scoped, ok := eng.(interface {
		PermissionScope() string
		SessionID() string
	}); ok {
		return scoped.PermissionScope() + "\x00" + scoped.SessionID()
	}
	return ""
}

type rateLimitEngine interface {
	RateLimitInfo() api.RateLimitInfo
}

func (c *UndoCmd) Name() string { return "undo" }

// MutatesEngine: /undo rewrites the working tree, so running it while a task
// is still editing files interleaved the rollback with the agent's writes and
// left a mix of both. It used to be accepted mid-task.
func (c *UndoCmd) MutatesEngine([]string) bool { return true }
func (c *UndoCmd) Aliases() []string           { return nil }
func (c *UndoCmd) Description() string         { return "回退到检查点" }
func (c *UndoCmd) Help() string {
	return "/undo [commit] - 整树回退并备份；/undo files <commit> <文件>... 预览文件级回滚；/undo apply <预览ID> 确认；/undo cancel 放弃预览"
}
func (c *UndoCmd) ArgHints() []string { return []string{"files", "apply", "cancel"} }

// PendingPreviewID is the token of the file-level preview waiting for
// approval ("" when there is none); the front end confirms it in one key.
func (c *UndoCmd) PendingPreviewID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.preview == nil {
		return ""
	}
	return c.preview.ID()
}
func (c *UndoCmd) Execute(ctx context.Context, in Input) (Output, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(in.Args) > 0 {
		switch in.Args[0] {
		case "files":
			c.preview = nil
			if len(in.Args) < 3 {
				return Output{Message: c.Help()}, nil
			}
			eng, ok := in.Engine.(selectiveCheckpointEngine)
			if !ok {
				return Output{Message: "文件级回滚不可用"}, nil
			}
			plan, err := eng.PreviewCheckpointFiles(in.Args[1], in.Args[2:])
			if err != nil {
				return Output{Message: "预览失败: " + err.Error()}, nil
			}
			c.preview, c.previewScope = plan, checkpointScope(in.Engine)
			return Output{Message: plan.Summary()}, nil
		case "apply":
			if len(in.Args) != 2 || c.preview == nil || c.previewScope != checkpointScope(in.Engine) {
				c.preview = nil
				return Output{Message: "当前项目/会话没有有效预览，请先 /undo files <commit> <文件>..."}, nil
			}
			eng, ok := in.Engine.(selectiveCheckpointEngine)
			if !ok {
				return Output{Message: "文件级回滚不可用"}, nil
			}
			backup, err := eng.ApplyCheckpointFiles(c.preview, in.Args[1])
			c.preview = nil
			message := "已按预览回滚选中文件，未选中文件保持不变。"
			if err != nil {
				message = "文件级回滚失败: " + err.Error()
			}
			if backup != "" {
				message += "\n回滚前已备份；撤销此次回滚: /undo " + backup
			}
			return Output{Message: message}, nil
		case "cancel":
			if len(in.Args) != 1 {
				return Output{Message: c.Help()}, nil
			}
			c.preview = nil
			return Output{Message: "已放弃文件级回滚预览。"}, nil
		}
	}
	c.preview = nil
	eng, ok := in.Engine.(checkpointEngine)
	if !ok || eng == nil {
		return Output{Message: "检查点功能不可用"}, nil
	}
	hash := ""
	if len(in.Args) > 0 {
		hash = strings.TrimSpace(in.Args[0])
	}
	backup, err := eng.RestoreCheckpoint(hash)
	var msg string
	switch {
	case err != nil:
		msg = fmt.Sprintf("回退失败: %v", err)
	case hash != "":
		msg = fmt.Sprintf("已回退到检查点 %s", hash)
	default:
		msg = "已回退到最近检查点"
	}
	// A failed restore may already have rewritten part of the tree; the backup
	// is then the only way back, and it used to be dropped with the error.
	if len(backup) >= 8 {
		msg += fmt.Sprintf("\n回退前的状态已备份，撤销这次回退: /undo %s", backup[:8])
	}
	return Output{Message: msg}, nil
}

func (c *CheckpointsCmd) Name() string        { return "checkpoints" }
func (c *CheckpointsCmd) Aliases() []string   { return nil }
func (c *CheckpointsCmd) Description() string { return "列出可用检查点" }
func (c *CheckpointsCmd) Help() string        { return "/checkpoints - 列出最近检查点" }
func (c *CheckpointsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	eng, ok := in.Engine.(checkpointEngine)
	if !ok || eng == nil {
		return Output{Message: "检查点功能不可用"}, nil
	}
	items := eng.ListCheckpoints()
	if len(items) == 0 {
		return Output{Message: "暂无检查点"}, nil
	}
	var sb strings.Builder
	sb.WriteString("最近检查点:\n")
	for i, it := range items {
		fmt.Fprintf(&sb, "%2d. %s\n", i+1, it)
	}
	return Output{Message: strings.TrimRight(sb.String(), "\n")}, nil
}

func (c *RateLimitCmd) Name() string        { return "ratelimit" }
func (c *RateLimitCmd) Aliases() []string   { return nil }
func (c *RateLimitCmd) Description() string { return "查看 API 速率限制状态" }
func (c *RateLimitCmd) Help() string {
	return "/ratelimit - 查看最近一次请求的速率限制信息"
}
func (c *RateLimitCmd) Execute(ctx context.Context, in Input) (Output, error) {
	eng, ok := in.Engine.(rateLimitEngine)
	if !ok || eng == nil {
		return Output{Message: "速率限制信息不可用"}, nil
	}
	info := eng.RateLimitInfo()
	if !info.HasData() {
		return Output{Message: "暂无速率限制数据（尚未收到相关响应头）"}, nil
	}
	// The reset durations were relative to the response that carried them;
	// shown later they must count down (or say the window has reset).
	elapsed := time.Duration(0)
	if !info.UpdatedAt.IsZero() {
		elapsed = time.Since(info.UpdatedAt)
	}
	resetNote := func(reset time.Duration) string {
		if reset <= 0 {
			return ""
		}
		if remaining := reset - elapsed; remaining > 0 {
			return fmt.Sprintf(" (reset in %s)", roundDuration(remaining))
		}
		return " (已重置)"
	}
	var sb strings.Builder
	sb.WriteString("=== Rate Limit ===\n")
	if info.RequestsLimit > 0 {
		fmt.Fprintf(&sb, "Requests: %d / %d%s\n", info.RequestsRemaining, info.RequestsLimit, resetNote(info.RequestsReset))
	}
	if info.TokensLimit > 0 {
		fmt.Fprintf(&sb, "Tokens: %d / %d%s\n", info.TokensRemaining, info.TokensLimit, resetNote(info.TokensReset))
	}
	if !info.UpdatedAt.IsZero() {
		fmt.Fprintf(&sb, "Updated: %s", info.UpdatedAt.Format(time.RFC3339))
	}
	return Output{Message: strings.TrimRight(sb.String(), "\n")}, nil
}

func roundDuration(d time.Duration) time.Duration {
	if d < time.Second {
		return d
	}
	return d.Round(time.Second)
}
