package checkpoint

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

type restoreFile struct {
	path       string
	exists     bool
	beforeHash [32]byte
	beforeMode os.FileMode
	after      []byte
	afterMode  os.FileMode
	delete     bool
}

// FileRestorePlan is an immutable, manager-bound preview for explicit approval.
type FileRestorePlan struct {
	id      string
	target  string
	root    string
	manager *Manager
	created time.Time
	files   []restoreFile
	used    bool
}

// ID is the token required to approve exactly this preview.
func (plan *FileRestorePlan) ID() string { return plan.id }

// Summary lists only the files this approval would restore or remove.
func (plan *FileRestorePlan) Summary() string {
	var text strings.Builder
	fmt.Fprintf(&text, "选择性回滚预览 %s\n目标检查点: %s\n", plan.id, plan.target)
	for _, file := range plan.files {
		action := "恢复"
		if file.delete {
			action = "删除（目标检查点中不存在）"
		}
		fmt.Fprintf(&text, "  %s: %s\n", file.path, action)
	}
	fmt.Fprintf(&text, "确认: /undo apply %s\n预览后文件若改变则拒绝回滚；预览 10 分钟内有效。", plan.id)
	return text.String()
}

func (m *Manager) selectiveTarget(hash string) (string, error) {
	if hash == "" || strings.HasPrefix(hash, "-") {
		return "", fmt.Errorf("选择性回滚需要明确的检查点 ID")
	}
	full, err := m.gitOutput(m.env(), "rev-parse", "--verify", "--quiet", hash+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("检查点不存在: %s: %w", hash, err)
	}
	target := strings.TrimSpace(full)
	if !m.isAncestor(target, m.refName) && !m.isAncestor(target, m.refName+"-undo") {
		return "", fmt.Errorf("检查点不属于当前项目: %s", hash)
	}
	return target, nil
}

func regularRestorePath(root, name string) (string, string, error) {
	if name == "" || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", "", fmt.Errorf("需要项目内的相对文件路径: %s", name)
	}
	rel := filepath.Clean(filepath.FromSlash(name))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("文件路径越界: %s", name)
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	current := root
	for _, part := range parts {
		if strings.EqualFold(part, ".git") || strings.Contains(part, ":") {
			return "", "", fmt.Errorf("不允许恢复该路径: %s", name)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf("不允许经符号链接回滚: %s", name)
		}
	}
	return filepath.ToSlash(rel), filepath.Join(root, rel), nil
}

func (m *Manager) treeFile(commit, path string) ([]byte, os.FileMode, bool, error) {
	out, err := m.gitOutput(m.env(), "ls-tree", "-z", commit, "--", ":(literal)"+path)
	if err != nil {
		return nil, 0, false, err
	}
	if out == "" {
		return nil, 0, false, nil
	}
	meta, name, ok := strings.Cut(strings.TrimSuffix(out, "\x00"), "\t")
	fields := strings.Fields(meta)
	if !ok || name != path || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return nil, 0, false, fmt.Errorf("只支持普通文件，不支持目录、符号链接或嵌套仓库: %s", path)
	}
	data, err := m.gitOutput(m.env(), "cat-file", "blob", fields[2])
	if err != nil {
		return nil, 0, false, err
	}
	mode := os.FileMode(0644)
	if fields[0] == "100755" {
		mode = 0755
	}
	return []byte(data), mode, true, nil
}

func restoreFingerprint(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("只支持普通文件: %s", path)
	}
	data, err := os.ReadFile(path)
	return data, info.Mode().Perm(), true, err
}

// ReadFiles reads regular files from a checkpoint belonging to this project,
// without restoring files or changing the working tree.
func (m *Manager) ReadFiles(hash string, paths []string) (string, map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, err := m.selectiveTarget(hash)
	if err != nil {
		return "", nil, err
	}
	files := make(map[string][]byte)
	for _, path := range paths {
		rel, _, err := regularRestorePath(m.workDir, path)
		if err != nil {
			return "", nil, err
		}
		data, _, exists, err := m.treeFile(target, rel)
		if err != nil {
			return "", nil, err
		}
		if !exists {
			return "", nil, fmt.Errorf("checkpoint does not contain %s", rel)
		}
		files[rel] = data
	}
	return target, files, nil
}

