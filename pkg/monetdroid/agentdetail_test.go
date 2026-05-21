package monetdroid

import (
	"strings"
	"testing"
)

// Sub-agent tool_results must nest inside their tool_use chip's result-slot
// on replay, mirroring the top-level timeline. The standalone result chip
// must not also render, and the payload must appear exactly once.
func TestRenderFinalSubagentSection_NestsResultInChip(t *testing.T) {
	const payload = "SUBAGENT-PAYLOAD-MARKER"
	st := &subagentRenderState{
		Section: &SubagentSection{AgentID: "a1", AgentType: "general-purpose"},
		InnerEvents: []ServerMsg{
			{Type: "tool_use", Tool: "Grep", ToolUseID: "st1", AgentID: "a1"},
			{Type: "tool_result", ToolUseID: "st1", Output: payload, AgentID: "a1"},
		},
	}
	out := renderFinalSubagentSection(st)

	if count := strings.Count(out, payload); count != 1 {
		t.Errorf("payload should appear exactly once; got %d\noutput:\n%s", count, out)
	}
	if !strings.Contains(out, `id="tool-result-slot-st1">`) {
		t.Errorf("expected populated result-slot for st1; output:\n%s", out)
	}
	// The standalone subagent tool_result chip uses tool-result-chip class.
	// It must not appear when the result is nested inside the tool_use chip.
	if strings.Contains(out, `tool-result-chip`) {
		t.Errorf("standalone subagent tool_result chip should not render when nested; output:\n%s", out)
	}
}

// Orphan tool_results inside a sub-agent (no matching tool_use in the same
// section) fall back to the standalone chip so the data is not lost.
func TestRenderFinalSubagentSection_OrphanResultStandsAlone(t *testing.T) {
	const payload = "ORPHAN-PAYLOAD-MARKER"
	st := &subagentRenderState{
		Section: &SubagentSection{AgentID: "a1", AgentType: "general-purpose"},
		InnerEvents: []ServerMsg{
			{Type: "tool_result", ToolUseID: "missing-tool-use", Output: payload, AgentID: "a1"},
		},
	}
	out := renderFinalSubagentSection(st)

	if count := strings.Count(out, payload); count != 1 {
		t.Errorf("orphan payload should appear exactly once standalone; got %d\noutput:\n%s", count, out)
	}
	if !strings.Contains(out, `tool-result-chip`) {
		t.Errorf("orphan result should fall back to standalone tool-result-chip; output:\n%s", out)
	}
}
