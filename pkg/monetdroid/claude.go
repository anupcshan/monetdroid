package monetdroid

import "github.com/anupcshan/monetdroid/pkg/claude/protocol"

// suppressResultTools lists tools whose tool_result output should not be
// shown to the user (the tool_use chip is still rendered).
var suppressResultTools = map[string]bool{
	"TodoWrite":       true,
	"AskUserQuestion": true,
	"Read":            true,
	"FileRead":        true,
	"Agent":           true,
}

// parentID extracts the parent tool_use ID from a stream event, or "" if none.
func parentID(event *protocol.StreamEvent) string {
	if event.ParentToolUseID != nil {
		return *event.ParentToolUseID
	}
	return ""
}

// handleRawStreamEvent processes raw streaming deltas (--include-partial-messages)
// and broadcasts text/thinking deltas for live display. Sub-agent deltas are ignored
// (their content is buffered by the final assistant event).
func handleRawStreamEvent(s *Session, raw *protocol.RawStreamEvent, broadcast func(ServerMsg)) {
	// Skip sub-agent streaming — too noisy, and the buffered view handles it.
	if raw.ParentToolUseID != nil {
		return
	}

	inner := raw.Event
	if inner.Type != "content_block_delta" {
		return
	}
	switch inner.Delta.Type {
	case "text_delta":
		if inner.Delta.Text != "" {
			broadcast(ServerMsg{Type: "text_delta", SessionID: s.ID, Text: inner.Delta.Text})
		}
	case "thinking_delta":
		if inner.Delta.Thinking != "" {
			broadcast(ServerMsg{Type: "thinking_delta", SessionID: s.ID, Text: inner.Delta.Thinking})
		}
	}
}

