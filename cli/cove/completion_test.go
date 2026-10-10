package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// names strips the "\tdescription" suffix that complete() appends, so tests can
// assert on the completion values themselves.
func names(suggestions []string) []string {
	out := make([]string, 0, len(suggestions))
	for _, s := range suggestions {
		name, _, _ := strings.Cut(s, "\t")
		out = append(out, name)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

var testCommands = []cmdEntry{
	{Name: "/help", Desc: "显示帮助", Type: "command"},
	{Name: "/history", Desc: "查看历史", Type: "command"},
	{Name: "/model", Desc: "切换模型", Type: "command",
		ArgHints: map[string][]string{"": {"gpt-4o", "claude-sonnet-5", "deepseek-chat"}}},
	{Name: "read", Desc: "Read a file", Type: "tool", Args: []string{"filePath", "offset", "limit"}},
	{Name: "edit", Desc: "Edit a file", Type: "tool", Args: []string{"filePath", "oldString", "newString"}},
}

// ---------- complete ----------

func TestCompletePrefixMatching(t *testing.T) {
	got := names(complete("/h", testCommands, nil))
	if !contains(got, "/help") || !contains(got, "/history") {
		t.Fatalf("complete(\"/h\") = %v, want both /help and /history", got)
	}
	if contains(got, "/model") {
		t.Fatalf("complete(\"/h\") returned a non-matching command: %v", got)
	}
}

func TestCompleteIsCaseInsensitive(t *testing.T) {
	got := names(complete("/HIST", testCommands, nil))
	if !contains(got, "/history") {
		t.Fatalf("complete(\"/HIST\") = %v, want /history", got)
	}
}

// TestCompleteExcludesToolsForSlashInput pins the rule that makes the picker
// usable: typing "/" is asking for commands, so raw tool names must not be
// offered there.
func TestCompleteExcludesToolsForSlashInput(t *testing.T) {
	got := names(complete("/", testCommands, nil))
	for _, name := range got {
		if name == "read" || name == "edit" {
			t.Fatalf("complete(\"/\") offered the tool %q: %v", name, got)
		}
	}
	if !contains(got, "/help") {
		t.Fatalf("complete(\"/\") = %v, want the commands", got)
	}

	// Without the slash, tools ARE candidates.
	got = names(complete("re", testCommands, nil))
	if !contains(got, "read") {
		t.Fatalf("complete(\"re\") = %v, want the read tool", got)
	}
}

func TestCompleteGroupsRegisteredAliases(t *testing.T) {
	reg := (&frontend{}).install(registerAllCommands())
	commands := buildCommandList(reg, tool.NewRegistry())
	suggestions := complete("/", commands, nil)
	values := names(suggestions)
	if len(values) != len(reg.All()) {
		t.Fatalf("got %d candidates for %d commands", len(values), len(reg.All()))
	}
	for _, command := range reg.All() {
		if !contains(values, "/"+command.Name()) {
			t.Errorf("missing canonical command /%s", command.Name())
		}
		for _, alias := range command.Aliases() {
			if contains(values, "/"+alias) {
				t.Errorf("alias /%s offered separately", alias)
			}
			entry, ok := findCompletionEntry("/"+alias, commands)
			if !ok || entry.Name != "/"+command.Name() {
				t.Errorf("alias /%s does not resolve to /%s", alias, command.Name())
			}
		}
	}
	for _, pair := range [][2]string{{"/cl", "/clear"}, {"/cls", "/cls"}, {"/qu", "/quit"}, {"/can", "/cancel"}, {"/short", "/shortcuts"}, {"/expa", "/expand"}} {
		got := complete(pair[0], commands, nil)
		if len(got) != 1 || names(got)[0] != pair[1] || !strings.Contains(got[0], "\t") {
			t.Errorf("complete(%q) = %v, want one %s candidate with description", pair[0], got, pair[1])
		}
	}
	if got := names(complete("/exp", commands, nil)); len(got) != 2 || !contains(got, "/expand") || !contains(got, "/export") {
		t.Errorf("distinct commands sharing a prefix must remain available: %v", got)
	}
}

func TestCompleteAliasArgumentsAndSkillCollision(t *testing.T) {
	commands := []cmdEntry{{Name: "/keys", Aliases: []string{"/shortcuts"}, Desc: "keys", ArgHints: map[string][]string{"": {"all"}}}}
	got := complete("/SHORTCUTS a", commands, nil)
	if len(got) != 1 || got[0] != "/SHORTCUTS all" {
		t.Fatalf("alias argument completion = %v", got)
	}
	got = complete("/", commands, map[string]string{"shortcuts": "skill"})
	if len(got) != 1 || names(got)[0] != "/keys" || !strings.Contains(got[0], "/shortcuts") {
		t.Fatalf("alias grouping and skill collision = %v", got)
	}
}

func TestCompleteEmptyAndWhitespaceInput(t *testing.T) {
	if got := complete("", testCommands, nil); got != nil {
		t.Fatalf("complete(\"\") = %v, want nil", got)
	}
	if got := complete("   ", testCommands, nil); got != nil {
		t.Fatalf("complete(\"   \") = %v, want nil", got)
	}
}

func TestCompleteNoMatch(t *testing.T) {
	if got := complete("/zzzznope", testCommands, nil); len(got) != 0 {
		t.Fatalf("complete of an unknown prefix = %v, want empty", got)
	}
}

func TestCompleteIsSorted(t *testing.T) {
	got := names(complete("/", testCommands, nil))
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("suggestions are not sorted: %v", got)
		}
	}
}

