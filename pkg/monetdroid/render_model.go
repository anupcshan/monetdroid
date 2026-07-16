package monetdroid

import (
	"fmt"
	"strings"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
	"github.com/anupcshan/monetdroid/pkg/monetdroid/render"
)

// DOMCmd is an alias for render.Cmd. See the render package for documentation.
type DOMCmd = render.Cmd

// RenderFull produces all DOM commands for a full page render. Callers should
// register bg paths and commands on the session from the model before
// calling this.
func RenderFull(m *SessionModel, sessionID string, reviewCount int) []DOMCmd {
	var cmds []DOMCmd

	// --- Chrome ---
	label := ShortPath(m.Cwd)
	if m.Label != "" {
		label = m.Label
		if m.AutoLabel {
			label = "(auto) " + label
		}
	}
	cmds = append(cmds,
		DOMCmd{Target: "session-label", Strategy: "innerHTML", Content: Esc(label)},
		DOMCmd{Target: "session-id", Strategy: "outerHTML",
			Content: fmt.Sprintf(`<input type="hidden" name="session_id" id="session-id" value="%s">`, Esc(sessionID))},
		DOMCmd{Target: "close-btn", Strategy: "outerHTML",
			Content: `<form id="close-btn" hx-post="/close" hx-swap="none" hx-include="#session-id"><button class="header-btn" type="submit" title="Close session">✕</button></form>`},
	)

	cmds = append(cmds, titleCmd(label)...)
	cmds = append(cmds, cwdCopyCmd(m.Cwd)...)
	cmds = append(cmds, activeCmds(m.HasActivity(), m.CanInterrupt())...)
	cmds = append(cmds, costBarCmd(sessionID, m.Cwd, m.Cost, m.DiffStat)...)
	cmds = append(cmds, modeBarCmd(sessionID, m.PermMode)...)
	cmds = append(cmds, todoCmds(m.Todos)...)
	cmds = append(cmds, queueBarCmd(sessionID, m.QueuedText)...)
	cmds = append(cmds, reviewBarCmd(sessionID, reviewCount)...)

	// --- Messages ---
	msgsHTML := renderModelMessages(m, sessionID)
	cmds = append(cmds, DOMCmd{Target: "msg-content", Strategy: "innerHTML", Content: msgsHTML})

	return cmds
}

