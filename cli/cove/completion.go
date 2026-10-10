package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/skills"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

type cmdEntry struct {
	Name     string
	Aliases  []string
	Desc     string
	Type     string
	Args     []string
	ArgHints map[string][]string
}

// buildCommandList is what completion offers: every registered command
// (with its argument hints) and every tool. The slash commands used to be a
// second hand-written table here.
func buildCommandList(cmdReg *command.Registry, toolReg *tool.Registry) []cmdEntry {
	var list []cmdEntry
	for _, c := range cmdReg.All() {
		e := cmdEntry{Name: "/" + c.Name(), Desc: c.Description(), Type: "cmd"}
		if commandCategory(c) != "" {
			e.Type = "builtin"
		}
		if h, ok := c.(command.ArgHinter); ok && len(h.ArgHints()) > 0 {
			e.ArgHints = map[string][]string{"": h.ArgHints()}
		}
		for _, a := range c.Aliases() {
			e.Aliases = append(e.Aliases, "/"+a)
		}
		list = append(list, e)
	}
	for _, t := range toolReg.All() {
		d := t.Def()
		args := toolArgNames(d.InputSchema)
		list = append(list, cmdEntry{Name: d.Name, Desc: d.Description, Type: "tool", Args: args})
		for _, alias := range d.Aliases {
			list = append(list, cmdEntry{Name: alias, Desc: d.Description, Type: "tool", Args: args})
		}
	}
	return list
}

func buildSkillDescs(mgr *skills.Manager) map[string]string {
	descs := make(map[string]string)
	if mgr == nil {
		return descs
	}
	for _, s := range mgr.All() {
		if s.Description != "" {
			descs[s.Name] = s.Description
		} else {
			descs[s.Name] = "技能"
		}
	}
	return descs
}

func complete(input string, commands []cmdEntry, skills map[string]string) []string {
	input = strings.TrimLeft(input, " \t")
	if input == "" {
		return nil
	}
	if suggestions, ok := completeAtPath(input); ok {
		return suggestions
	}
	if suggestions := completeArgs(input, commands); len(suggestions) > 0 {
		return suggestions
	}
	cmdNames := make(map[string]bool, len(commands))
	var matches []string
	lower := strings.ToLower(input)
	for _, c := range commands {
		cmdNames[strings.ToLower(c.Name)] = true
		for _, alias := range c.Aliases {
			cmdNames[strings.ToLower(alias)] = true
		}
		if strings.HasPrefix(input, "/") && c.Type == "tool" {
			continue
		}
		candidate := c.Name
		matched := strings.HasPrefix(strings.ToLower(candidate), lower)
		if !matched {
			for _, alias := range c.Aliases {
				if strings.HasPrefix(strings.ToLower(alias), lower) {
					candidate, matched = alias, true
					break
				}
			}
		}
		if !matched {
			continue
		}
		description := shortDesc(c.Desc)
		if len(c.Aliases) > 0 {
			label := "别名: " + strings.Join(c.Aliases, ", ")
			if candidate != c.Name {
				label = "同 " + c.Name
			}
			description = label + "  " + description
		}
		if description != "" {
			matches = append(matches, candidate+"\t"+description)
		} else {
			matches = append(matches, candidate)
		}
	}
	for name, desc := range skills {
		candidate := name
		if strings.HasPrefix(input, "/") {
			candidate = "/" + name
		}
		if cmdNames[strings.ToLower(candidate)] {
			continue
		}
		if strings.HasPrefix(strings.ToLower(candidate), lower) {
			if desc != "" {
				matches = append(matches, candidate+"\t"+shortDesc(desc))
			} else {
				matches = append(matches, candidate+"\t"+"技能")
			}
		}
	}
	sort.Strings(matches)
	return matches
}

// completeAtPathMax bounds the candidates of one @path completion.
const completeAtPathMax = 60

// completeAtPath completes an "@path" being typed as the last word (the
// attachment syntax) from the files under the working directory: the whole
// line with the path completed, directories ending in "/". ok is false when
// the last word is not an @path.
func completeAtPath(input string) (lines []string, ok bool) {
	start := strings.LastIndexAny(input, " \t") + 1
	word := input[start:]
	if !strings.HasPrefix(word, "@") {
		return nil, false
	}
	typed := strings.ReplaceAll(word[1:], "\\", "/")
	dir, base := "", typed
	if i := strings.LastIndexByte(typed, '/'); i >= 0 {
		dir, base = typed[:i+1], typed[i+1:]
	}
	cwd, _ := os.Getwd()
	entries, err := os.ReadDir(filepath.Join(cwd, filepath.FromSlash(dir)))
	if err != nil {
		return nil, true
	}
	lower := strings.ToLower(base)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(name), lower) {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		lines = append(lines, input[:start]+"@"+dir+name)
		if len(lines) == completeAtPathMax {
			break
		}
	}
	sort.Strings(lines)
	return lines, true
}

