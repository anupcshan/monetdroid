package monetdroid

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// SSEEvent is a rendered SSE event string tagged with a monotonic sequence number.
type SSEEvent struct {
	Seq        uint64
	Event      string
	CompactKey string // non-empty → newer event with same key supersedes this one
}

// EventLog is an append-only log of rendered SSE events for a session.
// Broadcast appends to it; replay reads from it. This ensures both paths
// produce identical output. Events with a CompactKey are deduplicated:
// when a new event arrives with the same key, the older one is removed.
type EventLog struct {
	mu     sync.Mutex
	events []SSEEvent
	seq    uint64
}

// compactKey returns a dedup key for single-element OOB swaps that use
// innerHTML or outerHTML. Multi-swap events and beforeend appends return "".
func compactKey(event string) string {
	if strings.Count(event, "hx-swap-oob=") != 1 {
		return ""
	}
	if !strings.Contains(event, `hx-swap-oob="innerHTML"`) &&
		!strings.Contains(event, `hx-swap-oob="outerHTML"`) {
		return ""
	}
	idx := strings.Index(event, `id="`)
	if idx < 0 {
		return ""
	}
	start := idx + 4
	end := strings.IndexByte(event[start:], '"')
	if end < 0 {
		return ""
	}
	return event[start : start+end]
}

// Append adds an event to the log and returns its sequence number.
// If the event is a single innerHTML/outerHTML OOB swap, any previous
// event targeting the same element is removed (it's been superseded).
func (l *EventLog) Append(event string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := compactKey(event)
	if key != "" {
		for i := len(l.events) - 1; i >= 0; i-- {
			if l.events[i].CompactKey == key {
				l.events = append(l.events[:i], l.events[i+1:]...)
				break
			}
		}
	}
	l.seq++
	l.events = append(l.events, SSEEvent{Seq: l.seq, Event: event, CompactKey: key})
	return l.seq
}

// Snapshot returns a copy of all events and the sequence number of the last one.
// Returns 0 if the log is empty.
func (l *EventLog) Snapshot() ([]SSEEvent, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]SSEEvent, len(l.events))
	copy(out, l.events)
	return out, l.seq
}

type SSEClient struct {
	id        string
	sessionID string
	cwd       string // set when ?cwd= connects before a session exists
	label     string // label from ?label= for pre-session state
	events    chan SSEEvent
	mu        sync.Mutex
}

func (c *SSEClient) Send(event SSEEvent) {
	select {
	case c.events <- event:
	default:
	}
}

func (c *SSEClient) ActiveSession() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *SSEClient) SetSession(id string) {
	c.mu.Lock()
	c.sessionID = id
	c.mu.Unlock()
}

// NotifyClient receives lightweight notification events (permission prompts, task completion).
type NotifyClient struct {
	events chan string
}

type Hub struct {
	clients       map[string]*SSEClient
	notifyClients map[string]*NotifyClient
	Sessions      *SessionManager
	Tracker       *SessionTracker
	Labels        *LabelStore
	Reviews       *ReviewStore
	mu            sync.RWMutex
}

// Close kills all active claude processes.
func (h *Hub) Close() {
	for _, s := range h.Sessions.List() {
		s.Close()
	}
}

func (h *Hub) AddNotifyClient(id string) *NotifyClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := &NotifyClient{events: make(chan string, 16)}
	h.notifyClients[id] = c
	return c
}

func (h *Hub) RemoveNotifyClient(id string) {
	h.mu.Lock()
	delete(h.notifyClients, id)
	h.mu.Unlock()
}

func (h *Hub) notifyAll(event string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.notifyClients {
		select {
		case c.events <- event:
		default:
		}
	}
}

func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".monetdroid")
}

func NewHub() *Hub {
	return NewHubWithDataDir(defaultDataDir())
}

