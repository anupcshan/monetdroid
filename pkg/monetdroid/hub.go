package monetdroid

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

type SSEClient struct {
	id        string
	sessionID string
	events    chan string
	mu        sync.Mutex
}

func (c *SSEClient) Send(event string) {
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
	mu            sync.RWMutex

	// BuildClaudeCmd overrides how the claude command is constructed.
	// If nil, defaults to exec.Command("claude", args...) with cwd set.
	BuildClaudeCmd func(cwd string, args []string) *exec.Cmd
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

func NewHub() *Hub {
	return &Hub{
		clients:       make(map[string]*SSEClient),
		notifyClients: make(map[string]*NotifyClient),
		Sessions:      NewSessionManager(),
	}
}

func (h *Hub) GetOrCreateClient(cid string) *SSEClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.clients[cid]; ok {
		return c
	}
	c := &SSEClient{id: cid, events: make(chan string, 64)}
	h.clients[cid] = c
	return c
}

func (h *Hub) RemoveClient(cid string) {
	h.mu.Lock()
	delete(h.clients, cid)
	h.mu.Unlock()
}

func (h *Hub) BroadcastToSession(sessionID string, event string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	sent := 0
	for _, c := range h.clients {
		if c.ActiveSession() == sessionID {
			c.Send(event)
			sent++
		}
	}
}

func (h *Hub) SendToClient(cid string, event string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if c, ok := h.clients[cid]; ok {
		c.Send(event)
	}
}