func completeArgs(input string, commands []cmdEntry) []string {
	head, rest, ok := strings.Cut(input, " ")
	if !ok {
		return nil
	}
	entry, found := findCompletionEntry(head, commands)
	if !found {
		return nil
	}
	rest = strings.TrimLeft(rest, " \t")
	base := head + " "
	if hints := entry.ArgHints[""]; len(hints) > 0 {
		return completeValueHints(base, rest, hints)
	}
	if len(entry.Args) == 0 {
		return nil
	}
	used := usedArgNames(rest)
	current := currentArgPrefix(rest)
	var matches []string
	for _, arg := range entry.Args {
		if used[arg] {
			continue
		}
		if current == "" || strings.HasPrefix(strings.ToLower(arg), strings.ToLower(current)) {
			matches = append(matches, base+replaceCurrentArgPrefix(rest, current, arg+"="))
		}
	}
	return matches
}

func completeValueHints(base, rest string, hints []string) []string {
	current := strings.TrimSpace(rest)
	var matches []string
	for _, hint := range hints {
		if current == "" || strings.HasPrefix(strings.ToLower(hint), strings.ToLower(current)) {
			matches = append(matches, base+hint)
		}
	}
	return matches
}

func findCompletionEntry(name string, commands []cmdEntry) (cmdEntry, bool) {
	for _, c := range commands {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
		for _, alias := range c.Aliases {
			if strings.EqualFold(alias, name) {
				return c, true
			}
		}
	}
	return cmdEntry{}, false
}

func usedArgNames(input string) map[string]bool {
	used := map[string]bool{}
	for _, part := range strings.Fields(input) {
		part = strings.Trim(part, ` "'{},`)
		if key, _, ok := strings.Cut(part, "="); ok {
			used[strings.Trim(key, ` "'`)] = true
			continue
		}
		if key, _, ok := strings.Cut(part, ":"); ok {
			used[strings.Trim(key, ` "'`)] = true
		}
	}
	return used
}

func currentArgPrefix(input string) string {
	input = strings.TrimRight(input, " \t")
	if input == "" {
		return ""
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	if strings.ContainsAny(last, "=:") {
		return ""
	}
	return strings.Trim(last, ` "'{},`)
}

func replaceCurrentArgPrefix(input, current, replacement string) string {
	if current == "" {
		if strings.TrimSpace(input) == "" {
			return replacement
		}
		return strings.TrimRight(input, " \t") + " " + replacement
	}
	idx := strings.LastIndex(input, current)
	if idx < 0 {
		return strings.TrimRight(input, " \t") + " " + replacement
	}
	return input[:idx] + replacement
}

func toolArgNames(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	if len(schema.Properties) == 0 {
		return nil
	}
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if required[names[i]] != required[names[j]] {
			return required[names[i]]
		}
		return names[i] < names[j]
	})
	return names
}

func providerNameSuggestions() []string {
	return []string{
		"anthropic", "deepseek", "openai", "openai-compatible", "glm", "kimi", "qwen", "doubao",
		"openrouter", "siliconflow", "groq", "together", "fireworks", "xai", "mistral",
	}
}

func showQuickCommands(commands []cmdEntry) {
	outln("\n可用命令:")
	for _, c := range commands {
		if c.Type == "cmd" || c.Type == "config" || c.Type == "builtin" {
			outf("  %-16s %s\n", c.Name, c.Desc)
		}
	}
	outln()
}

func handleUnknownCmd(input string, cmdReg *command.Registry) bool {
	parts := strings.Fields(input)
	name := strings.TrimPrefix(parts[0], "/")
	_, ok := cmdReg.Find(name)
	if ok {
		return false
	}
	suggestions := fuzzyMatch(name, cmdReg)
	if len(suggestions) > 0 {
		outf("未知命令: /%s\n你是不是想输入?\n", name)
		for _, s := range suggestions {
			outf("  /%s\n", s)
		}
		return true
	}
	outf("未知命令: /%s。输入 /help 查看可用命令。\n", name)
	return true
}

func fuzzyMatch(input string, cmdReg *command.Registry) []string {
	var matches []string
	lower := strings.ToLower(input)
	for _, c := range cmdReg.All() {
		name := c.Name()
		if strings.Contains(strings.ToLower(name), lower) {
			matches = append(matches, name)
		}
	}
	if len(matches) > 5 {
		return matches[:5]
	}
	return matches
}