func NewHubWithDataDir(dataDir string) *Hub {
	go func() {
		t := NewGitTrace("warm-cache")
		defer t.Log()
		ScanHistory(t)
	}()
	return &Hub{
		clients:       make(map[string]*SSEClient),
		notifyClients: make(map[string]*NotifyClient),
		Sessions:      NewSessionManager(),
		Tracker:       NewSessionTracker(dataDir),
		Labels:        NewLabelStore(dataDir),
		Reviews:       NewReviewStore(),
	}
}

func (h *Hub) RemoveClient(cid string) {
	h.mu.Lock()
	delete(h.clients, cid)
	h.mu.Unlock()
}

func (h *Hub) BroadcastToSession(sessionID string, event string) {
	s := h.Sessions.Get(sessionID)
	var seq uint64
	if s != nil {
		seq = s.EventLog.Append(event)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	sseEvent := SSEEvent{Seq: seq, Event: event}
	for _, c := range h.clients {
		if c.ActiveSession() == sessionID {
			c.Send(sseEvent)
		}
	}
}

// Broadcast sends a ServerMsg to all clients viewing that session.
func (h *Hub) Broadcast(msg ServerMsg) {
	sessionID := msg.SessionID
	if sessionID == "" {
		return
	}
	s := h.Sessions.Get(sessionID)

	// Accumulate cost — skip sub-agent cost events to avoid overwriting parent context
	if msg.Type == "cost" && msg.Cost != nil && s != nil {
		if s.GetAgentDepth() == 0 {
			s.AccumulateCost(msg.Cost)
		}
	}

	// Update todos from TodoWrite/TaskCreate/TaskUpdate tool_use events
	todosChanged := false
	if msg.Type == "tool_use" && s != nil {
		switch msg.Tool {
		case "TodoWrite":
			if todos := ParseTodos(msg.Input); todos != nil {
				s.SetTodos(todos)
				todosChanged = true
			}
		case "TaskCreate":
			if msg.Input != nil {
				s.AppendTaskFromCreate(msg.Input.TaskCreate)
				todosChanged = true
			}
		case "TaskUpdate":
			if msg.Input != nil {
				s.UpdateTask(msg.Input.TaskUpdate)
				todosChanged = true
			}
		}
	}

	msgHTML := RenderMsg(msg)

	var parts []string

	// OOB-swap the todos panel children (summary + body) to preserve open/closed state
	if todosChanged {
		todos := s.GetTodosCopy()
		parts = append(parts, OobSwap("todos-summary", "innerHTML", RenderTodosSummary(todos)))
		parts = append(parts, OobSwap("todos-body", "innerHTML", RenderTodosBody(todos)))
	}

	if msg.Type == "cost" && s != nil {
		parts = append(parts, OobSwap("cost-bar", "innerHTML", RenderCostBar(s)))
	}

	// Update agent chip stats via OOB swap
	if msg.Type == "agent_progress" && msg.AgentStat != nil && msg.ToolUseID != "" {
		parts = append(parts, OobSwap("agent-stats-"+msg.ToolUseID, "innerHTML", RenderAgentStatHTML(msg.AgentStat)))
	}

	// Start elapsed timer for Agent tool chips.
	// agent_started is broadcast by handleStreamEvent when task_started arrives,
	// which is after StartAgent creates the stop channel.
	if msg.Type == "agent_started" && msg.ToolUseID != "" && s != nil {
		toolUseID := msg.ToolUseID
		go func() {
			stopCh := s.GetAgentStop(toolUseID)
			if stopCh == nil {
				return
			}
			started := time.Now()
			for {
				select {
				case <-stopCh:
					return
				case <-time.After(1 * time.Second):
					secs := int(time.Since(started).Seconds())
					h.BroadcastToSession(sessionID, FormatSSE("htmx",
						OobSwap("elapsed-"+toolUseID, "innerHTML", fmt.Sprintf("%ds", secs))))
				}
			}
		}()
	}

	thinkingDots := `<span></span><span></span><span></span>`
	clearThinking := OobSwap("thinking", "innerHTML", "")
	clearStreaming := OobSwap("streaming", "innerHTML", "")

	stopBtnHTML := `<button class="stop-btn" id="stop-btn" hx-post="/stop" hx-swap="none" hx-include="#session-id">◼</button>`

	// --- Streaming deltas (text_delta, thinking_delta) ---
	if msg.Type == "text_delta" && s != nil {
		accumulated := s.AppendStreamingText(msg.Text)
		parts = append(parts, clearThinking)
		parts = append(parts, OobSwap("streaming", "innerHTML",
			fmt.Sprintf(`<div class="msg msg-assistant"><div class="msg-bubble streaming-text">%s</div></div>`, Esc(accumulated))))
		event := FormatSSE("htmx", strings.Join(parts, "\n"))
		h.BroadcastToSession(sessionID, event)
		return
	}
	if msg.Type == "thinking_delta" && s != nil {
		accumulated := s.AppendStreamingThinking(msg.Text)
		parts = append(parts, clearThinking)
		preview := accumulated
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		parts = append(parts, OobSwap("streaming", "innerHTML",
			fmt.Sprintf(`<div class="msg msg-thinking"><details class="thinking-chip" open><summary class="thinking-summary">%s</summary><div class="thinking-detail">%s</div></details></div>`, Esc(preview), Esc(accumulated))))
		event := FormatSSE("htmx", strings.Join(parts, "\n"))
		h.BroadcastToSession(sessionID, event)
		return
	}

	// When a final text/thinking message arrives, clear the streaming accumulator
	if (msg.Type == "text" || msg.Type == "thinking") && s != nil {
		s.ClearStreaming()
	}

	// Push to notification clients (Android app)
	if msg.Type == "permission_request" && s != nil {
		permLabel := msg.PermTool
		if msg.PermReason != "" {
			permLabel = msg.PermTool + ": " + msg.PermReason
		}
		info := s.GetTrackerInfo()
		data := fmt.Sprintf(`{"text":%q,"session":%q,"cwd":%q}`, permLabel, s.ID, ShortPath(info.Cwd))
		h.notifyAll(FormatSSE("permission", data))

		h.Tracker.Track(TrackedSession{
			ClaudeID:  s.ID,
			Label:     info.Label,
			AutoLabel: info.AutoLabel,
			Status:    "blocked",
			Result:    permLabel,
			Cwd:       info.Cwd,
			Branches:  info.Branches,
		})
	}
	// Inline permission: OOB swap into the tool chip's perm-slot (top-level tools only)
	if msg.Type == "permission_request" && msg.PermTool != "AskUserQuestion" && msg.ToolUseID != "" {
		if s != nil && s.IsTopLevelTool(msg.ToolUseID) {
			parts = append(parts, OobSwap("perm-slot-"+msg.ToolUseID, "innerHTML", RenderInlinePermission(msg)))
			// Upgrade the tool chip's detail with richer permission detail.
			upgraded := false
			switch msg.PermTool {
			case "Edit", "FileEdit":
				if fp, old, new_, replAll, ok := editDiffFromInput(msg.PermInput); ok {
					if diffHTML := RenderEditDiffTable(fp, old, new_, replAll, msg.SessionID, true); diffHTML != "" {
						parts = append(parts, OobSwap("tool-detail-"+msg.ToolUseID, "innerHTML", diffHTML))
						upgraded = true
					}
				}
			case "Write", "FileWrite":
				if fp, content, ok := writeDiffFromInput(msg.PermInput); ok {
					if diffHTML := RenderWriteDiffTable(fp, content, msg.SessionID, true); diffHTML != "" {
						parts = append(parts, OobSwap("tool-detail-"+msg.ToolUseID, "innerHTML", diffHTML))
						upgraded = true
					}
				}
			case "ExitPlanMode":
				if msg.PermInput != nil && msg.PermInput.PlanMode != nil && msg.PermInput.PlanMode.Plan != "" {
					parts = append(parts, OobSwap("tool-detail-"+msg.ToolUseID, "innerHTML", RenderMarkdown(msg.PermInput.PlanMode.Plan)))
					upgraded = true
				}
			}
			if !upgraded {
				permDetail := FormatPermDetail(msg.PermTool, msg.PermInput)
				if permDetail != "" {
					parts = append(parts, OobSwap("tool-detail-"+msg.ToolUseID, "innerHTML", Esc(permDetail)))
				}
			}
		} else {
			// Sub-agent or unknown tool — render standalone
			msgHTML = RenderPermission(msg)
		}
	}
	if msg.Type == "done" && s != nil {
		info := s.GetTrackerInfo()
		result := s.LastAssistantText()
		data := fmt.Sprintf(`{"text":"task complete","session":%q,"cwd":%q}`, s.ID, ShortPath(info.Cwd))
		h.notifyAll(FormatSSE("done", data))

		if len(result) > 200 {
			result = result[:200] + "..."
		}
		h.Tracker.Track(TrackedSession{
			ClaudeID:  s.ID,
			Label:     info.Label,
			AutoLabel: info.AutoLabel,
			Status:    "completed",
			Result:    result,
			Cwd:       info.Cwd,
			Branches:  info.Branches,
		})
	}

	if msg.Type == "running" {
		if s != nil {
			s.ClearStreaming()
			info := s.GetTrackerInfo()
			h.Tracker.Track(TrackedSession{
				ClaudeID:  s.ID,
				Label:     info.Label,
				AutoLabel: info.AutoLabel,
				Status:    "running",
				Cwd:       info.Cwd,
				Branches:  info.Branches,
			})
		}
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span class="di-running" id="running-dot"></span>`))
		parts = append(parts, OobSwap("stop-btn", "outerHTML", stopBtnHTML))
		parts = append(parts, OobSwap("thinking", "innerHTML", thinkingDots))
	}
	if msg.Type == "done" {
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span id="running-dot" style="display:none"></span>`))
		parts = append(parts, OobSwap("stop-btn", "outerHTML", `<span id="stop-btn"></span>`))
		parts = append(parts, clearThinking)
		parts = append(parts, clearStreaming)
		// Refresh git diff stat
		if s != nil {
			cwd := s.GetCwd()
			t := NewGitTrace("diff-stat")
			defer t.Log()
			if ds, err := GitDiffStat(t, cwd); err == nil {
				s.SetDiffStat(ds)
			}
			parts = append(parts, OobSwap("cost-bar", "innerHTML", RenderCostBar(s)))
		}
	}
	// Remove spinner when tool_result arrives
	if msg.Type == "tool_result" && msg.ToolUseID != "" {
		// Detect background Bash tasks — insert lazy-load SSE slot for output
		if bgPath := ParseBgTaskPath(msg.Output); bgPath != "" {
			// Register the output path and stop channel for the stream endpoint.
			toolUseID := msg.ToolUseID
			stopCh := make(chan struct{})
			if s != nil {
				s.RegisterBgStop(toolUseID, stopCh)
				s.RegisterBgPath(toolUseID, bgPath)
			}
			// Populate the bg-slot with a lazy-load trigger.
			// The SSE connection is only made when the user opens the details.
			// Keep the spinner — it will be removed when task_done arrives.
			parts = append(parts, OobSwap("bg-slot-"+msg.ToolUseID, "innerHTML",
				RenderBgSlot(sessionID, msg.ToolUseID)))
			msgHTML = ""
			// Elapsed timer — ticks every second until task completes.
			go func() {
				started := time.Now()
				for {
					select {
					case <-stopCh:
						return
					case <-time.After(1 * time.Second):
						secs := int(time.Since(started).Seconds())
						h.BroadcastToSession(sessionID, FormatSSE("htmx",
							OobSwap("elapsed-"+toolUseID, "innerHTML", fmt.Sprintf("%ds", secs))))
					}
				}
			}()
		} else {
			parts = append(parts, OobSwap("spinner-"+msg.ToolUseID, "outerHTML", ""))
		}
	}
	// Background task completed — remove spinner, show final elapsed time
	if msg.Type == "task_done" && msg.ToolUseID != "" {
		parts = append(parts, OobSwap("spinner-"+msg.ToolUseID, "outerHTML", ""))
	}

	if msgHTML != "" {
		parts = append(parts, clearStreaming)
		parts = append(parts, OobSwap("msg-content", "beforeend", msgHTML))
		parts = append(parts, clearThinking)
		if msg.Type == "tool_use" || msg.Type == "tool_result" {
			parts = append(parts, OobSwap("thinking", "innerHTML", thinkingDots))
		}
	}

	if msg.Type == "permission_mode" {
		var modeHTML string
		if msg.PermMode != "" && msg.PermMode != "default" {
			names := map[string]string{
				"acceptEdits":       "Auto-accepting edits",
				"plan":              "Plan mode (read-only)",
				"bypassPermissions": "All permissions bypassed",
				"dontAsk":           "Don't ask mode",
			}
			label := names[msg.PermMode]
			if label == "" {
				label = msg.PermMode
			}
			modeHTML = fmt.Sprintf(`<span class="mode-label">%s</span><form hx-post="/mode" hx-swap="none" style="display:inline"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="mode" value="default"><button type="submit" class="mode-reset">reset to default</button></form>`, Esc(label), Esc(sessionID))
		}
		parts = append(parts, OobSwap("mode-bar", "innerHTML", modeHTML))
	}

	if len(parts) == 0 {
		return
	}
	event := FormatSSE("htmx", strings.Join(parts, "\n"))
	h.BroadcastToSession(sessionID, event)
}

