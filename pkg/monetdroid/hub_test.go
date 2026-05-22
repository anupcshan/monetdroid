package monetdroid

import (
	"os"
	"strings"
	"testing"
)

func TestNewHubClaudeCommand(t *testing.T) {
	t.Run("nil command succeeds with empty override", func(t *testing.T) {
		h, err := NewHubWithDataDir("http://127.0.0.1:0", t.TempDir(), nil)
		if err != nil {
			t.Fatalf("NewHubWithDataDir: %v", err)
		}
		if len(h.claudeCommand) != 0 {
			t.Errorf("expected empty claudeCommand, got %v", h.claudeCommand)
		}
	})

	t.Run("missing binary returns error", func(t *testing.T) {
		_, err := NewHubWithDataDir("http://127.0.0.1:0", t.TempDir(), []string{"definitely-not-a-real-binary-xyz123"})
		if err == nil {
			t.Fatal("expected error for missing binary")
		}
	})

	t.Run("valid binary with extra args is stored", func(t *testing.T) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable: %v", err)
		}
		want := []string{exe, "--foo", "--bar"}
		h, err := NewHubWithDataDir("http://127.0.0.1:0", t.TempDir(), want)
		if err != nil {
			t.Fatalf("NewHubWithDataDir: %v", err)
		}
		if len(h.claudeCommand) != len(want) {
			t.Fatalf("expected %v, got %v", want, h.claudeCommand)
		}
		for i := range want {
			if h.claudeCommand[i] != want[i] {
				t.Errorf("claudeCommand[%d]: want %q, got %q", i, want[i], h.claudeCommand[i])
			}
		}
	})
}

// When a tool_use/tool_result pair straddles a pagination boundary, the result
// must render exactly once across the two slices: standalone in the slice that
// contains the tool_result, and NOT injected into the chip slot of the slice
// that contains only the tool_use. precomputeRenderContext.toolResultIndexes
// is what allows renderMessages to make that distinction.
func TestRenderMessages_PaginationSplitDoesNotDuplicateResult(t *testing.T) {
	const payload = "UNIQUE-PAYLOAD-MARKER"
	log := []ServerMsg{
		{Type: "user_message", Text: "hi"},
		{Type: "tool_use", Tool: "Grep", ToolUseID: "t1"},
		{Type: "text", Text: "thinking"},
		{Type: "tool_result", ToolUseID: "t1", Output: payload},
	}
	rc := precomputeRenderContext(log)

	older := renderMessages(log, 0, 2, rc, "sess") // user_message + tool_use
	newer := renderMessages(log, 2, 4, rc, "sess") // text + tool_result

	olderCount := strings.Count(older, payload)
	newerCount := strings.Count(newer, payload)

	if olderCount != 0 {
		t.Errorf("older slice (tool_use only) should not contain result payload; got %d occurrences", olderCount)
	}
	if newerCount != 1 {
		t.Errorf("newer slice (tool_result only) should contain payload exactly once; got %d", newerCount)
	}
	if olderCount+newerCount != 1 {
		t.Errorf("total payload occurrences across both slices should be 1; got %d", olderCount+newerCount)
	}
}

// When both tool_use and tool_result are in the same rendered slice, the
// standalone tool_result chip is suppressed and the payload appears exactly
// once, nested inside the tool_use chip's result-slot.
func TestRenderMessages_SameSliceNestsResult(t *testing.T) {
	const payload = "UNIQUE-PAYLOAD-MARKER"
	log := []ServerMsg{
		{Type: "tool_use", Tool: "Grep", ToolUseID: "t1"},
		{Type: "tool_result", ToolUseID: "t1", Output: payload},
	}
	rc := precomputeRenderContext(log)
	out := renderMessages(log, 0, 2, rc, "sess")

	if count := strings.Count(out, payload); count != 1 {
		t.Errorf("payload should appear exactly once when pair is in same slice; got %d", count)
	}
	if !strings.Contains(out, `id="tool-result-slot-t1">`) {
		t.Errorf("expected populated result-slot for t1; output:\n%s", out)
	}
	// The standalone tool_result chip class must be absent: only the nested
	// result-slot should carry the payload.
	if strings.Contains(out, `tool-result-chip`) {
		t.Errorf("standalone tool_result chip should not render when nested inside tool_use; output:\n%s", out)
	}
}
