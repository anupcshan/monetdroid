package monetdroid

import (
	"fmt"
	"strings"
)

// RenderSubagentSection renders the outer block for a sub-agent section. It
// is rendered once at SubagentStart with empty body and spinner running.
// The body fills in live via OOB swaps as inner tool_use/tool_result events
// arrive. At link time the heading and stats line update via OOB swaps.
//
// section may be nil during the initial live broadcast (no link info yet).
// On replay, section is populated with whatever final state was reached. The
// spinner span is omitted entirely once the section is linked or stopped,
// matching the live OOB swap that sets outerHTML="" on those transitions.
func RenderSubagentSection(agentID, agentType string, section *SubagentSection) string {
	heading := defaultSubagentHeading(agentID, agentType)
	stats := ""
	finalTextHTML := ""
	spinnerHTML := fmt.Sprintf(
		` <span class="tool-spinner" id="subagent-spinner-%s"><span class="spinner-dots"><span></span><span></span><span></span></span></span>`,
		Esc(agentID))
	if section != nil {
		if section.Linked {
			heading = linkedSubagentHeading(section.Description, agentID)
			stats = renderSubagentStats(section.TotalTokens, section.TotalToolUses, section.DurationMs)
		}
		if section.FinalText != "" {
			finalTextHTML = fmt.Sprintf(`<div class="msg msg-assistant"><div class="msg-bubble">%s</div></div>`,
				RenderMarkdown(section.FinalText))
		}
		if section.Stopped || section.Linked {
			spinnerHTML = ""
		}
	}
	return fmt.Sprintf(
		`<div class="msg msg-subagent" id="subagent-section-%s">`+
			`<details class="tool-chip">`+
			`<summary class="tool-name">⚙ <span class="subagent-heading" id="subagent-heading-%s">%s</span>%s<span class="agent-stats" id="subagent-stats-%s">%s</span></summary>`+
			`<div class="tool-detail">`+
			`<div class="subagent-body" id="subagent-body-%s"></div>`+
			`<div class="subagent-final" id="subagent-final-%s">%s</div>`+
			`</div>`+
			`</details>`+
			`</div>`,
		Esc(agentID),
		Esc(agentID), heading, spinnerHTML,
		Esc(agentID), stats,
		Esc(agentID),
		Esc(agentID), finalTextHTML,
	)
}

func defaultSubagentHeading(agentID, agentType string) string {
	if agentType != "" {
		return fmt.Sprintf("subagent %s (%s)", Esc(agentType), Esc(agentID))
	}
	return fmt.Sprintf("subagent (%s)", Esc(agentID))
}

func linkedSubagentHeading(description, agentID string) string {
	if description != "" {
		return fmt.Sprintf("Agent: %s", Esc(description))
	}
	return fmt.Sprintf("Agent (%s)", Esc(agentID))
}

func renderSubagentStats(tokens, toolUses, durationMs int) string {
	var parts []string
	if tokens > 0 {
		parts = append(parts, FmtK(tokens)+" tokens")
	}
	if toolUses > 0 {
		parts = append(parts, fmt.Sprintf("%d tools", toolUses))
	}
	if durationMs > 0 {
		parts = append(parts, fmt.Sprintf("%ds", (durationMs+500)/1000))
	}
	return strings.Join(parts, " . ")
}

// RenderSubagentChip renders an inner tool_use chip for a sub-agent. The
// markup is intentionally lightweight: no permission slots, no Bash bg
// slot, no Agent-specific machinery. These chips render inside the
// section's body container.
func RenderSubagentChip(msg ServerMsg) string {
	if msg.Tool == "TodoWrite" || msg.Tool == "TaskCreate" || msg.Tool == "TaskUpdate" {
		return ""
	}
	summary := ToolChipSummary(msg.Tool, msg.Input)
	detail := FormatToolInput(msg.Tool, msg.Input)
	resultSlot := fmt.Sprintf(`<div class="tool-result-content" id="tool-result-slot-%s"></div>`, Esc(msg.ToolUseID))
	return fmt.Sprintf(
		`<div class="msg msg-tool" id="tool-%s"><details class="tool-chip"><summary class="tool-name">⚙ %s</summary><div class="tool-detail">%s</div>%s</details></div>`,
		Esc(msg.ToolUseID), Esc(summary), Esc(detail), resultSlot)
}

// RenderSubagentToolResult renders the tool_result text for an inner
// sub-agent tool. Mirrors the main-stream tool_result chip.
func RenderSubagentToolResult(msg ServerMsg) string {
	if len(msg.Images) > 0 {
		return ""
	}
	return fmt.Sprintf(
		`<div class="msg msg-tool"><details class="tool-result-chip"><summary class="tool-result-summary">result</summary><div class="tool-result-full">%s</div></details></div>`,
		Esc(msg.Output))
}

// renderFinalSubagentSection emits the section block with its inner chips
// inlined and its final state (link, stats, stopped). Used on replay so the
// initial server-rendered HTML matches what live OOB swaps would have built.
//
// Inner tool_results are nested into their matching tool_use chip's
// result-slot (same shape as the main timeline). Orphan results whose
// tool_use is absent from the section fall back to the standalone chip.
func renderFinalSubagentSection(st *subagentRenderState) string {
	base := RenderSubagentSection(st.Section.AgentID, st.Section.AgentType, st.Section)
	if len(st.InnerEvents) == 0 {
		return base
	}

	toolUseIDs := make(map[string]bool)
	toolResults := make(map[string]ServerMsg)
	for _, msg := range st.InnerEvents {
		if msg.Type == "tool_use" && msg.ToolUseID != "" {
			toolUseIDs[msg.ToolUseID] = true
		}
		if msg.Type == "tool_result" && msg.ToolUseID != "" {
			toolResults[msg.ToolUseID] = msg
		}
	}

	var body strings.Builder
	for _, msg := range st.InnerEvents {
		if msg.Type == "tool_result" && msg.ToolUseID != "" && toolUseIDs[msg.ToolUseID] {
			continue
		}
		rendered := RenderMsg(msg)
		if msg.Type == "tool_use" && msg.ToolUseID != "" {
			if result, ok := toolResults[msg.ToolUseID]; ok {
				if inner := RenderToolResultInner(result); inner != "" {
					emptySlot := fmt.Sprintf(`<div class="tool-result-content" id="tool-result-slot-%s"></div>`, Esc(msg.ToolUseID))
					filledSlot := fmt.Sprintf(`<div class="tool-result-content" id="tool-result-slot-%s">%s</div>`, Esc(msg.ToolUseID), inner)
					rendered = strings.Replace(rendered, emptySlot, filledSlot, 1)
				}
			}
		}
		body.WriteString(rendered)
	}
	emptyBody := fmt.Sprintf(`<div class="subagent-body" id="subagent-body-%s"></div>`, Esc(st.Section.AgentID))
	filledBody := fmt.Sprintf(`<div class="subagent-body" id="subagent-body-%s">%s</div>`, Esc(st.Section.AgentID), body.String())
	return strings.Replace(base, emptyBody, filledBody, 1)
}
