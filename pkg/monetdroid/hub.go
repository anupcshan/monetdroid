package monetdroid

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SSEEvent is a rendered SSE event string tagged with a monotonic sequence number.
type SSEEvent struct {
	Seq   uint64
	Event string
}

// EventLog is an append-only log of rendered SSE events for a session.
// Broadcast appends to it; replay reads from it. This ensures both paths
// produce identical output.
type EventLog struct {
	mu     sync.Mutex
	events []SSEEvent
	seq    uint64
}

// Append adds an event to the log and returns its sequence number.
func (l *EventLog) Append(event string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	l.events = append(l.events, SSEEvent{Seq: l.seq, Event: event})
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
	Queue         *NotificationQueue
	Labels        *LabelStore
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

func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".monetdroid")
}

func NewHub() *Hub {
	return NewHubWithDataDir(defaultDataDir())
}

func NewHubWithDataDir(dataDir string) *Hub {
	go ScanHistory() // warm the session info cache
	return &Hub{
		clients:       make(map[string]*SSEClient),
		notifyClients: make(map[string]*NotifyClient),
		Sessions:      NewSessionManager(),
		Queue:         NewNotificationQueue(dataDir),
		Labels:        NewLabelStore(dataDir),
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

	stopBtnHTML := `<button class="stop-btn" id="stop-btn" hx-post="/stop" hx-swap="none" hx-include="#session-id">◼</button>`

	// Push to notification clients (Android app)
	if msg.Type == "permission_request" && s != nil {
		permLabel := msg.PermTool
		if msg.PermReason != "" {
			permLabel = msg.PermTool + ": " + msg.PermReason
		}
		s.Mu.Lock()
		cwd := s.Cwd
		sessionLabel := s.Label
		autoLabel := s.AutoLabel
		s.Mu.Unlock()
		data := fmt.Sprintf(`{"text":%q,"session":%q,"cwd":%q}`, permLabel, s.ID, ShortPath(cwd))
		h.notifyAll(FormatSSE("permission", data))

		h.Queue.Enqueue(QueueItem{
			ClaudeID:  s.ID,
			Label:     sessionLabel,
			AutoLabel: autoLabel,
			Status:    "blocked",
			Result:    permLabel,
			Cwd:       cwd,
		})
	}
	if msg.Type == "done" && s != nil {
		s.Mu.Lock()
		cwd := s.Cwd
		label := s.Label
		autoLabel := s.AutoLabel
		// Find last assistant text for result summary
		var result string
		for i := len(s.Log) - 1; i >= 0; i-- {
			if s.Log[i].Type == "text" && s.Log[i].Text != "" {
				result = s.Log[i].Text
				break
			}
		}
		s.Mu.Unlock()
		data := fmt.Sprintf(`{"text":"task complete","session":%q,"cwd":%q}`, s.ID, ShortPath(cwd))
		h.notifyAll(FormatSSE("done", data))

		if len(result) > 200 {
			result = result[:200] + "..."
		}
		h.Queue.Enqueue(QueueItem{
			ClaudeID:  s.ID,
			Label:     label,
			AutoLabel: autoLabel,
			Status:    "completed",
			Result:    result,
			Cwd:       cwd,
		})
	}

	if msg.Type == "running" {
		parts = append(parts, OobSwap("running-dot", "outerHTML", `<span class="di-running" id="running-dot"></span>`))
		parts = append(parts, OobSwap("stop-btn", "outerHTML", stopBtnHTML))
		parts = append(parts, OobSwap("messages", "beforeend", thinkingHTML))
		// Remove any "blocked" queue item — session is running again
		if s != nil {
			h.Queue.Ack(s.ID)
		}
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
	// Remove spinner when tool_result arrives
	if msg.Type == "tool_result" && msg.ToolUseID != "" {
		// Detect background Bash tasks and start tailing their output
		if bgPath := ParseBgTaskPath(msg.Output); bgPath != "" {
			bgDivID := "bg-" + msg.ToolUseID
			// Insert an output area inside the tool chip; suppress the result chip.
			// Keep the spinner — it will be removed when task_done arrives.
			parts = append(parts, OobSwap("tool-"+msg.ToolUseID, "beforeend",
				fmt.Sprintf(`<div class="tool-bg-output" id="%s"></div>`, bgDivID)))
			msgHTML = ""
			toolUseID := msg.ToolUseID
			stopCh := make(chan struct{})
			if s != nil {
				s.Mu.Lock()
				s.BgTaskStops[toolUseID] = stopCh
				s.Mu.Unlock()
			}
			go func() {
				TailBgTask(bgPath, stopCh, func(chunk string) {
					event := RenderBgOutput(toolUseID, chunk)
					if event != "" {
						h.BroadcastToSession(sessionID, FormatSSE("htmx", event))
					}
				}, func(elapsed time.Duration) {
					secs := int(elapsed.Seconds())
					h.BroadcastToSession(sessionID, FormatSSE("htmx",
						OobSwap("elapsed-"+toolUseID, "innerHTML", fmt.Sprintf("%ds", secs))))
				})
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
		parts = append(parts, OobSwap("messages", "beforeend", msgHTML))
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
		proc, err = StartProcess(s, s.Cwd, h.BuildClaudeCmd, logBroadcast, s.ID)
		if err != nil {
			h.Broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: err.Error()})
			return
		}
		s.Mu.Lock()
		s.proc = proc
		s.Mu.Unlock()
	}

	// Auto-label from first user message
	s.Mu.Lock()
	if s.Label == "" && text != "" {
		label := text
		if len(label) > 60 {
			label = label[:60] + "..."
		}
		s.Label = label
		s.AutoLabel = true
	}
	s.Mu.Unlock()

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

// SeedEventLog populates a session's EventLog from its current state.
// Called once when loading a session from disk or when a new session is
// created, before any live Broadcast events arrive.
func (h *Hub) SeedEventLog(s *Session) {
	s.Mu.Lock()
	log_ := make([]ServerMsg, len(s.Log))
	copy(log_, s.Log)
	running := s.Running
	permMode := s.PermissionMode
	label := s.Label
	autoLabel := s.AutoLabel
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

	// --- Chrome setup event: session-id, label, running state, cost, mode, todos, queue ---
	var chromeParts []string
	sessionLabel := ShortPath(cwd)
	if label != "" {
		sessionLabel = label
		if autoLabel {
			sessionLabel = "(auto) " + sessionLabel
		}
	}
	chromeParts = append(chromeParts, OobSwap("session-label", "innerHTML", Esc(sessionLabel)))
	chromeParts = append(chromeParts, OobSwap("session-id", "outerHTML",
		fmt.Sprintf(`<input type="hidden" name="session_id" id="session-id" value="%s">`, Esc(s.ID))))

	if running {
		chromeParts = append(chromeParts, OobSwap("running-dot", "outerHTML", `<span class="di-running" id="running-dot"></span>`))
		chromeParts = append(chromeParts, OobSwap("stop-btn", "outerHTML",
			`<button class="stop-btn" id="stop-btn" hx-post="/stop" hx-swap="none" hx-include="#session-id">◼</button>`))
	} else {
		chromeParts = append(chromeParts, OobSwap("running-dot", "outerHTML", `<span id="running-dot" style="display:none"></span>`))
		chromeParts = append(chromeParts, OobSwap("stop-btn", "outerHTML", `<span id="stop-btn"></span>`))
	}

	chromeParts = append(chromeParts, OobSwap("cost-bar", "innerHTML", RenderCostBar(s)))

	var modeHTML string
	if permMode != "" && permMode != "default" {
		names := map[string]string{
			"acceptEdits": "Auto-accepting edits", "plan": "Plan mode (read-only)",
			"bypassPermissions": "All permissions bypassed", "dontAsk": "Don't ask mode",
		}
		ml := names[permMode]
		if ml == "" {
			ml = permMode
		}
		modeHTML = fmt.Sprintf(`<span class="mode-label">%s</span><form hx-post="/mode" hx-swap="none" style="display:inline"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="mode" value="default"><button type="submit" class="mode-reset">reset to default</button></form>`, Esc(ml), Esc(s.ID))
	}
	chromeParts = append(chromeParts, OobSwap("mode-bar", "innerHTML", modeHTML))
	chromeParts = append(chromeParts, OobSwap("todos-summary", "innerHTML", RenderTodosSummary(todos)))
	chromeParts = append(chromeParts, OobSwap("todos-body", "innerHTML", RenderTodosBody(todos)))
	chromeParts = append(chromeParts, RenderQueueBar(s.ID, queuedText))

	s.EventLog.Append(FormatSSE("htmx", strings.Join(chromeParts, "\n")))

	// --- Render all messages from the log ---
	// Find last compact boundary to wrap pre-compaction messages
	lastCompact := -1
	for i, msg := range log_ {
		if msg.Type == "compact_boundary" {
			lastCompact = i
		}
	}

	// Collect tool_use IDs that have results (so we can strip spinners)
	completedTools := make(map[string]bool)
	for _, msg := range log_ {
		if msg.Type == "tool_result" && msg.ToolUseID != "" {
			completedTools[msg.ToolUseID] = true
		}
	}

	var msgsHTML strings.Builder
	if lastCompact >= 0 {
		msgsHTML.WriteString(`<div class="compacted-context">`)
	}
	suppressedIDs := make(map[string]bool)
	for i, msg := range log_ {
		if msg.Type == "compact_boundary" && i == lastCompact {
			msgsHTML.WriteString(`</div>`)
			msgsHTML.WriteString(RenderMsg(msg))
			continue
		}
		if msg.Type == "tool_use" && suppressResultTools[msg.Tool] {
			suppressedIDs[msg.ToolUseID] = true
		}
		if msg.Type == "tool_result" && suppressedIDs[msg.ToolUseID] {
			delete(suppressedIDs, msg.ToolUseID)
			continue
		}
		rendered := RenderMsg(msg)
		// Strip spinners for tool_use events that already have results
		if msg.Type == "tool_use" && completedTools[msg.ToolUseID] {
			rendered = stripSpinner(rendered, msg.ToolUseID)
		}
		msgsHTML.WriteString(rendered)
	}

	s.EventLog.Append(FormatSSE("htmx", OobSwap("messages", "innerHTML", msgsHTML.String())))
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