// Broadcast sends a ServerMsg to all clients viewing that session.
func (h *Hub) Broadcast(msg ServerMsg) {
	sessionID := msg.SessionID
	if sessionID == "" {
		return
	}
	s := h.Sessions.Get(sessionID)

	// Accumulate cost
	if msg.Type == "cost" && msg.Cost != nil && s != nil {
		s.Mu.Lock()
		if msg.Cost.TotalCostUSD > 0 {
			s.CostAccum.TotalCostUSD = msg.Cost.TotalCostUSD
		}
		if msg.Cost.ContextUsed > 0 {
			s.CostAccum.ContextUsed = msg.Cost.ContextUsed
		}
		if msg.Cost.ContextWindow > 0 {
			s.CostAccum.ContextWindow = msg.Cost.ContextWindow
		}
		s.Mu.Unlock()
	}

	// Update todos from TodoWrite tool_use events
	if msg.Type == "tool_use" && msg.Tool == "TodoWrite" {
		if todos := ParseTodos(msg.Input); todos != nil {
			s.Mu.Lock()
			s.Todos = todos
			s.Mu.Unlock()
		}
	}

	msgHTML := RenderMsg(msg)

	var parts []string
	if msgHTML != "" {
		parts = append(parts, OobSwap("messages", "beforeend", msgHTML))
	}

	// OOB-swap the todos panel children (summary + body) to preserve open/closed state
	if msg.Type == "tool_use" && msg.Tool == "TodoWrite" {
		s.Mu.Lock()
		todos := make([]Todo, len(s.Todos))
		copy(todos, s.Todos)
		s.Mu.Unlock()
		parts = append(parts, OobSwap("todos-summary", "innerHTML", RenderTodosSummary(todos)))
		parts = append(parts, OobSwap("todos-body", "innerHTML", RenderTodosBody(todos)))
	}

	if msg.Type == "cost" && s != nil {
		parts = append(parts, OobSwap("cost-bar", "innerHTML", RenderCostBar(s)))
	}

	thinkingHTML := `<div class="thinking-indicator" id="thinking"><span></span><span></span><span></span></div>`
	emptyThinking := OobSwap("thinking", "outerHTML", `<div id="thinking"></div>`)

	stopBtnHTML := `<button class="stop-btn" id="stop-btn" hx-post="/stop" hx-swap="none">◼</button>`

	// Push to notification clients (Android app)
	if msg.Type == "permission_request" && s != nil {
		label := msg.PermTool
		if msg.PermReason != "" {
			label = msg.PermTool + ": " + msg.PermReason
		}
		s.Mu.Lock()
		claudeID := s.ClaudeID
		cwd := s.Cwd
		s.Mu.Unlock()
		data := fmt.Sprintf(`{"text":%q,"session":%q,"cwd":%q}`, label, claudeID, ShortPath(cwd))
		h.notifyAll(FormatSSE("permission", data))
	}
	if msg.Type == "done" && s != nil {
		s.Mu.Lock()
		claudeID := s.ClaudeID
		cwd := s.Cwd
		s.Mu.Unlock()
		data := fmt.Sprintf(`{"text":"task complete","session":%q,"cwd":%q}`, claudeID, ShortPath(cwd))
		h.notifyAll(FormatSSE("done", data))
	}

	if msg.Type == "running" {
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span class="di-running" id="running-dot"></span>`))
		parts = append(parts, OobSwap("stop-btn", "outerHTML", stopBtnHTML))
		parts = append(parts, OobSwap("messages", "beforeend", thinkingHTML))
	}
	if msg.Type == "done" {
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span id="running-dot" style="display:none"></span>`))
		parts = append(parts, OobSwap("stop-btn", "outerHTML", `<span id="stop-btn"></span>`))
		parts = append(parts, emptyThinking)
		// Refresh git diff stat
		if s != nil {
			s.Mu.Lock()
			cwd := s.Cwd
			s.Mu.Unlock()
			if ds, err := GitDiffStat(cwd); err == nil {
				s.Mu.Lock()
				s.DiffStat = ds
				s.Mu.Unlock()
			}
			parts = append(parts, OobSwap("cost-bar", "innerHTML", RenderCostBar(s)))
		}
	}
	if msgHTML != "" {
		parts = append(parts, OobSwap("thinking", "outerHTML", ""))
		if msg.Type == "tool_use" || msg.Type == "tool_result" {
			parts = append(parts, OobSwap("messages", "beforeend", thinkingHTML))
		}
	}

	// Update the browser URL when a session gets its ClaudeID.
	//
	// Problem: New sessions start with /?cwd=... in the URL. The ClaudeID
	// arrives later (via the "system" init event from claude). If the user
	// reloads before the URL is updated, handleEvents sees ?cwd= and creates
	// a brand new empty session, losing all messages.
	//
	// Solution: Push a URL update to the browser via HTMX when we learn the
	// ClaudeID. This is tricky because SSE events can't set response headers
	// (like HX-Replace-Url) — only HTTP responses can. So we use a two-step
	// approach:
	//
	//   1. OOB swap a <span> into #url-state with hx-trigger="load" and
	//      hx-get="/session-url?session=<id>". The outerHTML swap causes
	//      HTMX to process the new element and fire its load trigger.
	//
	//   2. The /session-url handler returns 200 with the HX-Replace-Url
	//      header, which HTMX uses to update the browser URL bar.
	//
	// Approaches that DON'T work (tested):
	//   - hx-trigger="load" with innerHTML OOB swap: HTMX doesn't fire
	//     load triggers for child elements added via innerHTML OOB swaps.
	//   - hx-swap="none" on the triggered request: HTMX ignores
	//     HX-Replace-Url on 204 responses or when hx-swap="none".
	//   - <img onerror="history.replaceState(...)">: works but is an XSS
	//     pattern, not a proper HTMX mechanism.
	if msg.Type == "session_id" {
		parts = append(parts, fmt.Sprintf(
			`<span id="url-state" hx-swap-oob="outerHTML" hx-get="/session-url?session=%s" hx-trigger="load" hx-target="#url-state" hx-swap="innerHTML"></span>`, Esc(msg.Text)))
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

func (h *Hub) StartTurn(s *Session, text string, images []ImageData) {
	s.Mu.Lock()
	s.Interrupted = false
	proc := s.proc
	s.Mu.Unlock()

	// Ensure process is alive
	if proc == nil || proc.IsDead() {
		logBroadcast := func(msg ServerMsg) {
			s.Append(msg)
			h.Broadcast(msg)
		}
		var err error
		proc, err = StartProcess(s, h.BuildClaudeCmd, logBroadcast)
		if err != nil {
			h.Broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: err.Error()})
			return
		}
		s.Mu.Lock()
		s.proc = proc
		s.Mu.Unlock()
	}

	s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text, Images: images})
	h.Broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text, Images: images})
	h.Broadcast(ServerMsg{Type: "running", SessionID: s.ID})

	go func() {
		if err := proc.SendUserMessage(text, images); err != nil {
			h.Broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: err.Error()})
			return
		}

		// Wait for turn to complete (result event or process death)
		select {
		case <-proc.turnDone:
		case <-proc.dead:
		}

		h.Broadcast(ServerMsg{Type: "done", SessionID: s.ID})

		// Handle queued message
		s.Mu.Lock()
		interrupted := s.Interrupted
		next := s.QueuedText
		if !interrupted {
			s.QueuedText = ""
		}
		s.Mu.Unlock()

		if !interrupted && next != "" {
			h.BroadcastToSession(s.ID, FormatSSE("htmx", RenderQueueBar(s.ID, "")))
			s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})
			h.Broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})
			h.Broadcast(ServerMsg{Type: "running", SessionID: s.ID})
			if err := proc.SendUserMessage(next, nil); err != nil {
				h.Broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: err.Error()})
				return
			}
			select {
			case <-proc.turnDone:
			case <-proc.dead:
			}
			h.Broadcast(ServerMsg{Type: "done", SessionID: s.ID})
		}
	}()
}

