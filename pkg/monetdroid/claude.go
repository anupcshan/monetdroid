package monetdroid

import (
	"encoding/json"
)

// handleStreamEvent processes non-control messages from the CLI and broadcasts them.
func handleStreamEvent(s *Session, event map[string]any, broadcast func(ServerMsg)) {
	eventType, _ := event["type"].(string)

	switch eventType {
	case "system":
		if sid, ok := event["session_id"].(string); ok && sid != "" {
			s.Mu.Lock()
			wasEmpty := s.ClaudeID == ""
			if wasEmpty {
				s.ClaudeID = sid
			}
			s.Mu.Unlock()
			if wasEmpty {
				broadcast(ServerMsg{Type: "session_id", SessionID: s.ID, Text: sid})
			}
		}

	case "assistant":
		msg, ok := event["message"].(map[string]any)
		if !ok {
			return
		}
		content, ok := msg["content"].([]any)
		if !ok {
			return
		}
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := b["type"].(string)
			switch blockType {
			case "text":
				text, _ := b["text"].(string)
				if text != "" {
					broadcast(ServerMsg{Type: "text", SessionID: s.ID, Text: text})
				}
			case "tool_use":
				name, _ := b["name"].(string)
				input := b["input"]
				s.Mu.Lock()
				s.LastTool = name
				s.Mu.Unlock()
				broadcast(ServerMsg{Type: "tool_use", SessionID: s.ID, Tool: name, Input: input})
			}
		}
		if usage, ok := msg["usage"].(map[string]any); ok {
			inTok, _ := usage["input_tokens"].(float64)
			outTok, _ := usage["output_tokens"].(float64)
			cacheRead, _ := usage["cache_read_input_tokens"].(float64)
			cacheCreate, _ := usage["cache_creation_input_tokens"].(float64)
			contextUsed := int(inTok + cacheRead + cacheCreate + outTok)
			if contextUsed > 0 {
				broadcast(ServerMsg{
					Type: "cost", SessionID: s.ID,
					Cost: &CostInfo{ContextUsed: contextUsed},
				})
			}
		}

	case "result":
		if text, ok := event["result"].(string); ok && text != "" {
			broadcast(ServerMsg{Type: "result", SessionID: s.ID, Text: text})
		}
		if sid, ok := event["session_id"].(string); ok && sid != "" {
			s.Mu.Lock()
			s.ClaudeID = sid
			s.Mu.Unlock()
		}
		cost := &CostInfo{}
		if totalCost, ok := event["total_cost_usd"].(float64); ok {
			cost.TotalCostUSD = totalCost
		}
		if mu, ok := event["modelUsage"].(map[string]any); ok {
			for _, v := range mu {
				if info, ok := v.(map[string]any); ok {
					if cw, ok := info["contextWindow"].(float64); ok && cw > 0 {
						cost.ContextWindow = int(cw)
					}
				}
				break
			}
		}
		if cost.TotalCostUSD > 0 || cost.ContextWindow > 0 {
			broadcast(ServerMsg{Type: "cost", SessionID: s.ID, Cost: cost})
		}

	case "user":
		msg, ok := event["message"].(map[string]any)
		if !ok {
			return
		}
		content, ok := msg["content"].([]any)
		if !ok {
			return
		}
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if b["type"] == "tool_result" {
				output := ""
				switch c := b["content"].(type) {
				case string:
					output = c
				default:
					j, _ := json.Marshal(c)
					output = string(j)
				}
				if !isBoringResult(output) {
					broadcast(ServerMsg{Type: "tool_result", SessionID: s.ID, Output: Truncate(output, 2000)})
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
