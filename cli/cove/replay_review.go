package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/render"
)

type filePreviewTaskReview struct {
	Version    int                        `json:"version"`
	Task       filePreviewTask            `json:"task"`
	Plan       *engine.ImplementationPlan `json:"plan,omitempty"`
	Acceptance *engine.AcceptanceReport   `json:"acceptance,omitempty"`
}

func saveFilePreviewTaskReview(dir string, review filePreviewTaskReview) error {
	if !validFilePreviewID(review.Task.ID) {
		return fmt.Errorf("无效的任务编号")
	}
	review.Version = 1
	data, err := json.Marshal(review)
	if err != nil {
		return err
	}
	root := filepath.Join(dir, "tasks")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	return fsatomic.WriteFile(filepath.Join(root, review.Task.ID+".json"), data, 0600)
}

func loadFilePreviewTaskReview(dir, taskID string) (filePreviewTaskReview, error) {
	var review filePreviewTaskReview
	if !validFilePreviewID(taskID) {
		return review, fmt.Errorf("缺少任务级记录")
	}
	path := filepath.Join(dir, "tasks", taskID+".json")
	info, err := os.Stat(path)
	if err != nil {
		return review, err
	}
	if info.Size() > 4<<20 {
		return review, fmt.Errorf("任务记录过大")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return review, err
	}
	if err := json.Unmarshal(data, &review); err != nil {
		return review, err
	}
	if review.Version != 1 || review.Task.ID != taskID {
		return review, fmt.Errorf("任务记录不匹配")
	}
	return review, nil
}

func filePreviewOverview(dir string, groups []filePreviewGroup, selected []string) string {
	chosen := make(map[string]bool, len(selected))
	for _, id := range selected {
		chosen[id] = true
	}
	var output strings.Builder
	for _, group := range groups {
		var records []filePreviewRecord
		for _, id := range group.IDs {
			if !chosen[id] {
				continue
			}
			if record, err := loadFilePreview(dir, id); err == nil {
				records = append(records, record)
			}
		}
		if len(records) == 0 {
			continue
		}
		fmt.Fprintf(&output, "任务审阅: %s\n已选 %d/%d 次改动\n", group.Title, len(records), len(group.IDs))
		type fileCounts struct{ changes, added, removed int }
		counts := make(map[string]fileCounts)
		var paths []string
		for _, record := range records {
			path := record.Block.Header
			count, exists := counts[path]
			if !exists {
				paths = append(paths, path)
			}
			count.changes++
			for _, line := range strings.Split(record.Block.Full, "\n") {
				if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
					count.added++
				} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
					count.removed++
				}
			}
			counts[path] = count
		}
		for _, path := range paths {
			count := counts[path]
			fmt.Fprintf(&output, "  %s · %d 次改动 · 累计 +%d −%d\n", path, count.changes, count.added, count.removed)
		}
		review, err := loadFilePreviewTaskReview(dir, records[0].Task.ID)
		if err != nil {
			output.WriteString("验收: 未记录任务级证据，不能据此声称通过。\n")
			continue
		}
		if review.Plan != nil {
			decision := map[string]string{"approved": "已批准", "rejected": "已拒绝", "pending": "待确认"}[review.Plan.Decision]
			if decision == "" {
				decision = "未记录决定"
			}
			fmt.Fprintf(&output, "方案（%s）: %s\n计划涉及文件: %s\n关键决策: %s\n计划验收标准（不等于已通过）: %s\n", decision, review.Plan.Summary, strings.Join(review.Plan.Files, ", "), strings.Join(review.Plan.Decisions, "; "), strings.Join(review.Plan.Checks, "; "))
		}
		if review.Acceptance == nil {
			output.WriteString("验收: 未记录任务级证据，不能据此声称通过。\n")
		} else {
			output.WriteString("历史任务验收证据（不是当前工作区重新验证，也不证明每行改动正确）:\n")
			output.WriteString(review.Acceptance.Summary())
			output.WriteByte('\n')
		}
	}
	return render.StripControls(output.String())
}