// ReplaySession sends the full session state to a specific client.
func (h *Hub) ReplaySession(cid string, s *Session) {
	s.Mu.Lock()
	log_ := make([]ServerMsg, len(s.Log))
	copy(log_, s.Log)
	running := s.Running
	permMode := s.PermissionMode
	cwd := s.Cwd
	queuedText := s.QueuedText
	s.Mu.Unlock()

	// Refresh git diff stat
	if cwd != "" {
		if ds, err := GitDiffStat(cwd); err == nil {
			s.Mu.Lock()
			s.DiffStat = ds
			s.Mu.Unlock()
		}
	}

	// Rebuild todos from the last TodoWrite in the log
	var todos []Todo
	for _, msg := range log_ {
		if msg.Type == "tool_use" && msg.Tool == "TodoWrite" {
			if t := ParseTodos(msg.Input); t != nil {
				todos = t
			}
		}
	}
	s.Mu.Lock()
	s.Todos = todos
	s.Mu.Unlock()

	var msgsHTML strings.Builder
	suppressedIDs := make(map[string]bool)
	for _, msg := range log_ {
		if msg.Type == "tool_use" && suppressResultTools[msg.Tool] {
			suppressedIDs[msg.ToolUseID] = true
		}
		if msg.Type == "tool_result" && suppressedIDs[msg.ToolUseID] {
			delete(suppressedIDs, msg.ToolUseID)
			continue
		}
		msgsHTML.WriteString(RenderMsg(msg))
	}

	var parts []string
	parts = append(parts, OobSwap("session-label", "innerHTML", Esc(ShortPath(cwd))))
	parts = append(parts, OobSwap("messages", "innerHTML", msgsHTML.String()))
	parts = append(parts, OobSwap("cost-bar", "innerHTML", RenderCostBar(s)))

	if running {
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span class="di-running" id="running-dot"></span>`))
		parts = append(parts, OobSwap("stop-btn", "outerHTML", `<button class="stop-btn" id="stop-btn" hx-post="/stop" hx-swap="none">◼</button>`))
	} else {
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span id="running-dot" style="display:none"></span>`))
		parts = append(parts, OobSwap("stop-btn", "outerHTML", `<span id="stop-btn"></span>`))
	}

	var modeHTML string
	if permMode != "" && permMode != "default" {
		names := map[string]string{
			"acceptEdits": "Auto-accepting edits", "plan": "Plan mode (read-only)",
			"bypassPermissions": "All permissions bypassed", "dontAsk": "Don't ask mode",
		}
		label := names[permMode]
		if label == "" {
			label = permMode
		}
		modeHTML = fmt.Sprintf(`<span class="mode-label">%s</span><form hx-post="/mode" hx-swap="none" style="display:inline"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="mode" value="default"><button type="submit" class="mode-reset">reset to default</button></form>`, Esc(label), Esc(s.ID))
	}
	parts = append(parts, OobSwap("mode-bar", "innerHTML", modeHTML))
	parts = append(parts, OobSwap("todos-summary", "innerHTML", RenderTodosSummary(todos)))
	parts = append(parts, OobSwap("todos-body", "innerHTML", RenderTodosBody(todos)))
	parts = append(parts, RenderQueueBar(s.ID, queuedText))

	event := FormatSSE("htmx", strings.Join(parts, "\n"))
	h.SendToClient(cid, event)
}