func (h *Hub) StartTurn(s *Session, text string, images []protocol.ImageData) {
	proc := s.ResetInterruptAndGetProc()

	// Ensure process is alive
	if proc == nil || proc.IsDead() {
		broadcast := func(msg ServerMsg) {
			// Streaming deltas are ephemeral — don't persist in the session log.
			if msg.Type != "text_delta" && msg.Type != "thinking_delta" {
				s.Append(msg)
			}
			h.Broadcast(msg)
		}
		var err error
		proc, err = claude.StartProcessWithConfig(s.GetCwd(), func(event protocol.StreamEvent) {
			handleStreamEvent(s, &event, broadcast)
		}, s.ID, &claude.ProcessConfig{
			PermissionHandler: func(req protocol.PermissionRequest) protocol.PermResponse {
				return s.HandlePermission(req, func(msg ServerMsg) { h.Broadcast(msg) })
			},
			OnRawEvent: func(raw protocol.RawStreamEvent) {
				handleRawStreamEvent(s, &raw, broadcast)
			},
		})
		if err != nil {
			h.Broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: err.Error()})
			return
		}
		s.SetProc(proc)
	}

	// Drain stale turnDone from previously untracked turns
	// (e.g. messages injected during permission-blocked state).
	proc.DrainTurnDone()

	// Auto-label from first user message
	s.TryAutoLabel(text)

	s.SetRunning(true)
	s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text, Images: images})
	h.Broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text, Images: images})
	h.Broadcast(ServerMsg{Type: "running", SessionID: s.ID})

	go func() {
		if err := proc.SendUserMessage(text, images); err != nil {
			h.Broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: err.Error()})
			s.SetRunning(false)
			return
		}
		h.waitAndDrainLoop(s, proc)
	}()
}

