package view

import (
	"testing"

	"ai-usage/internal/usage"
)

func TestDetailGroupOf(t *testing.T) {
	if g := DetailGroupOf(usage.ToolClaude, "personal"); g != GroupOf(usage.ToolClaude, "personal") {
		t.Fatalf("slotted pair changed: %+v", g)
	}
	a := DetailGroupOf(usage.ToolGateway, "work-foundry")
	b := DetailGroupOf(usage.ToolGateway, "github-models")
	if a.Label != "Gateway · work-foundry" || a.Slot != Other.Slot || a.Key == b.Key {
		t.Fatalf("gateway groups: %+v %+v", a, b)
	}
	if g := DetailGroupOf("mystery", ""); g.Label != "mystery" {
		t.Fatalf("unknown tool: %+v", g)
	}
}
