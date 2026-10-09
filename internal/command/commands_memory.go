package command

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/filelock"
	"github.com/liuzhixin405/cove-agent/internal/memory"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

func (c *MemoryCmd) Name() string        { return "memory" }
func (c *MemoryCmd) Aliases() []string   { return nil }
func (c *MemoryCmd) Description() string { return "管理持久化记忆" }
func (c *MemoryCmd) Help() string {
	return "/memory [list|add|remove|search <关键词>|source <名称>|stats] - 管理持久记忆文件与来源证据"
}
func (c *MemoryCmd) ArgHints() []string {
	return []string{"list", "add", "remove", "search", "source", "stats"}
}
func (c *MemoryCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.MemoryStore == nil {
		return Output{Message: "记忆存储不可用"}, nil
	}
	if len(in.Args) == 0 || in.Args[0] == "list" {
		entries := in.MemoryStore.All()
		if len(entries) == 0 {
			return Output{Message: "暂无记忆文件"}, nil
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		var sb strings.Builder
		fmt.Fprintf(&sb, "记忆文件 (共 %d 条):\n", len(entries))
		for _, e := range entries {
			marker := sourceMarker(e)
			fmt.Fprintf(&sb, "- %s%s [%s]: %s\n", e.Name, marker, humanBytes(len(e.Content)), entrySummary(e.Content))
		}
		return Output{Message: sb.String()}, nil
	}
	switch in.Args[0] {
	case "source":
		if len(in.Args) != 2 {
			return Output{Message: "用法: /memory source <名称>"}, nil
		}
		store, ok := in.MemoryStore.(interface {
			Provenance(string) (*memory.Provenance, error)
		})
		if !ok {
			return Output{Message: "此记忆存储不支持溯源"}, nil
		}
		record, err := store.Provenance(in.Args[1])
		if err != nil {
			return Output{}, err
		}
		if record == nil {
			return Output{Message: "来源未知（旧记忆或正文被外部修改）；不会推断来源会话。"}, nil
		}
		var text strings.Builder
		fmt.Fprintf(&text, "记忆来源: %s\n正文 SHA256: %s\n", in.Args[1], record.ContentHash)
		for index, source := range record.Sources {
			when := "时间未知"
			if !source.At.IsZero() {
				when = source.At.Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(&text, "%d. %s  %s\n", index+1, source.Kind, when)
			if len(source.SessionIDs) > 0 {
				fmt.Fprintf(&text, "   来源会话: %s\n", strings.Join(source.SessionIDs, ", "))
			}
			if source.Cwd != "" {
				fmt.Fprintf(&text, "   项目: %s\n", source.Cwd)
			}
			if source.FirstMessage > 0 {
				fmt.Fprintf(&text, "   消息位置: %d-%d；输入 SHA256: %s\n", source.FirstMessage, source.LastMessage, source.TranscriptHash)
			}
			if source.Evidence != "" {
				fmt.Fprintf(&text, "   依据片段:\n%s\n", source.Evidence)
			}
		}
		text.WriteString("来源记录仅说明写入依据，不构成事实真实性验证。")
		return Output{Message: text.String()}, nil
	case "add":
		if len(in.Args) < 3 {
			return Output{Message: "用法: /memory add <名称> <内容>"}, nil
		}
		name := in.Args[1]
		content := strings.Join(in.Args[2:], " ")
		source := memory.ProvenanceSource{Kind: "manual", Cwd: in.Cwd}
		if eng, ok := in.Engine.(interface{ SessionID() string }); ok {
			source.SessionIDs = []string{eng.SessionID()}
		}
		writer, sourced := in.MemoryStore.(interface {
			SaveWithSource(string, string, memory.ProvenanceSource) error
			AppendWithSource(string, string, memory.ProvenanceSource) (string, error)
		})
		// The write (and the probe it builds on) holds both memory write
		// locks like every other writer: without them a turn-end extraction
		// mid read -> append -> rename in this or another cove process
		// replaced what was just saved, or lost the fact it had appended.
		var msg string
		err := withMemoryWriteLock(in.MemoryStore, func() error {
			// A name only the global directory has: build on its content, or
			// the new project copy would hide what the global one said.
			if ap, ok := in.MemoryStore.(memoryAppender); ok {
				if _, fromLower, exists := ap.BaseContent(name); exists && fromLower {
					var written string
					var err error
					if sourced {
						written, err = writer.AppendWithSource(name, content, source)
					} else {
						written, err = ap.Append(name, content)
					}
					if err != nil {
						return err
					}
					msg = fmt.Sprintf("记忆 '%s' 已保存（在全局同名记忆基础上追加到项目目录）", written)
					return nil
				}
			}
			var err error
			if sourced {
				err = writer.SaveWithSource(name, content, source)
			} else {
				err = in.MemoryStore.Save(name, content)
			}
			if err != nil {
				return err
			}
			msg = fmt.Sprintf("记忆 '%s' 已保存", name)
			return nil
		})
		if errors.Is(err, filelock.ErrTimeout) {
			return Output{Message: "记忆目录正被另一个 cove 进程写入，请稍后重试"}, nil
		}
		if err != nil {
			return Output{}, err
		}
		return Output{Message: msg}, nil
	case "remove", "delete", "rm":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /memory remove <名称>"}, nil
		}
		// Same locks as add: a background extraction in its read -> append
		// -> rename window would otherwise write the "deleted" file back.
		err := withMemoryWriteLock(in.MemoryStore, func() error { return in.MemoryStore.Delete(in.Args[1]) })
		if errors.Is(err, filelock.ErrTimeout) {
			return Output{Message: "记忆目录正被另一个 cove 进程写入，请稍后重试"}, nil
		}
		if err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("记忆 '%s' 已删除", in.Args[1])}, nil
	case "search", "find":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /memory search <关键词>"}, nil
		}
		query := strings.Join(in.Args[1:], " ")
		results := in.MemoryStore.Search(query, 5)
		if len(results) == 0 {
			return Output{Message: fmt.Sprintf("未找到与 %q 相关的记忆", query)}, nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "与 %q 相关的记忆 (BM25 关键词检索):\n", query)
		for _, r := range results {
			marker := sourceMarker(r.Entry)
			preview := strings.ReplaceAll(r.Entry.Content, "\n", " ")
			if len(preview) > 80 {
				preview = textutil.ClipRunes(preview, 83)
			}
			fmt.Fprintf(&sb, "  %s%s [%.2f]: %s\n", r.Entry.Name, marker, r.Score, preview)
		}
		return Output{Message: sb.String()}, nil
	case "stats", "stat":
		st := in.MemoryStore.Stats()
		var sb strings.Builder
		sb.WriteString("记忆统计:\n")
		fmt.Fprintf(&sb, "  文件数:   %d (其中指令文件 %d)\n", st.FileCount, st.ProjectCount)
		fmt.Fprintf(&sb, "  总行数:   %d\n", st.TotalLines)
		fmt.Fprintf(&sb, "  总大小:   %s / %s\n", humanBytes(st.TotalBytes), humanBytes(st.MaxTotalBytes))
		if st.MaxTotalBytes > 0 {
			fmt.Fprintf(&sb, "  使用率:   %.1f%%\n", float64(st.TotalBytes)*100/float64(st.MaxTotalBytes))
		}
		fmt.Fprintf(&sb, "  单条上限: %s\n", humanBytes(st.MaxEntryBytes))
		if st.LastExtractedAt.IsZero() {
			sb.WriteString("  上次提取: 尚无记录\n")
		} else {
			fmt.Fprintf(&sb, "  上次提取: %s，保存 %d 条\n", st.LastExtractedAt.Format("2006-01-02 15:04"), st.LastExtractedCount)
		}
		return Output{Message: sb.String()}, nil
	default:
		return Output{Message: c.Help()}, nil
	}
}