// waitAndDrainLoop waits for the current turn to complete, then drains
// any queued messages, sending each as a new turn. Loops until the queue
// is empty or the session is interrupted.
func (h *Hub) waitAndDrainLoop(s *Session, proc *claude.ClaudeProcess) {
	for {
		proc.WaitForTurnDone(context.Background())

		s.SetRunning(false)
		h.Broadcast(ServerMsg{Type: "done", SessionID: s.ID})

		if proc.IsDead() {
			s.CloseAllBgStops()
		}

		interrupted, next := s.DrainQueue()
		if interrupted || next == "" {
			break
		}

		h.BroadcastToSession(s.ID, FormatSSE("htmx", RenderQueueBar(s.ID, "")))
		s.SetRunning(true)
		s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})
		h.Broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})
		h.Broadcast(ServerMsg{Type: "running", SessionID: s.ID})

		if err := proc.SendUserMessage(next, nil); err != nil {
			h.Broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: err.Error()})
			s.SetRunning(false)
			return
		}
	}
}

// renderContext holds precomputed metadata for rendering a slice of messages.
type renderContext struct {
	lastCompact    int
	completedTools map[string]bool
	bgTaskResults  map[string]string    // tool_use id → output file path
	suppressedIDs  map[string]bool      // tool_use ids for tools whose results are suppressed
	pendingPerms   map[string]ServerMsg // tool_use id → unresolved inline permission_request
}

