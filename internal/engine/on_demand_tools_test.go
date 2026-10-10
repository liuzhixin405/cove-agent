package engine

import (
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

func hasToolDef(defs []api.ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

// draw_image is sent to the model only when the conversation asks for an
// image: it used to sit in every request's tool list although coding turns
// never need it. Text-less primitives cannot draw a flowchart either; those
// go through write (SVG, Mermaid).
func TestDrawImageIsOfferedOnlyWhenTheConversationWantsAnImage(t *testing.T) {
	eng := newTestEngine(&mockProvider{}, tool.NewDrawImageTool(), &mockTool{name: "write"})
	eng.messages = []api.Message{{Role: "user", Content: "把 internal/api 的错误处理重构一下"}}
	if hasToolDef(eng.buildAPIToolDefs(), "draw_image") {
		t.Fatal("a coding request must not carry draw_image")
	}
	if !hasToolDef(eng.buildAPIToolDefs(), "write") {
		t.Fatal("other tools stay")
	}
	for _, msg := range []string{"帮我画一张 200x200 的占位图", "generate a placeholder PNG icon", "给 README 配一张示意图片"} {
		eng.messages = []api.Message{{Role: "user", Content: msg}}
		if !hasToolDef(eng.buildAPIToolDefs(), "draw_image") {
			t.Fatalf("%q must bring draw_image in", msg)
		}
	}
	// Once used in the conversation it stays available.
	eng.messages = []api.Message{
		{Role: "user", Content: "画个图"},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "1", Name: "draw_image", Input: map[string]any{}}}},
		{Role: "user", Content: "再调一下颜色"},
	}
	if !hasToolDef(eng.buildAPIToolDefs(), "draw_image") {
		t.Fatal("a tool already used in the conversation stays offered")
	}
}