// memoryAppender is the part of *memory.Store /memory add uses for a name
// that only a lower-priority (global) directory has.
type memoryAppender interface {
	BaseContent(name string) (content string, fromLower, ok bool)
	Append(name, content string) (string, error)
}

// memoryDirer is the part of *memory.Store that names the directory its
// writes go to, whose lock file guards them.
type memoryDirer interface {
	PrimaryDir() string
}

// withMemoryWriteLock runs fn under the memory write locks of store's write
// directory (memory.WithWriteLock). A store without a directory (a test
// double) has no lock file to take; fn then runs under the in-process lock
// alone.
func withMemoryWriteLock(store MemoryStore, fn func() error) error {
	if d, ok := store.(memoryDirer); ok && d.PrimaryDir() != "" {
		return memory.WithWriteLock(d.PrimaryDir(), fn)
	}
	unlock := memory.LockWrites()
	defer unlock()
	return fn()
}

// sourceMarker labels where a memory entry was loaded from.
func sourceMarker(e memory.Entry) string {
	switch {
	case e.Project || e.Source == memory.SourceInstructions:
		return " (指令文件)"
	case e.Source == memory.SourceProject:
		return " (项目)"
	case e.Source == memory.SourceGlobal:
		return " (全局)"
	}
	return ""
}

// entrySummary is the first non-blank line of a memory, clipped for a list.
func entrySummary(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return textutil.ClipRunes(line, 60)
		}
	}
	return "(空)"
}
