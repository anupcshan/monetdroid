package monetdroid

import (
	"encoding/json"
	"log"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// bgTaskNotificationPattern extracts tool-use-id and status from
// <task-notification> XML injected into user prompts on bg task completion.
var bgTaskNotificationPattern = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>.*?<status>([^<]+)</status>`)

// suppressResultTools lists tools whose tool_result output should not be
// shown to the user (the tool_use chip is still rendered).
var suppressResultTools = map[string]bool{
	"TodoWrite":       true,
	"TaskCreate":      true,
	"TaskUpdate":      true,
	"AskUserQuestion": true,
	"Read":            true,
	"FileRead":        true,
	"Agent":           true,
}

// handleRawStreamEvent processes raw streaming deltas (--include-partial-messages)
// and broadcasts text/thinking deltas for live display. Sub-agent deltas are ignored
// (their content is buffered by the final assistant event).
func handleRawStreamEvent(s *Session, raw *protocol.RawStreamEvent, broadcast func(ServerMsg)) {
	// Skip sub-agent streaming. The buffered view handles it, and raw deltas are too noisy.
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

// parentID extracts the parent tool_use ID from a stream event, or "" if none.
func parentID(event *protocol.StreamEvent) string {
	if event.ParentToolUseID != nil {
		return *event.ParentToolUseID
	}
	return ""
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
				broadcast(ServerMsg{
					Type:        "subagent_started",
					SessionID:   s.ID,
					AgentID:     event.ToolUseID,
					AgentType:   event.SubagentType,
					Description: event.Description,
				})
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
				stat := s.GetAgentStat(event.ToolUseID)
				// A background agent deferred finalization from its launch
				// tool_result. task_notification is its completion point. It
				// carries the real summary and totals, so finalize here.
				// Description is left blank, unlike the foreground path.
				// UpdateAgentStat above stored the summary into
				// stat.Description. Propagating it would overwrite the
				// subagent_started heading with that summary on a re-render.
				if stat != nil && stat.Background {
					s.FinishAgent(event.ToolUseID)
					finished := ServerMsg{
						Type:      "subagent_finished",
						SessionID: s.ID,
						AgentID:   event.ToolUseID,
						Text:      event.Summary,
					}
					finished.TotalTokens = stat.TotalTokens
					finished.TotalToolUses = stat.ToolUses
					finished.DurationMs = stat.DurationMs
					broadcast(finished)
					broadcast(ServerMsg{Type: "task_done", SessionID: s.ID, ToolUseID: event.ToolUseID})
				} else if stat == nil {
					// Untracked background task (e.g. Bash): emit task_done.
					broadcast(ServerMsg{Type: "task_done", SessionID: s.ID, ToolUseID: event.ToolUseID})
				}
				s.CloseBgStop(event.ToolUseID)
			}
		}

	case "assistant":
		// Sub-agent assistant events: route inner tool_use into the
		// section body. The sub-agent's answer text is not streamed here.
		// It arrives as the parent's Agent tool_result and renders via
		// subagent_finished, so streaming it would duplicate the final
		// text and leak into the main timeline.
		if pid != "" {
			// A forked skill runs in a sub-agent context but never emits the
			// task_started lifecycle event that announces an Agent invocation.
			// When a parented message arrives for an unannounced parent, create
			// the sub-agent section the missing event would have created.
			if s.GetAgentStat(pid) == nil {
				s.StartAgent(pid, event.TaskDescription)
				broadcast(ServerMsg{
					Type:        "subagent_started",
					SessionID:   s.ID,
					AgentID:     pid,
					AgentType:   event.SubagentType,
					Description: event.TaskDescription,
				})
			}
			for _, b := range event.Message.Content.Blocks {
				if b.Type == "tool_use" {
					if suppressResultTools[b.Name] {
						s.SuppressTool(b.ID, b.Name)
					}
					if b.Name == "AskUserQuestion" {
						continue
					}
					broadcast(ServerMsg{Type: "tool_use", SessionID: s.ID, Tool: b.Name, ToolUseID: b.ID, Input: protocol.ParseToolInput(b.Name, b.RawInput), AgentID: pid})
				}
			}
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
					continue
				}
				// The sub-agent section rendered from task_started stands in
				// for the parent Agent chip, so Agent gets no tool_use of its
				// own. Registration still happens here: it precedes
				// task_started, and the parent tool_result below finalizes
				// whatever is registered.
				if b.Name == "Agent" {
					s.StartAgent(b.ID, "")
					if agentRunsInBackground(b.RawInput) {
						s.MarkAgentBackground(b.ID)
					}
					continue
				}
				broadcast(ServerMsg{Type: "tool_use", SessionID: s.ID, Tool: b.Name, ToolUseID: b.ID, Cwd: s.GetCwd(), Input: protocol.ParseToolInput(b.Name, b.RawInput)})
				// Bash foreground: trigger bashstreamer by writing the signal file.
				if b.Name == "Bash" {
					handleBashToolUse(s, b.ID, b.RawInput)
				}
			}
		}
		if u := event.Message.Usage; u != nil {
			contextUsed := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens
			if contextUsed > 0 {
				s.StreamContext = true
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
		// ModelUsage is a map. Pick the lexicographically first key so
		// the name shown for a multi-model turn is stable rather than
		// dependent on Go's randomized map iteration.
		models := make([]string, 0, len(event.ModelUsage))
		for name := range event.ModelUsage {
			models = append(models, name)
		}
		sort.Strings(models)
		if len(models) > 0 {
			name := models[0]
			info := event.ModelUsage[name]
			if info.ContextWindow > 0 {
				cost.ContextWindow = info.ContextWindow
			}
			if name != "" {
				cost.ModelName = name
			}
		}
		if cost.TotalCostUSD > 0 || cost.ContextWindow > 0 {
			broadcast(ServerMsg{Type: "cost", SessionID: s.ID, Cost: cost})
		}
		// Turn done. The scan loop also signals turnDone on result.
		broadcast(ServerMsg{Type: "done", SessionID: s.ID})
		// The assistant stream is the primary context source on providers that
		// carry usage. Fall back to the JSONL scan only when the stream did not
		// report context this turn (providers whose stream omits usage).
		if !s.StreamContext {
			refreshTokenCount(s, broadcast)
		}
		s.StreamContext = false

	case "user":
		// Sub-agent user events: broadcast with AgentID so the render
		// routes them into the subagent-body container.
		if pid != "" {
			for _, b := range event.Message.Content.Blocks {
				if b.Type == "tool_result" {
					suppressed := s.RemoveSuppressed(b.ToolUseID)
					if len(b.Content.Images) > 0 {
						broadcast(ServerMsg{Type: "tool_result", SessionID: s.ID, ToolUseID: b.ToolUseID, Images: b.Content.Images, AgentID: pid})
						continue
					}
					if suppressed {
						continue
					}
					output := b.Content.String()
					if !isBoringResult(output) {
						broadcast(ServerMsg{Type: "tool_result", SessionID: s.ID, ToolUseID: b.ToolUseID, Output: output, AgentID: pid})
					}
				}
			}
			return
		}

		// Parent user events: broadcast normally
		for _, b := range event.Message.Content.Blocks {
			if b.Type == "tool_result" {
				suppressed := s.RemoveSuppressed(b.ToolUseID)
				stat := s.GetAgentStat(b.ToolUseID)

				// A background agent's tool_result is its launch
				// acknowledgement, not its result. Finalization is deferred
				// to the agent's task_notification, so drop it here and leave
				// the section in its running state.
				if stat != nil && stat.Background {
					continue
				}

				// Foreground Agent tool_results finalize the sub-agent. This
				// is the deterministic "no more events for this Agent" point.
				// FinishAgent and task_done fire here rather than at
				// task_notification.
				if stat != nil {
					output := b.Content.String()
					s.FinishAgent(b.ToolUseID)
					finished := ServerMsg{
						Type:      "subagent_finished",
						SessionID: s.ID,
						AgentID:   b.ToolUseID,
						Text:      output,
					}
					finished.TotalTokens = stat.TotalTokens
					finished.TotalToolUses = stat.ToolUses
					finished.DurationMs = stat.DurationMs
					finished.Description = stat.Description
					broadcast(finished)
					broadcast(ServerMsg{Type: "task_done", SessionID: s.ID, ToolUseID: b.ToolUseID})
					continue
				}

				// Always show images even for suppressed tools (e.g. Read on screenshots).
				if len(b.Content.Images) > 0 {
					broadcast(ServerMsg{Type: "tool_result", SessionID: s.ID, ToolUseID: b.ToolUseID, Images: b.Content.Images})
					continue
				}
				output := ""
				if !suppressed {
					out := b.Content.String()
					if !isBoringResult(out) {
						output = out
					}
				}
				// Always broadcast tool_result, with empty output when the
				// result is suppressed or boring, so the tool chip's spinner
				// is stripped on result arrival.
				broadcast(ServerMsg{Type: "tool_result", SessionID: s.ID, ToolUseID: b.ToolUseID, Output: output})
			}
		}

		// bg Bash completion: the CLI injects <task-notification> XML
		// into the next user prompt after a background task finishes.
		for _, m := range bgTaskNotificationPattern.FindAllStringSubmatch(event.Message.Content.Text, -1) {
			toolUseID := m[1]
			status := m[2]
			if status == "completed" {
				broadcast(ServerMsg{Type: "task_done", SessionID: s.ID, ToolUseID: toolUseID})
				s.CloseBgStop(toolUseID)
			}
		}
	}
}

// handleBashToolUse triggers bashstreamer for a foreground Bash tool_use
// by writing the signal file. Background Bash is skipped.
func handleBashToolUse(s *Session, toolUseID string, rawInput json.RawMessage) {
	if s.BashSignalPath == "" {
		return
	}
	var input struct {
		Command         string `json:"command"`
		RunInBackground *bool  `json:"run_in_background,omitempty"`
	}
	if err := json.Unmarshal(rawInput, &input); err != nil || input.Command == "" {
		return
	}
	if input.RunInBackground != nil && *input.RunInBackground {
		return
	}
	s.WaitForBashConnected(2 * time.Second)
	s.StoreOutstandingBash(toolUseID, input.Command)
	if err := os.WriteFile(s.BashSignalPath, []byte(s.ID+" "+toolUseID), 0o644); err != nil {
		log.Printf("[bs] write signal file for tool %s: %v", toolUseID, err)
	}
}

// agentRunsInBackground reports whether an Agent tool_use input requested
// run_in_background. The flag decides whether the section finalizes at the
// parent tool_result (foreground) or at the agent's task_notification
// (background).
func agentRunsInBackground(rawInput json.RawMessage) bool {
	var input struct {
		RunInBackground *bool `json:"run_in_background,omitempty"`
	}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return false
	}
	return input.RunInBackground != nil && *input.RunInBackground
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}

func refreshTokenCount(s *Session, broadcast func(ServerMsg)) {
	go func() {
		jsonlPath := s.JSONLPath
		if jsonlPath == "" {
			jsonlPath = FindJSONLByClaudeID(s.ID)
			if jsonlPath != "" {
				s.JSONLPath = jsonlPath
			}
		}
		if jsonlPath == "" {
			return
		}
		used, window, modelName, err := scanTokenUsage(jsonlPath)
		if err != nil || (used == 0 && window == 0 && modelName == "") {
			time.Sleep(2 * time.Second)
			used, window, modelName, err = scanTokenUsage(jsonlPath)
		}
		if err != nil {
			return
		}
		// If the session was closed while we slept, the cost event would
		// land on a new session loaded from disk with the same ID.
		if s.ctx.Err() != nil {
			return
		}
		cost := &CostInfo{}
		changed := false
		if used > 0 && used != s.CostAccum.ContextUsed {
			cost.ContextUsed = used
			changed = true
		}
		if window > 0 && window != s.CostAccum.ContextWindow {
			cost.ContextWindow = window
			changed = true
		}
		if modelName != "" && modelName != s.CostAccum.ModelName {
			cost.ModelName = modelName
			changed = true
		}
		if changed {
			broadcast(ServerMsg{Type: "cost", SessionID: s.ID, Cost: cost})
		}
	}()
}