// RenderEvent produces DOM commands for a single live event, driven by
// model state. It replaces the per-message OOB-swap logic in Broadcast.
// Returns nil if the event requires no DOM updates.
func RenderEvent(m *SessionModel, msg ServerMsg, sessionID string) []DOMCmd {
	switch msg.Type {
	case "running":
		return streamingClearCmds()

	case "done":
		return []DOMCmd{
			{Target: "thinking", Strategy: "innerHTML", Content: ""},
			{Target: "streaming", Strategy: "innerHTML", Content: ""},
			{Target: "cost-bar", Strategy: "innerHTML", Content: RenderCostBarModel(sessionID, m)},
		}

	case "cost":
		if msg.Cost != nil {
			return []DOMCmd{{Target: "cost-bar", Strategy: "innerHTML", Content: RenderCostBarModel(sessionID, m)}}
		}

	case "tool_use":
		rendered := RenderMsg(msg)
		if rendered == "" {
			return nil
		}
		// Sub-agent inner events flow into the section body, not the main timeline.
		if msg.AgentID != "" {
			return []DOMCmd{{
				Target:   "subagent-body-" + msg.AgentID,
				Strategy: "beforeend",
				Content:  rendered,
			}}
		}
		if msg.Tool == "Bash" {
			// For Bash tools, inject the bg-slot placeholder. It will be
			// populated when the tool_result arrives (or was already
			// populated during replay).
			rendered = injectBgSlot(rendered, msg.ToolUseID)
		}
		return append(streamingClearCmds(), DOMCmd{
			Target:   "msg-content",
			Strategy: "beforeend",
			Content:  rendered,
		})

	case "tool_result":
		if msg.ToolUseID == "" {
			return nil
		}
		// Sub-agent inner tool_results flow into the section body.
		if msg.AgentID != "" {
			rendered := RenderMsg(msg)
			if rendered == "" {
				return nil
			}
			return []DOMCmd{{
				Target:   "subagent-body-" + msg.AgentID,
				Strategy: "beforeend",
				Content:  rendered,
			}}
		}
		var cmds []DOMCmd
		if bt, ok := m.BgTasks[msg.ToolUseID]; ok && bt.OutputPath != "" {
			// Background task: populate the bg-slot, keep the spinner.
			cmds = append(cmds, DOMCmd{
				Target:   "bg-slot-" + msg.ToolUseID,
				Strategy: "innerHTML",
				Content:  RenderBgSlot(sessionID, msg.ToolUseID),
			})
		} else {
			// Non-background task: strip the spinner.
			cmds = append(cmds, DOMCmd{
				Target:   "spinner-" + msg.ToolUseID,
				Strategy: "outerHTML",
				Content:  "",
			})
			// Nest the result inside the tool chip's result-slot.
			if inner := RenderToolResultInner(msg); inner != "" {
				cmds = append(cmds, DOMCmd{
					Target:   "tool-result-slot-" + msg.ToolUseID,
					Strategy: "innerHTML",
					Content:  inner,
				})
			}
		}
		return cmds

	case "task_done":
		if msg.ToolUseID == "" {
			return nil
		}
		return []DOMCmd{{
			Target:   "spinner-" + msg.ToolUseID,
			Strategy: "outerHTML",
			Content:  "",
		}}

	case "text":
		rendered := RenderMsg(msg)
		if rendered == "" {
			return nil
		}
		return append(streamingClearCmds(), DOMCmd{
			Target:   "msg-content",
			Strategy: "beforeend",
			Content:  rendered,
		})

	case "thinking":
		rendered := RenderMsg(msg)
		if rendered == "" {
			return nil
		}
		return append(streamingClearCmds(), DOMCmd{
			Target:   "msg-content",
			Strategy: "beforeend",
			Content:  rendered,
		})

	case "permission_request":
		if msg.ToolUseID == "" {
			return nil
		}
		// AskUserQuestion renders as a standalone permission chip, not inline.
		if msg.PermTool == "AskUserQuestion" {
			rendered := RenderPermission(msg)
			if rendered == "" {
				return nil
			}
			return []DOMCmd{{
				Target:   "msg-content",
				Strategy: "beforeend",
				Content:  rendered,
			}}
		}
		rendered := RenderPermission(msg)
		if rendered == "" {
			return nil
		}
		// Inline permission goes into the tool chip's perm-slot.
		return []DOMCmd{{
			Target:   "perm-slot-" + msg.ToolUseID,
			Strategy: "innerHTML",
			Content:  RenderInlinePermission(msg),
		}}

	case "permission_mode":
		return modeBarCmd(sessionID, msg.PermMode)

	case "subagent_started":
		rendered := RenderSubagentSection(msg.AgentID, msg.AgentType, nil)
		if rendered == "" {
			return nil
		}
		return []DOMCmd{{
			Target:   "msg-content",
			Strategy: "beforeend",
			Content:  rendered,
		}}

	case "subagent_linked":
		if msg.AgentID == "" {
			return nil
		}
		var cmds []DOMCmd
		if st, ok := m.SubagentSections[msg.AgentID]; ok {
			heading := linkedSubagentHeading(msg.Description, msg.AgentID)
			cmds = append(cmds,
				DOMCmd{Target: "subagent-heading-" + msg.AgentID, Strategy: "innerHTML", Content: heading},
				DOMCmd{Target: "subagent-stats-" + msg.AgentID, Strategy: "innerHTML",
					Content: renderSubagentStats(msg.TotalTokens, msg.TotalToolUses, msg.DurationMs)},
			)
			if msg.Text != "" {
				cmds = append(cmds, DOMCmd{Target: "subagent-final-" + msg.AgentID, Strategy: "innerHTML",
					Content: fmt.Sprintf(`<div class="msg msg-assistant"><div class="msg-bubble">%s</div></div>`,
						RenderMarkdown(msg.Text))})
			}
			_ = st // confirm section exists in the map
		}
		cmds = append(cmds, DOMCmd{
			Target:   "subagent-spinner-" + msg.AgentID,
			Strategy: "outerHTML",
			Content:  "",
		})
		return cmds

	case "subagent_stopped":
		if msg.AgentID == "" {
			return nil
		}
		return []DOMCmd{{
			Target:   "subagent-spinner-" + msg.AgentID,
			Strategy: "outerHTML",
			Content:  "",
		}}

	case "user_message":
		rendered := RenderMsg(msg)
		if rendered == "" {
			return nil
		}
		return []DOMCmd{{
			Target:   "msg-content",
			Strategy: "beforeend",
			Content:  rendered,
		}}

	case "error":
		rendered := RenderMsg(msg)
		if rendered == "" {
			return nil
		}
		return []DOMCmd{{
			Target:   "msg-content",
			Strategy: "beforeend",
			Content:  rendered,
		}}
	}
	return nil
}