// precomputeRenderContext scans the full log to build rendering metadata.
func precomputeRenderContext(log []ServerMsg) renderContext {
	rc := renderContext{
		lastCompact:    -1,
		completedTools: make(map[string]bool),
		bgTaskResults:  make(map[string]string),
		suppressedIDs:  make(map[string]bool),
		pendingPerms:   make(map[string]ServerMsg),
	}
	for i, msg := range log {
		if msg.Type == "compact_boundary" {
			rc.lastCompact = i
		}
		if msg.Type == "tool_result" && msg.ToolUseID != "" {
			rc.completedTools[msg.ToolUseID] = true
			if bgPath := ParseBgTaskPath(msg.Output); bgPath != "" {
				rc.bgTaskResults[msg.ToolUseID] = bgPath
			}
		}
		if msg.Type == "tool_use" && suppressResultTools[msg.Tool] {
			rc.suppressedIDs[msg.ToolUseID] = true
		}
		if msg.Type == "permission_request" && msg.PermTool != "AskUserQuestion" && msg.ToolUseID != "" {
			rc.pendingPerms[msg.ToolUseID] = msg
		}
	}
	return rc
}

// renderMessages renders messages from log[start:end] into HTML.
func renderMessages(log []ServerMsg, start, end int, rc renderContext, sessionID string) string {
	var b strings.Builder
	if rc.lastCompact >= 0 && start <= rc.lastCompact {
		// We're rendering from inside the compacted region
		if start == 0 {
			b.WriteString(`<div class="compacted-context">`)
		}
	}
	for i := start; i < end; i++ {
		msg := log[i]
		if msg.Type == "compact_boundary" && i == rc.lastCompact {
			b.WriteString(`</div>`)
			b.WriteString(RenderMsg(msg))
			continue
		}
		if msg.Type == "tool_result" && rc.suppressedIDs[msg.ToolUseID] {
			if len(msg.Images) == 0 {
				continue
			}
		}
		// Suppress bg task tool_results — their output is loaded lazily
		if msg.Type == "tool_result" && rc.bgTaskResults[msg.ToolUseID] != "" {
			continue
		}
		// Skip inline permission_request messages — they're rendered inside tool chips
		if msg.Type == "permission_request" && msg.PermTool != "AskUserQuestion" && msg.ToolUseID != "" {
			continue
		}
		rendered := RenderMsg(msg)
		// Strip spinners for tool_use events that already have results
		if msg.Type == "tool_use" && rc.completedTools[msg.ToolUseID] {
			rendered = stripSpinner(rendered, msg.ToolUseID)
		}
		// Populate the bg-slot with a lazy-load trigger for bg task tool chips
		if msg.Type == "tool_use" && rc.bgTaskResults[msg.ToolUseID] != "" {
			emptySlot := fmt.Sprintf(`<div id="bg-slot-%s"></div>`, Esc(msg.ToolUseID))
			populatedSlot := fmt.Sprintf(`<div id="bg-slot-%s">%s</div>`, Esc(msg.ToolUseID), RenderBgSlot(sessionID, msg.ToolUseID))
			rendered = strings.Replace(rendered, emptySlot, populatedSlot, 1)
		}
		// Populate the perm-slot with inline permission for unresolved permissions
		if msg.Type == "tool_use" && msg.ToolUseID != "" {
			if pm, ok := rc.pendingPerms[msg.ToolUseID]; ok {
				emptySlot := fmt.Sprintf(`<div id="perm-slot-%s"></div>`, Esc(msg.ToolUseID))
				filledSlot := fmt.Sprintf(`<div id="perm-slot-%s">%s</div>`, Esc(msg.ToolUseID), RenderInlinePermission(pm))
				rendered = strings.Replace(rendered, emptySlot, filledSlot, 1)
				// Force the tool chip <details> open so the permission buttons are visible
				rendered = strings.Replace(rendered, `<details class="tool-chip">`, `<details class="tool-chip" open>`, 1)
			}
		}
		b.WriteString(rendered)
	}
	return b.String()
}