// ---------- skills in completion ----------

func TestCompleteIncludesSkills(t *testing.T) {
	skillDescs := map[string]string{"security-audit": "Scan deps", "dockerize": ""}

	got := names(complete("/sec", testCommands, skillDescs))
	if !contains(got, "/security-audit") {
		t.Fatalf("complete(\"/sec\") = %v, want /security-audit", got)
	}

	// Without a slash, the bare skill name is the candidate.
	got = names(complete("dock", testCommands, skillDescs))
	if !contains(got, "dockerize") {
		t.Fatalf("complete(\"dock\") = %v, want dockerize", got)
	}

	// A skill with no description still gets a label rather than a bare name,
	// so the picker shows what it is.
	for _, s := range complete("dock", testCommands, skillDescs) {
		if strings.HasPrefix(s, "dockerize") && !strings.Contains(s, "\t") {
			t.Fatalf("a description-less skill was offered without a label: %q", s)
		}
	}
}

// TestCompleteSkillDoesNotShadowCommand pins the de-duplication: a skill whose
// name collides with a real command must not be offered twice.
func TestCompleteSkillDoesNotShadowCommand(t *testing.T) {
	skillDescs := map[string]string{"help": "a skill also called help"}
	got := names(complete("/hel", testCommands, skillDescs))

	n := 0
	for _, name := range got {
		if name == "/help" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("/help offered %d times, want exactly 1: %v", n, got)
	}
}

// ---------- completeArgs ----------

func TestCompleteArgsOffersToolArgumentNames(t *testing.T) {
	got := complete("read ", testCommands, nil)
	if len(got) == 0 {
		t.Fatal("no argument suggestions for \"read \"")
	}
	joined := strings.Join(got, " | ")
	for _, want := range []string{"filePath=", "offset=", "limit="} {
		if !strings.Contains(joined, want) {
			t.Errorf("argument suggestions missing %q: %s", want, joined)
		}
	}
	// Each suggestion must be a complete replacement line, i.e. keep the command.
	for _, s := range got {
		if !strings.HasPrefix(s, "read ") {
			t.Errorf("suggestion %q does not keep the command prefix", s)
		}
	}
}

// TestCompleteArgsSkipsAlreadyUsedArguments is the property that makes arg
// completion useful: an argument already present must not be offered again.
func TestCompleteArgsSkipsAlreadyUsedArguments(t *testing.T) {
	suggestions := complete("read filePath=x ", testCommands, nil)
	if len(suggestions) == 0 {
		t.Fatal("no suggestions for a partially-filled command")
	}

	// Each suggestion is a FULL replacement line, so it legitimately still
	// contains the "filePath=x" the user already typed. What must not happen is
	// filePath being offered again as the newly appended argument — assert on
	// the appended token, which is the suffix of the line.
	for _, s := range suggestions {
		if strings.HasSuffix(s, "filePath=") {
			t.Errorf("filePath was offered again although it is already set: %q", s)
		}
	}

	joined := strings.Join(suggestions, " | ")
	if !strings.Contains(joined, "offset=") || !strings.Contains(joined, "limit=") {
		t.Errorf("the remaining arguments were not offered: %s", joined)
	}
}

func TestCompleteArgsFiltersByCurrentPrefix(t *testing.T) {
	got := strings.Join(complete("edit old", testCommands, nil), " | ")
	if !strings.Contains(got, "oldString=") {
		t.Errorf("prefix \"old\" did not match oldString: %s", got)
	}
	if strings.Contains(got, "newString=") {
		t.Errorf("prefix \"old\" wrongly matched newString: %s", got)
	}
}

func TestCompleteArgsUnknownCommandYieldsNothing(t *testing.T) {
	if got := completeArgs("/nosuchcmd foo", testCommands); got != nil {
		t.Fatalf("completeArgs for an unknown command = %v, want nil", got)
	}
	// No space means we are still completing the command name, not its args.
	if got := completeArgs("read", testCommands); got != nil {
		t.Fatalf("completeArgs without a space = %v, want nil", got)
	}
}

// ---------- value hints ----------

