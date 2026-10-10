package engine

import (
	"strings"
	"testing"
)

func TestSuggestsComplexTask_ShortSimpleMessage(t *testing.T) {
	if suggestsComplexTask("fix the typo in README") {
		t.Fatal("expected a short, simple message to not be flagged as complex")
	}
}

func TestSuggestsComplexTask_LongMessage(t *testing.T) {
	msg := strings.Repeat("please handle this carefully ", 12) // > 300 chars
	if !suggestsComplexTask(msg) {
		t.Fatal("expected a long message to be flagged as complex")
	}
}

func TestSuggestsComplexTask_Keyword(t *testing.T) {
	if !suggestsComplexTask("please refactor the auth module") {
		t.Fatal("expected a keyword match to be flagged as complex")
	}
	if !suggestsComplexTask("请重构一下权限模块") {
		t.Fatal("expected a Chinese keyword match to be flagged as complex")
	}
}

func TestSuggestsComplexTask_MultipleFiles(t *testing.T) {
	if !suggestsComplexTask("update main.go, handler.go and util.go together") {
		t.Fatal("expected 3+ file mentions to be flagged as complex")
	}
	if suggestsComplexTask("update main.go") {
		t.Fatal("a single short file mention alone should not be flagged as complex")
	}
}

func TestTaskDecompositionGuidance_EmptyForSimpleMessage(t *testing.T) {
	if g := taskDecompositionGuidance("fix the typo in README"); g != "" {
		t.Fatalf("expected no guidance for a simple message, got: %q", g)
	}
}

func TestTaskDecompositionGuidance_NonEmptyForComplexMessage(t *testing.T) {
	g := taskDecompositionGuidance("please refactor the entire auth module")
	if g == "" {
		t.Fatal("expected guidance for a complex-looking message")
	}
	if !strings.Contains(g, "3-8 tasks") {
		t.Fatalf("expected guidance to mention step decomposition, got: %q", g)
	}
}

// The length threshold counts characters, not bytes: 120 Chinese characters
// (360 bytes) are a short message, not a multi-step task.
func TestSuggestsComplexTaskCountsRunes(t *testing.T) {
	short := strings.Repeat("中", 120) // 360 bytes, 120 runes
	if suggestsComplexTask(short) {
		t.Fatal("120 Chinese characters counted as a long message")
	}
	long := strings.Repeat("中", 300)
	if !suggestsComplexTask(long) {
		t.Fatal("300 Chinese characters not counted as a long message")
	}
}

// The guidance teaches the parallel path: independent steps run through
// execute_plan, ordering is declared with depends:. It used to tell the model
// to work one step at a time, which argued against the concurrency cove has.
func TestTaskDecompositionGuidanceTeachesParallelExecution(t *testing.T) {
	g := taskDecompositionGuidance("请重构一下权限模块")
	for _, want := range []string{"execute_plan", "depends:", "parallel"} {
		if !strings.Contains(g, want) {
			t.Errorf("guidance lacks %q:\n%s", want, g)
		}
	}
	if strings.Contains(g, "one at a time") {
		t.Errorf("guidance still tells the model to work serially:\n%s", g)
	}
}