// PreviewFiles captures selected files without modifying the working tree.
func (m *Manager) PreviewFiles(hash string, paths []string) (*FileRestorePlan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(paths) == 0 {
		return nil, fmt.Errorf("至少选择一个文件")
	}
	target, err := m.selectiveTarget(hash)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(m.workDir)
	if err != nil {
		return nil, err
	}
	backup, _, err := m.snapshot(m.refName+"-undo", "before selective preview")
	if err != nil {
		return nil, fmt.Errorf("预览前备份失败: %w", err)
	}
	plan := &FileRestorePlan{id: rand.Text(), target: target, root: root, manager: m, created: time.Now()}
	seen := map[string]bool{}
	for _, name := range paths {
		rel, full, err := regularRestorePath(root, name)
		if err != nil {
			return nil, err
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		before, mode, exists, err := restoreFingerprint(full)
		if err != nil {
			return nil, err
		}
		captured, _, capturedExists, err := m.treeFile(backup, rel)
		if err != nil {
			return nil, err
		}
		if exists != capturedExists || !bytes.Equal(before, captured) {
			return nil, fmt.Errorf("文件在备份后改变，或未纳入可恢复备份: %s", rel)
		}
		after, afterMode, targetExists, err := m.treeFile(target, rel)
		if err != nil {
			return nil, err
		}
		if !exists && !targetExists {
			return nil, fmt.Errorf("文件既不存在，也不在检查点内: %s", rel)
		}
		if exists {
			executable := afterMode&0111 != 0
			afterMode = mode &^ 0111
			if executable {
				afterMode |= 0100
			}
		} else {
			afterMode = 0600 | afterMode&0100
		}
		plan.files = append(plan.files, restoreFile{path: rel, exists: exists, beforeHash: sha256.Sum256(before), beforeMode: mode, after: after, afterMode: afterMode, delete: !targetExists})
	}
	return plan, nil
}

// ApplyFiles restores an approved preview once, rejecting drift before writes.
// A backup is returned on partial failure so the operation remains reversible.
func (m *Manager) ApplyFiles(plan *FileRestorePlan, token string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if plan == nil || plan.manager != m || plan.id != token || plan.used || time.Since(plan.created) > 10*time.Minute {
		return "", fmt.Errorf("回滚预览无效、已使用或过期，请重新预览")
	}
	root, err := filepath.EvalSymlinks(m.workDir)
	if err != nil || root != plan.root {
		return "", fmt.Errorf("工作目录已改变，请重新预览")
	}
	validate := func() error {
		for _, file := range plan.files {
			_, full, err := regularRestorePath(root, file.path)
			if err != nil {
				return err
			}
			data, mode, exists, err := restoreFingerprint(full)
			if err != nil {
				return err
			}
			if exists != file.exists || mode != file.beforeMode || sha256.Sum256(data) != file.beforeHash {
				return fmt.Errorf("预览后文件已改变，拒绝覆盖: %s", file.path)
			}
		}
		return nil
	}
	if err := validate(); err != nil {
		return "", err
	}
	backup, _, err := m.snapshot(m.refName+"-undo", "before selective undo")
	if err != nil {
		return "", fmt.Errorf("回滚前备份失败: %w", err)
	}
	if err := validate(); err != nil {
		return "", err
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer func() { _ = confined.Close() }()
	plan.used = true
	for _, file := range plan.files {
		_, full, err := regularRestorePath(root, file.path)
		if err != nil {
			return backup, err
		}
		data, mode, exists, err := restoreFingerprint(full)
		if err != nil || exists != file.exists || mode != file.beforeMode || sha256.Sum256(data) != file.beforeHash {
			return backup, fmt.Errorf("执行期间文件已改变，停止回滚: %s", file.path)
		}
		if file.delete {
			err = confined.Remove(file.path)
		} else {
			if err = confined.MkdirAll(filepath.Dir(file.path), 0755); err == nil {
				err = fsatomic.WriteFileRoot(confined, file.path, file.after, file.afterMode)
			}
		}
		if err != nil {
			return backup, fmt.Errorf("回滚 %s 失败: %w", file.path, err)
		}
	}
	return backup, nil
}
