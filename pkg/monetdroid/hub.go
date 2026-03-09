package monetdroid

import (
	"fmt"
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

type Hub struct {
	clients  map[string]*SSEClient
	Sessions *SessionManager
	mu       sync.RWMutex
}

func NewHub() *Hub {
	return &Hub{
		clients:  make(map[string]*SSEClient),
		Sessions: NewSessionManager(),
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
		s.CostAccum.InputTokens += msg.Cost.InputTokens
		s.CostAccum.OutputTokens += msg.Cost.OutputTokens
		if msg.Cost.ContextUsed > 0 {
			s.CostAccum.ContextUsed = msg.Cost.ContextUsed
		}
		if msg.Cost.ContextWindow > 0 {
			s.CostAccum.ContextWindow = msg.Cost.ContextWindow
		}
		s.Mu.Unlock()
	}

	msgHTML := RenderMsg(msg)

	var parts []string
	if msgHTML != "" {
		parts = append(parts, OobSwap("messages", "beforeend", msgHTML))
	}

	if msg.Type == "cost" && s != nil {
		parts = append(parts, OobSwap("cost-bar", "innerHTML", RenderCostBar(s)))
	}

	thinkingHTML := `<div class="thinking-indicator" id="thinking"><span></span><span></span><span></span></div>`
	emptyThinking := OobSwap("thinking", "outerHTML", `<div id="thinking"></div>`)

	if msg.Type == "running" {
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span class="di-running" id="running-dot"></span>`))
		parts = append(parts, OobSwap("messages", "beforeend", thinkingHTML))
	}
	if msg.Type == "done" {
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span id="running-dot" style="display:none"></span>`))
		parts = append(parts, emptyThinking)
	}
	if msgHTML != "" {
		parts = append(parts, OobSwap("thinking", "outerHTML", ""))
		if msg.Type == "tool_use" || msg.Type == "tool_result" {
			parts = append(parts, OobSwap("messages", "beforeend", thinkingHTML))
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

func (h *Hub) StartTurn(s *Session, text string) {
	s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text})
	h.Broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text})
	h.Broadcast(ServerMsg{Type: "running", SessionID: s.ID})
	logBroadcast := func(msg ServerMsg) {
		s.Append(msg)
		h.Broadcast(msg)
	}
	go func() {
		RunClaudeTurn(s, text, logBroadcast)
		s.Mu.Lock()
		next := s.QueuedText
		s.QueuedText = ""
		s.Mu.Unlock()
		if next != "" {
			h.BroadcastToSession(s.ID, FormatSSE("htmx", RenderQueueBar(s.ID, "")))
			s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})
			h.Broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})
			h.Broadcast(ServerMsg{Type: "running", SessionID: s.ID})
			RunClaudeTurn(s, next, logBroadcast)
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

	var msgsHTML strings.Builder
	for _, msg := range log_ {
		msgsHTML.WriteString(RenderMsg(msg))
	}

	var parts []string
	parts = append(parts, OobSwap("session-label", "innerHTML", Esc(ShortPath(cwd))))
	parts = append(parts, OobSwap("messages", "innerHTML", msgsHTML.String()))
	parts = append(parts, OobSwap("cost-bar", "innerHTML", RenderCostBar(s)))

	if running {
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span class="di-running" id="running-dot"></span>`))
	} else {
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span id="running-dot" style="display:none"></span>`))
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
	parts = append(parts, RenderQueueBar(s.ID, queuedText))

	event := FormatSSE("htmx", strings.Join(parts, "\n"))
	h.SendToClient(cid, event)
}
