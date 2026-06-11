package monetdroid

import (
	"encoding/json"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude"
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

// handleStreamEvent processes non-control messages from the CLI and broadcasts them.
//
// Event flows by category:
//
//   - Agent tasks: task_started / task_progress / task_notification arrive as
//     stream JSON system events. task_notification fires for agent tasks only,
//     not for background Bash.
//
//   - Background Bash tasks: creation arrives via hooks (PreToolUse →
//     PostToolUse with backgroundTaskId → PostToolBatch with "Output is
//     being written to:"). Completion has no dedicated event; the signal is
//     <task-notification> XML injected into the next UserPromptSubmit prompt
//     text, parsed by the "user" case below.
//
//   - Tool lifecycle: tool_use flows from hookToStreamEvents (PreToolUse hook).
//     tool_result flows from hookToStreamEvents (PostToolBatch hook) or from
//     stream JSON user events (for stdout-side non-hook paths). Agent tool
//     results are routed through handleHookEvent (PostToolUse for Agent) and
//     never reach this function.
//
//   - Assistant text/thinking: "assistant" stream events carry text and
//     thinking blocks. Streaming deltas arrive as stream_event raw events
//     (routed through handleRawStreamEvent). MessageDisplay hooks carry
//     final display text and are not used for streaming.
//
// Sub-agent events (parent_tool_use_id non-empty) never reach here: the
// process.go scan filter drops sidechain stdout events, and inner sub-agent
// hooks are dispatched through handleHookEvent.
func handleStreamEvent(s *Session, event *protocol.StreamEvent, broadcast func(ServerMsg)) {
	switch event.Type {
	case "system":
		switch event.Subtype {
		case "task_started":
			if event.TaskType == "local_agent" && event.ToolUseID != "" {
				s.StartAgent(event.ToolUseID, event.Description)
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
				// Background Bash tasks aren't tracked as agents (no
				// task_started arrival), so they finalize here.
				// Agent tasks finalize at parent's PostToolUse for Agent
				// (routed via handleHookEvent), which is the only payload
				// pairing the sub-agent's agent_id with the parent
				// tool_use_id.
				if s.GetAgentStat(event.ToolUseID) == nil {
					broadcast(ServerMsg{Type: "task_done", SessionID: s.ID, ToolUseID: event.ToolUseID})
				}
				s.CloseBgStop(event.ToolUseID)
			}
		}

	case "assistant":
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
				// Parent Agent tool_use blocks are filtered out at the hook
				// layer (see hookToStreamEvents PreToolUse). Sub-agent
				// sections render in the main timeline instead.
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
		for _, b := range event.Message.Content.Blocks {
			if b.Type == "tool_result" {
				suppressed := s.RemoveSuppressed(b.ToolUseID)

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
				// Always broadcast tool_result (empty output when suppressed/boring)
				// so the tool chip's spinner is stripped on result arrival.
				broadcast(ServerMsg{Type: "tool_result", SessionID: s.ID, ToolUseID: b.ToolUseID, Output: output})
			}
		}

		// bg Bash completion: the CLI injects <task-notification> XML
		// into the next user prompt after a background task finishes.
		// Example: <task-notification><tool-use-id>call_01_AbC</tool-use-id>
		// <status>completed</status>...</task-notification>
		// task_notification stream events (system type) do NOT fire for
		// bg Bash. The Stop hook's background_tasks list is a snapshot
		// only and does not signal completion. task_done broadcast here
		// updates the model (marking the BgTask completed) and removes
		// the tool chip spinner via RenderEvent.
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

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}

// hookEnvelope holds the common fields decoded from every hook payload.
// Type-specific fields are decoded by the individual handler from ev.Body.
type hookEnvelope struct {
	EventName string `json:"hook_event_name"`
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
}

// handleHookEvent routes raw hook payloads received via OnHookEvent. Two
// flows live here:
//
//  1. Parent-side Agent bookkeeping (agent_id empty, tool_name == "Agent"):
//     PreToolUse stashes the description and marks the parent's tool_use_id
//     suppressed so the eventual Agent tool_result is hidden from the main
//     stream. PostToolUse broadcasts subagent_linked, renaming the section
//     heading and filling in totals + final text. Parent's PostToolUse is
//     also the deterministic completion signal for the Agent invocation in
//     AgentStats, so FinishAgent + task_done fire here.
//
//  2. Sub-agent inner events (agent_id non-empty): SubagentStart creates the
//     section and broadcasts subagent_started. PreToolUse and PostToolBatch
//     broadcast inner tool_use / tool_result chips tagged with agent_id.
//     SubagentStop broadcasts subagent_stopped to clear the section spinner.
func handleHookEvent(s *Session, ev claude.HookEvent, broadcast func(ServerMsg)) {
	var env hookEnvelope
	if err := json.Unmarshal(ev.Body, &env); err != nil {
		log.Printf("[hook] handleHookEvent envelope: %v", err)
		return
	}
	if env.EventName == "SessionStart" {
		type sessionStartPayload struct {
			TranscriptPath string `json:"transcript_path"`
			Model          string `json:"model"`
		}
		var p sessionStartPayload
		if err := json.Unmarshal(ev.Body, &p); err != nil {
			log.Printf("[hook] SessionStart parse error: %v", err)
		} else {
			if p.TranscriptPath != "" {
				s.JSONLPath = p.TranscriptPath
			}
			if p.Model != "" {
				s.AccumulateCost(&CostInfo{ModelName: p.Model})
				broadcast(ServerMsg{Type: "cost", SessionID: s.ID, Cost: &CostInfo{ModelName: p.Model}})
			}
		}
		broadcast(ServerMsg{Type: "session_started", SessionID: s.ID})
	}

	// UserPromptSubmit signals a turn start. Broadcast "running" so the
	// model sets turnActive. On the new-session path, StartTurn also
	// broadcasts "running" (the model does not yet exist when hooks are
	// replayed). The duplicate is harmless: Apply is idempotent.
	if env.EventName == "UserPromptSubmit" {
		broadcast(ServerMsg{Type: "running", SessionID: s.ID})
	}

	// Stop/StopFailure signal a completed turn. Broadcast "done" so the
	// model clears turnActive. Duplicate with waitAndDrainLoop's explicit
	// broadcast is harmless (Apply is idempotent for turnActive).
	// The JSONL file has been updated with the final assistant usage.
	if env.EventName == "Stop" || env.EventName == "StopFailure" {
		broadcast(ServerMsg{Type: "done", SessionID: s.ID})
		refreshTokenCount(s, broadcast)
	}

	if env.AgentID == "" {
		handleParentAgentHook(s, ev, env, broadcast)
		return
	}

	handleSubagentHook(s, ev, env, broadcast)
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

func handleParentAgentHook(s *Session, ev claude.HookEvent, env hookEnvelope, broadcast func(ServerMsg)) {
	if env.ToolName != "Agent" || env.ToolUseID == "" {
		return
	}
	switch env.EventName {
	case "PreToolUse":
		var p struct {
			ToolInput struct {
				Description  string `json:"description"`
				SubagentType string `json:"subagent_type"`
			} `json:"tool_input"`
		}
		if err := json.Unmarshal(ev.Body, &p); err != nil {
			log.Printf("[hook] parent PreToolUse Agent parse: %v", err)
			return
		}
		s.StashAgentDescription(env.ToolUseID, p.ToolInput.Description)
		// Suppress the eventual Agent tool_result in the main stream: its
		// final content arrives via subagent_linked instead.
		s.SuppressTool(env.ToolUseID, "Agent")

	case "PostToolUse":
		var p struct {
			ToolResponse struct {
				AgentID           string          `json:"agentId"`
				Content           json.RawMessage `json:"content"`
				TotalTokens       int             `json:"totalTokens"`
				TotalToolUseCount int             `json:"totalToolUseCount"`
				TotalDurationMs   int             `json:"totalDurationMs"`
			} `json:"tool_response"`
		}
		if err := json.Unmarshal(ev.Body, &p); err != nil {
			log.Printf("[hook] parent PostToolUse Agent parse: %v", err)
			return
		}
		agentID := p.ToolResponse.AgentID
		if agentID == "" {
			return
		}
		description := s.TakeAgentDescription(env.ToolUseID)
		finalText := decodeToolResponseText(p.ToolResponse.Content)
		s.LinkSubagent(agentID, env.ToolUseID, description, finalText,
			p.ToolResponse.TotalTokens, p.ToolResponse.TotalToolUseCount, p.ToolResponse.TotalDurationMs)
		broadcast(ServerMsg{
			Type:            "subagent_linked",
			SessionID:       s.ID,
			AgentID:         agentID,
			ParentToolUseID: env.ToolUseID,
			ToolUseID:       env.ToolUseID,
			Description:     description,
			Text:            finalText,
			TotalTokens:     p.ToolResponse.TotalTokens,
			TotalToolUses:   p.ToolResponse.TotalToolUseCount,
			DurationMs:      p.ToolResponse.TotalDurationMs,
		})
		// Finalize the Agent invocation in AgentStats. Stdout's
		// task_notification doesn't finalize local_agent tasks (its handler
		// keys off GetAgentStat); this is the deterministic terminator.
		if s.GetAgentStat(env.ToolUseID) != nil {
			s.FinishAgent(env.ToolUseID)
			broadcast(ServerMsg{Type: "task_done", SessionID: s.ID, ToolUseID: env.ToolUseID})
		}
	}
}

func handleSubagentHook(s *Session, ev claude.HookEvent, env hookEnvelope, broadcast func(ServerMsg)) {
	switch env.EventName {
	case "SubagentStart":
		s.StartSubagent(env.AgentID, env.AgentType)
		broadcast(ServerMsg{
			Type:      "subagent_started",
			SessionID: s.ID,
			AgentID:   env.AgentID,
			AgentType: env.AgentType,
		})

	case "SubagentStop":
		s.MarkSubagentStopped(env.AgentID)
		broadcast(ServerMsg{
			Type:      "subagent_stopped",
			SessionID: s.ID,
			AgentID:   env.AgentID,
		})

	case "PreToolUse":
		if env.ToolUseID == "" || env.ToolName == "" {
			return
		}
		var p struct {
			ToolInput json.RawMessage `json:"tool_input"`
		}
		if err := json.Unmarshal(ev.Body, &p); err != nil {
			log.Printf("[hook] sub-agent PreToolUse parse: %v", err)
			return
		}
		if suppressResultTools[env.ToolName] {
			s.SuppressTool(env.ToolUseID, env.ToolName)
		}
		broadcast(ServerMsg{
			Type:      "tool_use",
			SessionID: s.ID,
			AgentID:   env.AgentID,
			Tool:      env.ToolName,
			ToolUseID: env.ToolUseID,
			Input:     protocol.ParseToolInput(env.ToolName, p.ToolInput),
		})

	case "PostToolBatch":
		var p struct {
			ToolCalls []struct {
				ToolUseID       string          `json:"tool_use_id"`
				ToolResponseRaw json.RawMessage `json:"tool_response"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(ev.Body, &p); err != nil {
			log.Printf("[hook] sub-agent PostToolBatch parse: %v", err)
			return
		}
		for _, tc := range p.ToolCalls {
			output := decodeToolResponseText(tc.ToolResponseRaw)
			if s.RemoveSuppressed(tc.ToolUseID) {
				continue
			}
			if isBoringResult(output) {
				continue
			}
			broadcast(ServerMsg{
				Type:      "tool_result",
				SessionID: s.ID,
				AgentID:   env.AgentID,
				ToolUseID: tc.ToolUseID,
				Output:    output,
			})
		}
	}
}

// decodeToolResponseText extracts the rendered text from a tool_response
// payload, which per the Claude Code hooks doc (PostToolBatch section) is
// "a serialized string or content-block array". Falls back to the raw JSON
// bytes for any other shape.
func decodeToolResponseText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}