// renderSentinel returns an HTML element that triggers loading older messages.
// Scroll position is preserved by the beforeSwap/afterSwap handlers on #messages.
func renderSentinel(sessionID string, beforeIdx int) string {
	return fmt.Sprintf(
		`<div id="load-older" hx-get="/messages/before?session=%s&idx=%d" `+
			`hx-trigger="intersect root:#messages threshold:0" hx-swap="outerHTML">`+
			`<span class="loading-older">Loading older messages...</span></div>`,
		url.QueryEscape(sessionID), beforeIdx)
}

// SeedEventLog populates a session's EventLog from its current state.
// Called once when loading a session from disk or when a new session is
// created, before any live Broadcast events arrive.
func (h *Hub) SeedEventLog(s *Session) {
	snap := s.SeedSnapshot()

	// Refresh git diff stat
	if snap.Cwd != "" {
		t := NewGitTrace("seed-diff-stat")
		defer t.Log()
		if ds, err := GitDiffStat(t, snap.Cwd); err == nil {
			s.SetDiffStat(ds)
		}
	}

	// Rebuild todos by replaying tool_use events in order. TodoWrite replaces
	// the list wholesale; TaskCreate appends; TaskUpdate mutates by ID.
	s.SetTodos(nil)
	for _, msg := range snap.Log {
		if msg.Type != "tool_use" {
			continue
		}
		switch msg.Tool {
		case "TodoWrite":
			if t := ParseTodos(msg.Input); t != nil {
				s.SetTodos(t)
			}
		case "TaskCreate":
			if msg.Input != nil {
				s.AppendTaskFromCreate(msg.Input.TaskCreate)
			}
		case "TaskUpdate":
			if msg.Input != nil {
				s.UpdateTask(msg.Input.TaskUpdate)
			}
		}
	}
	todos := s.GetTodosCopy()

	// --- Chrome setup event: session-id, label, running state, cost, mode, todos, queue ---
	var chromeParts []string
	sessionLabel := ShortPath(snap.Cwd)
	if snap.Label != "" {
		sessionLabel = snap.Label
		if snap.AutoLabel {
			sessionLabel = "(auto) " + sessionLabel
		}
	}
	chromeParts = append(chromeParts, OobSwap("session-label", "innerHTML", Esc(sessionLabel)))
	chromeParts = append(chromeParts, TitleOob(sessionLabel))
	chromeParts = append(chromeParts, FaviconOob(sessionLabel))
	chromeParts = append(chromeParts, OobSwap("session-id", "outerHTML",
		fmt.Sprintf(`<input type="hidden" name="session_id" id="session-id" value="%s">`, Esc(s.ID))))
	chromeParts = append(chromeParts, OobSwap("close-btn", "outerHTML",
		`<form id="close-btn" hx-post="/close" hx-swap="none" hx-include="#session-id"><button class="header-btn" type="submit" title="Close session">✕</button></form>`))
	chromeParts = append(chromeParts, CwdCopyButton(s.GetCwd()))

	if snap.Running {
		chromeParts = append(chromeParts, OobSwap("running-dot", "outerHTML", `<span class="di-running" id="running-dot"></span>`))
		chromeParts = append(chromeParts, OobSwap("stop-btn", "outerHTML",
			`<button class="stop-btn" id="stop-btn" hx-post="/stop" hx-swap="none" hx-include="#session-id">◼</button>`))
		chromeParts = append(chromeParts, OobSwap("thinking", "innerHTML", `<span></span><span></span><span></span>`))
	} else {
		chromeParts = append(chromeParts, OobSwap("running-dot", "outerHTML", `<span id="running-dot" style="display:none"></span>`))
		chromeParts = append(chromeParts, OobSwap("stop-btn", "outerHTML", `<span id="stop-btn"></span>`))
	}

	chromeParts = append(chromeParts, OobSwap("cost-bar", "innerHTML", RenderCostBar(s)))

	var modeHTML string
	if snap.PermMode != "" && snap.PermMode != "default" {
		names := map[string]string{
			"acceptEdits": "Auto-accepting edits", "plan": "Plan mode (read-only)",
			"bypassPermissions": "All permissions bypassed", "dontAsk": "Don't ask mode",
		}
		ml := names[snap.PermMode]
		if ml == "" {
			ml = snap.PermMode
		}
		modeHTML = fmt.Sprintf(`<span class="mode-label">%s</span><form hx-post="/mode" hx-swap="none" style="display:inline"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="mode" value="default"><button type="submit" class="mode-reset">reset to default</button></form>`, Esc(ml), Esc(s.ID))
	}
	chromeParts = append(chromeParts, OobSwap("mode-bar", "innerHTML", modeHTML))
	chromeParts = append(chromeParts, OobSwap("todos-summary", "innerHTML", RenderTodosSummary(todos)))
	chromeParts = append(chromeParts, OobSwap("todos-body", "innerHTML", RenderTodosBody(todos)))
	chromeParts = append(chromeParts, RenderQueueBar(s.ID, snap.QueuedText))

	s.EventLog.Append(FormatSSE("htmx", strings.Join(chromeParts, "\n")))

	// --- Render messages from the log (paginated) ---
	rc := precomputeRenderContext(snap.Log)
	// Register bg paths for lazy-load stream endpoint
	for id, bgPath := range rc.bgTaskResults {
		s.RegisterBgPath(id, bgPath)
	}

	const pageSize = 100
	start := 0
	if len(snap.Log) > pageSize {
		start = len(snap.Log) - pageSize
		// Don't split inside the compacted region
		if rc.lastCompact >= 0 && start <= rc.lastCompact {
			start = 0
		}
	}

	var msgsHTML strings.Builder
	if start > 0 {
		msgsHTML.WriteString(renderSentinel(s.ID, start))
	}
	msgsHTML.WriteString(renderMessages(snap.Log, start, len(snap.Log), rc, s.ID))

	s.EventLog.Append(FormatSSE("htmx", OobSwap("msg-content", "innerHTML", msgsHTML.String())))
}

// BuildReplay writes all stored SSE events for the session directly to w
// and returns the sequence number of the last event written. The caller
// should drop any live events with seq <= the returned value.
func (h *Hub) BuildReplay(s *Session, w io.Writer, flusher http.Flusher) uint64 {
	events, lastSeq := s.EventLog.Snapshot()
	for _, e := range events {
		fmt.Fprint(w, e.Event)
	}
	flusher.Flush()
	return lastSeq
}