// handleStreamEvent processes non-control messages from the CLI and broadcasts them.
func handleStreamEvent(s *Session, event *protocol.StreamEvent, broadcast func(ServerMsg)) {
	pid := parentID(event)

	switch event.Type {
	case "system":
		switch event.Subtype {
		case "task_started":
			if event.TaskType == "local_agent" && event.ToolUseID != "" {
				s.StartAgent(event.ToolUseID, event.Description)
				broadcast(ServerMsg{Type: "agent_started", SessionID: s.ID, ToolUseID: event.ToolUseID})
			}
		case "task_progress":
			if event.ToolUseID != "" {
				s.UpdateAgentStat(event.ToolUseID, event.TaskUsage, event.Description, event.LastToolName)
				stat := s.GetAgentStat(event.ToolUseID)
				if stat != nil {
					broadcast(ServerMsg{Type: "agent_progress", SessionID: s.ID, ToolUseID: event.ToolUseID, AgentStat: stat})
				}
			}
		case "task_notification":
			if event.ToolUseID != "" {
				if event.TaskUsage != nil {
					s.UpdateAgentStat(event.ToolUseID, event.TaskUsage, event.Summary, "")
				}
				s.FinishAgent(event.ToolUseID)
				broadcast(ServerMsg{Type: "task_done", SessionID: s.ID, ToolUseID: event.ToolUseID})
				s.CloseBgStop(event.ToolUseID)
			}
		}

	case "assistant":
		// Sub-agent assistant events: buffer instead of broadcasting
		if pid != "" {
			for _, b := range event.Message.Content.Blocks {
				switch b.Type {
				case "tool_use":
					if suppressResultTools[b.Name] {
						s.SuppressTool(b.ID, b.Name)
					}
					if b.Name == "AskUserQuestion" {
						continue
					}
					s.BufferAgentEvent(pid, ServerMsg{Type: "tool_use", SessionID: s.ID, Tool: b.Name, ToolUseID: b.ID, Input: protocol.ParseToolInput(b.Name, b.RawInput), ParentToolUseID: pid})
				case "text":
					if b.Text != "" {
						s.BufferAgentEvent(pid, ServerMsg{Type: "text", SessionID: s.ID, Text: b.Text, ParentToolUseID: pid})
					}
				case "thinking":
					if b.Thinking != "" {
						s.BufferAgentEvent(pid, ServerMsg{Type: "thinking", SessionID: s.ID, Text: b.Thinking, ParentToolUseID: pid})
					}
				}
			}
			// Skip cost accumulation for sub-agent events
			return
		}

		// Parent assistant events: broadcast normally
		for _, b := range event.Message.Content.Blocks {
			switch b.Type {
			case "thinking":
				if b.Thinking != "" {
					broadcast(ServerMsg{Type: "thinking", SessionID: s.ID, Text: b.Thinking})
				}
			case "text":
				if b.Text != "" {
					broadcast(ServerMsg{Type: "text", SessionID: s.ID, Text: b.Text})
				}
			case "tool_use":
				if suppressResultTools[b.Name] {
					s.SuppressTool(b.ID, b.Name)
				}
				if b.Name == "AskUserQuestion" {
					continue // rendered by the permission prompt UI
				}
				broadcast(ServerMsg{Type: "tool_use", SessionID: s.ID, Tool: b.Name, ToolUseID: b.ID, Input: protocol.ParseToolInput(b.Name, b.RawInput)})
			}
		}
		if u := event.Message.Usage; u != nil {
			contextUsed := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens
			if contextUsed > 0 {
				broadcast(ServerMsg{
					Type: "cost", SessionID: s.ID,
					Cost: &CostInfo{ContextUsed: contextUsed},
				})
			}
		}

	case "result":
		if event.Result != "" {
			broadcast(ServerMsg{Type: "result", SessionID: s.ID, Text: event.Result})
		}
		cost := &CostInfo{}
		if event.TotalCost > 0 {
			cost.TotalCostUSD = event.TotalCost
		}
		for _, info := range event.ModelUsage {
			if info.ContextWindow > 0 {
				cost.ContextWindow = info.ContextWindow
			}
			break
		}
		if cost.TotalCostUSD > 0 || cost.ContextWindow > 0 {
			broadcast(ServerMsg{Type: "cost", SessionID: s.ID, Cost: cost})
		}

	case "user":
		// Sub-agent user events: buffer instead of broadcasting
		if pid != "" {
			for _, b := range event.Message.Content.Blocks {
				if b.Type == "tool_result" {
					suppressed := s.RemoveSuppressed(b.ToolUseID)
					if len(b.Content.Images) > 0 {
						s.BufferAgentEvent(pid, ServerMsg{Type: "tool_result", SessionID: s.ID, ToolUseID: b.ToolUseID, Images: b.Content.Images, ParentToolUseID: pid})
						continue
					}
					if suppressed {
						continue
					}
					output := b.Content.String()
					if !isBoringResult(output) {
						s.BufferAgentEvent(pid, ServerMsg{Type: "tool_result", SessionID: s.ID, ToolUseID: b.ToolUseID, Output: Truncate(output, 2000), ParentToolUseID: pid})
					}
				}
			}
			return
		}

		// Parent user events: broadcast normally
		for _, b := range event.Message.Content.Blocks {
			if b.Type == "tool_result" {
				suppressed := s.RemoveSuppressed(b.ToolUseID)

				// Agent tool_results: buffer into the agent's detail view instead of main stream
				if s.GetAgentStat(b.ToolUseID) != nil {
					output := b.Content.String()
					s.BufferAgentEvent(b.ToolUseID, ServerMsg{Type: "tool_result", SessionID: s.ID, ToolUseID: b.ToolUseID, Output: Truncate(output, 2000)})
					continue
				}

				// Always show images even for suppressed tools (e.g. Read on screenshots).
				if len(b.Content.Images) > 0 {
					broadcast(ServerMsg{Type: "tool_result", SessionID: s.ID, ToolUseID: b.ToolUseID, Images: b.Content.Images})
					continue
				}
				if suppressed {
					continue
				}
				output := b.Content.String()
				if !isBoringResult(output) {
					broadcast(ServerMsg{Type: "tool_result", SessionID: s.ID, ToolUseID: b.ToolUseID, Output: Truncate(output, 2000)})
				}
			}
		}
	}
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}
