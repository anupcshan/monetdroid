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

// TestEventLogCompactKey_Implicit verifies that compactKey derives keys
// only for single-swap events using innerHTML or outerHTML.
func TestEventLogCompactKey_Implicit(t *testing.T) {
	t.Run("innerHTML single swap gets key from element id", func(t *testing.T) {
		el := &EventLog{}
		event := FormatSSE("htmx", OobSwap("cost-bar", "innerHTML", "<span>5¢</span>"))
		el.Append(event, "", "")
		seq := el.seq
		if el.events[0].CompactKey != "cost-bar" {
			t.Errorf("expected CompactKey 'cost-bar', got %q", el.events[0].CompactKey)
		}
		// Same key replaces.
		event2 := FormatSSE("htmx", OobSwap("cost-bar", "innerHTML", "<span>10¢</span>"))
		el.Append(event2, "", "")
		if el.seq != seq+1 {
			t.Errorf("seq should increment: %d -> %d", seq, el.seq)
		}
		if len(el.events) != 1 {
			t.Fatalf("expected 1 event after compaction, got %d", len(el.events))
		}
		if el.events[0].Seq != seq+1 {
			t.Errorf("expected seq %d, got %d", seq+1, el.events[0].Seq)
		}
	})

	t.Run("outerHTML single swap gets key from element id", func(t *testing.T) {
		el := &EventLog{}
		event := FormatSSE("htmx", OobSwap("spinner-t1", "outerHTML", ""))
		el.Append(event, "", "")
		if el.events[0].CompactKey != "spinner-t1" {
			t.Errorf("expected CompactKey 'spinner-t1', got %q", el.events[0].CompactKey)
		}
	})

	t.Run("beforeend single swap gets no key", func(t *testing.T) {
		el := &EventLog{}
		event := FormatSSE("htmx", OobSwap("msg-content", "beforeend", "<div>msg</div>"))
		el.Append(event, "", "")
		if el.events[0].CompactKey != "" {
			t.Errorf("beforeend should have no CompactKey, got %q", el.events[0].CompactKey)
		}
		// Another beforeend accumulates, does not replace.
		event2 := FormatSSE("htmx", OobSwap("msg-content", "beforeend", "<div>msg2</div>"))
		el.Append(event2, "", "")
		if len(el.events) != 2 {
			t.Errorf("beforeend events should accumulate, got %d", len(el.events))
		}
	})

	t.Run("multi-swap event gets no implicit key", func(t *testing.T) {
		el := &EventLog{}
		event := FormatSSE("htmx", strings.Join([]string{
			OobSwap("streaming", "innerHTML", ""),
			OobSwap("thinking", "innerHTML", ""),
		}, "\n"))
		el.Append(event, "", "")
		if el.events[0].CompactKey != "" {
			t.Errorf("multi-swap should have no implicit CompactKey, got %q", el.events[0].CompactKey)
		}
	})
}

// TestEventLogCompactKey_Explicit verifies that an explicit compact key
// overrides implicit derivation and causes superseding.
func TestEventLogCompactKey_Explicit(t *testing.T) {
	t.Run("explicit key supersedes previous same key", func(t *testing.T) {
		el := &EventLog{}
		event := FormatSSE("htmx", OobSwap("streaming", "innerHTML", "old"))
		el.Append(event, "streaming", "")
		if len(el.events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(el.events))
		}
		// Second with same explicit key replaces, even if HTML id differs.
		event2 := FormatSSE("htmx", OobSwap("streaming", "innerHTML", "new"))
		el.Append(event2, "streaming", "")
		if len(el.events) != 1 {
			t.Fatalf("explicit key should supersede; got %d events", len(el.events))
		}
		if !strings.Contains(el.events[0].Event, "new") {
			t.Errorf("expected 'new' content, got: %s", el.events[0].Event)
		}
	})

	t.Run("explicit key overrides implicit multi-swap exclusion", func(t *testing.T) {
		el := &EventLog{}
		// A multi-swap event (2 swaps) normally gets no implicit key.
		liveClear := FormatSSE("htmx", strings.Join([]string{
			OobSwap("streaming", "innerHTML", ""),
			OobSwap("thinking", "innerHTML", ""),
		}, "\n"))
		el.Append(liveClear, "streaming", "")
		if el.events[0].CompactKey != "streaming" {
			t.Errorf("explicit key should override multi-swap exclusion, got key %q", el.events[0].CompactKey)
		}
		// Second multi-swap with same explicit key supersedes the first.
		el.Append(liveClear, "streaming", "")
		if len(el.events) != 1 {
			t.Errorf("explicit key on multi-swap should still compact, got %d", len(el.events))
		}
	})
}