// --- Helpers ---

func streamingClearCmds() []DOMCmd {
	return []DOMCmd{
		{Target: "streaming", Strategy: "innerHTML", Content: ""},
	}
}

func activeCmds(active, canStop bool) []DOMCmd {
	var cmds []DOMCmd
	if active {
		cmds = append(cmds,
			DOMCmd{Target: "running-dot", Strategy: "outerHTML",
				Content: `<span class="di-running" id="running-dot"></span>`},
			DOMCmd{Target: "thinking", Strategy: "innerHTML", Content: `<span></span><span></span><span></span>`},
		)
	} else {
		cmds = append(cmds,
			DOMCmd{Target: "running-dot", Strategy: "outerHTML",
				Content: `<span id="running-dot" style="display:none"></span>`},
		)
	}
	if canStop {
		cmds = append(cmds,
			DOMCmd{Target: "stop-btn", Strategy: "outerHTML",
				Content: `<button class="stop-btn" id="stop-btn" hx-post="/stop" hx-swap="none" hx-include="#session-id">◼</button>`},
		)
	} else {
		cmds = append(cmds,
			DOMCmd{Target: "stop-btn", Strategy: "outerHTML",
				Content: `<span id="stop-btn"></span>`},
		)
	}
	return cmds
}

func costBarCmd(sessionID, cwd string, cost CostInfo, ds DiffStat) []DOMCmd {
	return []DOMCmd{{Target: "cost-bar", Strategy: "innerHTML", Content: renderCostBarModel(sessionID, cwd, cost, ds)}}
}

// modeOptions is the set of permission modes surfaced in the mode picker, in
// display order. Each becomes one entry in the popover. pkg/claude's parser is
// the authority on which strings are valid modes. This list is the curated
// subset the UI exposes.
var modeOptions = []struct {
	mode  claude.PermissionMode
	label string
	desc  string
}{
	{claude.PermDefault, "Default", "Ask unless already allowed or denied"},
	{claude.PermAcceptEdits, "Accept edits", "Auto-approve edits, ask for the rest"},
	{claude.PermAuto, "Auto", "Classifier auto-approves safe actions, blocks risky ones"},
}

// modeLabel returns the UI label for a permission mode. Falls back to the raw
// mode string for anything outside modeOptions.
func modeLabel(mode claude.PermissionMode) string {
	for _, o := range modeOptions {
		if o.mode == mode {
			return o.label
		}
	}
	return string(mode)
}

func modeBarCmd(sessionID string, mode claude.PermissionMode) []DOMCmd {
	return []DOMCmd{{Target: "mode-bar", Strategy: "innerHTML", Content: renderModeBar(sessionID, mode)}}
}

