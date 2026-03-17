package monetdroid

// suppressResultTools lists tools whose tool_result output should not be
// shown to the user (the tool_use chip is still rendered).
var suppressResultTools = map[string]bool{
	"TodoWrite":       true,
	"AskUserQuestion": true,
	"Read":            true,
	"FileRead":        true,
}

// handleStreamEvent processes non-control messages from the CLI and broadcasts them.
func handleStreamEvent(s *Session, event *streamEvent, broadcast func(ServerMsg)) {
	switch event.Type {
	case "system":
		if event.Subtype == "task_notification" && event.ToolUseID != "" {
			broadcast(ServerMsg{Type: "task_done", SessionID: s.ID, ToolUseID: event.ToolUseID})
			s.Mu.Lock()
			if ch, ok := s.BgTaskStops[event.ToolUseID]; ok {
				close(ch)
				delete(s.BgTaskStops, event.ToolUseID)
			}
			s.Mu.Unlock()
		}

	case "assistant":
		for _, b := range event.Message.Content.Blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					broadcast(ServerMsg{Type: "text", SessionID: s.ID, Text: b.Text})
				}
			case "tool_use":
				if suppressResultTools[b.Name] {
					s.Mu.Lock()
					s.SuppressedToolIDs[b.ID] = b.Name
					s.Mu.Unlock()
				}
				if b.Name == "AskUserQuestion" {
					continue // rendered by the permission prompt UI
				}
				broadcast(ServerMsg{Type: "tool_use", SessionID: s.ID, Tool: b.Name, ToolUseID: b.ID, Input: b.Input})
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
		for _, b := range event.Message.Content.Blocks {
			if b.Type == "tool_result" {
				s.Mu.Lock()
				_, suppressed := s.SuppressedToolIDs[b.ToolUseID]
				if suppressed {
					delete(s.SuppressedToolIDs, b.ToolUseID)
				}
				s.Mu.Unlock()
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