// TestEventLogCompactKey_ParentRemoval verifies cascading removal. When a
// CompactKey match supersedes an event, all events with a matching ParentKey
// are removed with it.
func TestEventLogCompactKey_ParentRemoval(t *testing.T) {
	t.Run("superseding parent removes child fragments", func(t *testing.T) {
		el := &EventLog{}
		// Container with explicit key.
		el.Append(FormatSSE("htmx", OobSwap("streaming", "innerHTML", "<div id='detail'>Hello</div>")), "streaming", "")
		if len(el.events) != 1 {
			t.Fatalf("expected 1 container event, got %d", len(el.events))
		}
		// Child fragments parented to the container's key.
		el.Append(FormatSSE("htmx", OobSwap("detail", "beforeend", " world")), "", "streaming")
		el.Append(FormatSSE("htmx", OobSwap("detail", "beforeend", "!")), "", "streaming")
		if len(el.events) != 3 {
			t.Fatalf("expected 3 events (container + 2 fragments), got %d", len(el.events))
		}
		// A new event with same CompactKey supersedes the container and removes children.
		el.Append(FormatSSE("htmx", OobSwap("streaming", "innerHTML", "")), "streaming", "")
		if len(el.events) != 1 {
			t.Errorf("superseding parent should cascade-remove fragments, got %d events", len(el.events))
		}
	})

	t.Run("fragments with no parent survive parent supersede", func(t *testing.T) {
		el := &EventLog{}
		el.Append(FormatSSE("htmx", OobSwap("streaming", "innerHTML", "<div>Hi</div>")), "streaming", "")
		// Fragment with a different parent key is not cleaned.
		el.Append(FormatSSE("htmx", OobSwap("other", "beforeend", "x")), "", "other-parent")
		// Supersede the container.
		el.Append(FormatSSE("htmx", OobSwap("streaming", "innerHTML", "")), "streaming", "")
		if len(el.events) != 2 {
			t.Errorf("unrelated parent key should survive; got %d events", len(el.events))
		}
	})

	t.Run("parent key does not cause self-compaction", func(t *testing.T) {
		el := &EventLog{}
		el.Append(FormatSSE("htmx", OobSwap("detail", "beforeend", "A")), "", "streaming")
		el.Append(FormatSSE("htmx", OobSwap("detail", "beforeend", "B")), "", "streaming")
		if len(el.events) != 2 {
			t.Errorf("parent key alone should not compact siblings; got %d", len(el.events))
		}
	})

	t.Run("superseding clears only matching parent key", func(t *testing.T) {
		el := &EventLog{}
		el.Append(FormatSSE("htmx", OobSwap("streaming", "innerHTML", "c1")), "streaming", "")
		el.Append(FormatSSE("htmx", OobSwap("d1", "beforeend", "f1")), "", "streaming")
		el.Append(FormatSSE("htmx", OobSwap("d1", "beforeend", "f2")), "", "streaming")
		// Another container with a different key and its own children.
		el.Append(FormatSSE("htmx", OobSwap("thinking", "innerHTML", "c2")), "thinking", "")
		el.Append(FormatSSE("htmx", OobSwap("d2", "beforeend", "f3")), "", "thinking")
		// Supersede only "streaming".
		el.Append(FormatSSE("htmx", OobSwap("streaming", "innerHTML", "")), "streaming", "")
		if len(el.events) != 3 {
			t.Errorf("only 'streaming'-parented fragments should be removed; got %d events", len(el.events))
		}
	})
}

// TestRenderMessages_PaginationSplitDoesNotDuplicateResult verifies that a
// tool_use/tool_result pair straddling a pagination boundary renders the
// result exactly once: standalone in the tool_result slice, not injected
// into the chip slot of the tool_use-only slice.
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