func renderModeBar(sessionID string, mode claude.PermissionMode) string {
	// Sessions resumed from history carry no stored permission mode. The CLI
	// process always starts in default, so an unset mode is effectively default.
	if mode == "" {
		mode = claude.PermDefault
	}
	triggerClass := "mode-trigger"
	if mode != claude.PermDefault {
		triggerClass += " mode-trigger-active"
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<button type="button" class="%s" popovertarget="mode-picker-popover">Mode: %s ▾</button>`, triggerClass, Esc(modeLabel(mode)))
	b.WriteString(`<div popover id="mode-picker-popover" class="mode-picker"><div class="mode-picker-label">Permission mode</div>`)
	for _, o := range modeOptions {
		b.WriteString(renderModeOption(sessionID, o.mode, o.label, o.desc, mode))
	}
	b.WriteString(`</div>`)
	return b.String()
}

func renderModeOption(sessionID string, m claude.PermissionMode, label, desc string, current claude.PermissionMode) string {
	mark := ""
	if m == current {
		mark = " ✓"
	}
	inner := fmt.Sprintf(`<span class="mode-option-name">%s%s</span><span class="mode-option-desc">%s</span>`, Esc(label), mark, Esc(desc))
	if m == current {
		return fmt.Sprintf(`<div class="mode-option mode-option-current">%s</div>`, inner)
	}
	return fmt.Sprintf(`<form class="mode-option-form" data-mode="%s" hx-post="/mode" hx-swap="none" hx-on::after-request="document.getElementById('mode-picker-popover').hidePopover()"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="mode" value="%s"><button type="submit" class="mode-option">%s</button></form>`, Esc(string(m)), Esc(sessionID), Esc(string(m)), inner)
}

func todoCmds(todos []protocol.Todo) []DOMCmd {
	return []DOMCmd{
		{Target: "todos-summary", Strategy: "innerHTML", Content: RenderTodosSummary(todos)},
		{Target: "todos-body", Strategy: "innerHTML", Content: RenderTodosBody(todos)},
	}
}

func queueBarCmd(_ /* sessionID */, queuedText string) []DOMCmd {
	if queuedText == "" {
		return []DOMCmd{{Target: "queue-bar", Strategy: "innerHTML", Content: ""}}
	}
	escaped := Esc(queuedText)
	return []DOMCmd{{Target: "queue-bar", Strategy: "innerHTML", Content: fmt.Sprintf(
		`<div class="queue-bar" id="queue-bar"><span>Queued message:</span><span class="queue-text">%s</span><button hx-post="/dequeue" hx-swap="none" hx-include="#session-id">Cancel</button></div>`,
		escaped,
	)}}
}

func reviewBarCmd(sessionID string, count int) []DOMCmd {
	barHTML := RenderReviewBar(sessionID, count)
	return []DOMCmd{{Target: "review-bar", Strategy: "outerHTML", Content: barHTML}}
}

func titleCmd(label string) []DOMCmd {
	if label == "" {
		return nil
	}
	return []DOMCmd{
		{Target: "page-title", Strategy: "innerHTML", Content: fmt.Sprintf(`<script>document.title=%q</script>`, label)},
	}
}

func cwdCopyCmd(cwd string) []DOMCmd {
	return []DOMCmd{
		{Target: "session-cwd", Strategy: "outerHTML",
			Content: fmt.Sprintf(`<input type="hidden" name="cwd" id="session-cwd" value="%s">`, Esc(cwd))},
		{Target: "cwd-row", Strategy: "innerHTML",
			Content: fmt.Sprintf(`<span class="cwd-text">%s</span><button class="cwd-copy" onclick="navigator.clipboard.writeText(this.previousElementSibling.textContent)">📋</button>`, Esc(ShortPath(cwd)))},
		{Target: "cwd-copy", Strategy: "outerHTML",
			Content: `<button class="header-btn" id="cwd-copy" hx-on:click="this.closest('.header').classList.toggle('show-cwd')">📁</button>`},
	}
}

// RenderCostBarModel renders the cost bar from model state.
func RenderCostBarModel(sessionID string, m *SessionModel) string {
	return renderCostBarModel(sessionID, m.Cwd, m.Cost, m.DiffStat)
}

// FormatSSEDOM converts DOM commands to the SSE wire format.
func FormatSSEDOM(cmds []DOMCmd, extraOOBs ...string) string {
	return render.Format(cmds, extraOOBs...)
}

// renderCostBarModel is a session-less cost bar render.
func renderCostBarModel(sessionID, cwd string, c CostInfo, ds DiffStat) string {
	var parts []string
	if c.TotalCostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", c.TotalCostUSD))
	}
	if c.ContextUsed > 0 && c.ContextWindow > 0 {
		pct := 100 * c.ContextUsed / c.ContextWindow
		parts = append(parts, fmt.Sprintf("%s/%s (%d%%)", FmtK(c.ContextUsed), FmtK(c.ContextWindow), pct))
	} else if c.ContextUsed > 0 {
		parts = append(parts, FmtK(c.ContextUsed))
	}
	if c.ModelName != "" {
		parts = append(parts, ShortModelName(c.ModelName))
	}
	parts = append(parts, RenderDiffStat(sessionID, ds))
	parts = append(parts, fmt.Sprintf(`<a href="/kb/?cwd=%s" class="diff-stat-link" style="color:var(--text2)">KB</a>`, Esc(cwd)))
	return strings.Join(parts, " · ")
}

// injectBgSlot adds the bg-slot placeholder div into a Bash tool chip's HTML.
func injectBgSlot(html, toolUseID string) string {
	slot := fmt.Sprintf(`<div id="bg-slot-%s"></div>`, Esc(toolUseID))
	if strings.Contains(html, slot) {
		return html
	}
	// Append before the closing </details> tag.
	idx := strings.LastIndex(html, "</details>")
	if idx < 0 {
		return html
	}
	return html[:idx] + slot + html[idx:]
}

// renderModelMessages renders the message list from model state, matching
// the output of renderMessages. Paginated to the last 100 messages.
func renderModelMessages(m *SessionModel, sessionID string) string {
	rc := buildRenderContext(m)

	const pageSize = 100
	start := 0
	log_ := m.Messages
	if len(log_) > pageSize {
		start = len(log_) - pageSize
		if rc.lastCompact >= 0 && start <= rc.lastCompact {
			start = 0
		}
	}

	var b strings.Builder
	if start > 0 {
		b.WriteString(renderSentinel(sessionID, start))
	}
	b.WriteString(renderMessages(log_, start, len(log_), rc, sessionID))
	return b.String()
}

// buildRenderContext creates a renderContext from the model, bridging the
// new model type to the existing renderMessages function.
func buildRenderContext(m *SessionModel) renderContext {
	bgTaskResults := make(map[string]string)
	for id, bt := range m.BgTasks {
		if bt.OutputPath != "" {
			bgTaskResults[id] = bt.OutputPath
		}
	}

	subagentSections := make(map[string]*subagentRenderState)
	for agentID, s := range m.SubagentSections {
		st := &subagentRenderState{Section: &SubagentSection{
			AgentID:         s.AgentID,
			AgentType:       s.AgentType,
			Linked:          s.Linked,
			ParentToolUseID: s.ParentToolUseID,
			Description:     s.Description,
			FinalText:       s.FinalText,
			TotalTokens:     s.TotalTokens,
			TotalToolUses:   s.TotalToolUses,
			DurationMs:      s.DurationMs,
			Stopped:         s.Stopped,
		}}
		subagentSections[agentID] = st
	}

	// Populate each section's inner tool stream from the log. The model tracks
	// section metadata but not inner events, so scan the log the same way the
	// log-based replay path (precomputeRenderContext) does.
	for _, msg := range m.Messages {
		if msg.AgentID == "" {
			continue
		}
		st := subagentSections[msg.AgentID]
		if st == nil {
			continue
		}
		if msg.Type == "tool_use" || msg.Type == "tool_result" {
			st.InnerEvents = append(st.InnerEvents, msg)
		}
	}

	return renderContext{
		lastCompact:       m.LastCompact,
		toolResults:       m.ToolResults,
		toolUseIndexes:    m.ToolUseIndexes,
		toolResultIndexes: m.ToolResultIndexes,
		bgTasks:           m.BgTasks,
		bgTaskResults:     bgTaskResults,
		suppressedIDs:     m.SuppressedIDs,
		pendingPerms:      m.PendingPerms,
		subagentSections:  subagentSections,
	}
}
