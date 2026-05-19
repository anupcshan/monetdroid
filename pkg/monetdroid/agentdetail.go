package monetdroid

import (
	"fmt"
	"net/url"
	"strings"
)

// RenderAgentSlot returns a lazy-load trigger for agent detail output.
// The actual content is loaded when the user opens the tool chip's <details>.
func RenderAgentSlot(sessionID, toolUseID string) string {
	return fmt.Sprintf(
		`<div class="agent-detail-slot" id="agent-slot-%s" `+
			`hx-get="/agent-detail/connect?session=%s&tool_id=%s" `+
			`hx-trigger="revealed once" hx-swap="innerHTML"></div>`,
		Esc(toolUseID), url.QueryEscape(sessionID), url.QueryEscape(toolUseID))
}

// RenderAgentSSEDiv returns the SSE-connected div that streams agent detail
// events starting from the given buffer offset. The connect handler captures
// the offset of events it has already rendered as static HTML and passes
// that length here; the stream endpoint resumes from exactly that point so
// no event falls into the gap between the connect response and the SSE
// handler starting.
func RenderAgentSSEDiv(sessionID, toolUseID string, fromOffset int) string {
	return fmt.Sprintf(
		`<div hx-ext="sse" `+
			`sse-connect="/agent-detail/stream?session=%s&tool_id=%s&from=%d" `+
			`sse-swap="event" hx-swap="beforeend" sse-close="done"></div>`,
		url.QueryEscape(sessionID), url.QueryEscape(toolUseID), fromOffset)
}

// RenderAgentStatHTML returns the inline stats display for an agent chip.
func RenderAgentStatHTML(stat *AgentStat) string {
	if stat == nil {
		return ""
	}
	var parts []string
	if stat.TotalTokens > 0 {
		parts = append(parts, FmtK(stat.TotalTokens)+" tokens")
	}
	if stat.ToolUses > 0 {
		parts = append(parts, fmt.Sprintf("%d tools", stat.ToolUses))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// RenderAgentDetail renders all buffered events for an agent as HTML.
// Reuses RenderMsg for each event — no agent-specific rendering.
func RenderAgentDetail(events []ServerMsg) string {
	var b strings.Builder
	for _, msg := range events {
		rendered := RenderMsg(msg)
		if rendered != "" {
			b.WriteString(rendered)
		}
	}
	return b.String()
}