func TestCompleteValueHintsForCommand(t *testing.T) {
	got := complete("/model ", testCommands, nil)
	joined := strings.Join(got, " | ")
	for _, want := range []string{"gpt-4o", "claude-sonnet-5", "deepseek-chat"} {
		if !strings.Contains(joined, want) {
			t.Errorf("value hints missing %q: %s", want, joined)
		}
	}

	// Typing a prefix narrows the hints.
	got = complete("/model claude", testCommands, nil)
	joined = strings.Join(got, " | ")
	if !strings.Contains(joined, "claude-sonnet-5") {
		t.Errorf("prefix \"claude\" did not match: %s", joined)
	}
	if strings.Contains(joined, "gpt-4o") {
		t.Errorf("prefix \"claude\" wrongly matched gpt-4o: %s", joined)
	}
}

// ---------- the small helpers ----------

func TestUsedArgNames(t *testing.T) {
	cases := map[string][]string{
		"filePath=x offset=2":     {"filePath", "offset"},
		`filePath="a b" limit=10`: {"filePath", "limit"},
		`{"filePath": "x"}`:       {"filePath"},
		"filePath=x bare":         {"filePath"},
		"":                        {},
		"nokeys here":             {},
	}
	for input, want := range cases {
		got := usedArgNames(input)
		for _, w := range want {
			if !got[w] {
				t.Errorf("usedArgNames(%q) missing %q (got %v)", input, w, got)
			}
		}
		if len(want) == 0 && len(got) != 0 {
			t.Errorf("usedArgNames(%q) = %v, want empty", input, got)
		}
	}
}

func TestCurrentArgPrefix(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"   ":            "",
		"file":           "file",
		"filePath=x off": "off",
		"filePath=x":     "", // a completed key=value is not a prefix
		"filePath=x ":    "",
		`"quoted`:        "quoted",
		"a:b":            "",
	}
	for input, want := range cases {
		if got := currentArgPrefix(input); got != want {
			t.Errorf("currentArgPrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestReplaceCurrentArgPrefix(t *testing.T) {
	cases := []struct {
		input, current, replacement, want string
	}{
		{"", "", "filePath=", "filePath="},
		{"   ", "", "filePath=", "filePath="},
		{"offset=1", "", "filePath=", "offset=1 filePath="},
		{"off", "off", "offset=", "offset="},
		{"filePath=x off", "off", "offset=", "filePath=x offset="},
		// A current prefix that is not actually present must be appended, not
		// silently dropped.
		{"filePath=x", "zzz", "offset=", "filePath=x offset="},
	}
	for _, tc := range cases {
		if got := replaceCurrentArgPrefix(tc.input, tc.current, tc.replacement); got != tc.want {
			t.Errorf("replaceCurrentArgPrefix(%q, %q, %q) = %q, want %q",
				tc.input, tc.current, tc.replacement, got, tc.want)
		}
	}
}

func TestFindCompletionEntry(t *testing.T) {
	if _, ok := findCompletionEntry("READ", testCommands); !ok {
		t.Error("findCompletionEntry is not case-insensitive")
	}
	if _, ok := findCompletionEntry("nope", testCommands); ok {
		t.Error("findCompletionEntry found a non-existent command")
	}
	e, _ := findCompletionEntry("/model", testCommands)
	if len(e.ArgHints[""]) == 0 {
		t.Error("findCompletionEntry returned an entry without its ArgHints")
	}
}

// ---------- toolArgNames ----------

func TestToolArgNamesParsesSchema(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"filePath":{"type":"string"},
			"offset":{"type":"integer"},
			"limit":{"type":"integer"}
		},
		"required":["filePath"]
	}`)
	got := toolArgNames(schema)
	if len(got) != 3 {
		t.Fatalf("toolArgNames = %v, want 3 names", got)
	}
	for _, want := range []string{"filePath", "offset", "limit"} {
		if !contains(got, want) {
			t.Errorf("toolArgNames missing %q: %v", want, got)
		}
	}
}

func TestToolArgNamesHandlesBadInput(t *testing.T) {
	for _, raw := range []json.RawMessage{
		nil,
		json.RawMessage(``),
		json.RawMessage(`not json`),
		json.RawMessage(`{}`),
		json.RawMessage(`{"properties":{}}`),
		json.RawMessage(`{"properties":"not an object"}`),
		json.RawMessage(`[]`),
	} {
		got := toolArgNames(raw)
		if len(got) != 0 {
			t.Errorf("toolArgNames(%s) = %v, want empty", raw, got)
		}
	}
}

// TestToolArgNamesIsDeterministic guards against map-iteration order leaking
// into the UI: the same schema must produce the same ordering every call.
func TestToolArgNamesIsDeterministic(t *testing.T) {
	schema := json.RawMessage(`{"properties":{"c":{},"a":{},"b":{},"d":{},"e":{}}}`)
	first := strings.Join(toolArgNames(schema), ",")
	for i := 0; i < 20; i++ {
		if got := strings.Join(toolArgNames(schema), ","); got != first {
			t.Fatalf("toolArgNames is order-unstable: %q vs %q", first, got)
		}
	}
}
