package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed index.html
var staticFiles embed.FS

// --- Protocol types ---

// Client -> Server
type ClientMsg struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Text      string `json:"text,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	// History loading
	DirKey    string `json:"dir_key,omitempty"`
	HistoryID string `json:"history_id,omitempty"`
	// Permission response
	PermID         string      `json:"perm_id,omitempty"`
	PermAllow      bool        `json:"perm_allow,omitempty"`
	PermSuggestion interface{} `json:"perm_suggestion,omitempty"` // the chosen suggestion object, or null
	PermMode       string      `json:"perm_mode,omitempty"`
}

// Server -> Client
type ServerMsg struct {
	Type      string        `json:"type"`
	SessionID string        `json:"session_id,omitempty"`
	Text      string        `json:"text,omitempty"`
	Tool      string        `json:"tool,omitempty"`
	Input     interface{}   `json:"input,omitempty"`
	Output    string        `json:"output,omitempty"`
	Error     string        `json:"error,omitempty"`
	Sessions  []SessionInfo `json:"sessions,omitempty"`
	Cost      *CostInfo     `json:"cost,omitempty"`
	// History
	History     []HistoryGroup   `json:"history,omitempty"`
	HistoryMsgs []HistoryMessage `json:"history_msgs,omitempty"`
	// Permission
	PermID          string      `json:"perm_id,omitempty"`
	PermTool        string      `json:"perm_tool,omitempty"`
	PermInput       interface{} `json:"perm_input,omitempty"`
	PermReason      string      `json:"perm_reason,omitempty"`
	PermSuggestions interface{} `json:"perm_suggestions,omitempty"`
	PermMode        string      `json:"perm_mode,omitempty"`
}

type SessionInfo struct {
	ID           string `json:"id"`
	ClaudeID     string `json:"claude_id"`
	Cwd          string `json:"cwd"`
	MessageCount int    `json:"message_count"`
	Running      bool   `json:"running"`
	CreatedAt    string `json:"created_at"`
}

type CostInfo struct {
	InputTokens   int `json:"input_tokens"`
	OutputTokens  int `json:"output_tokens"`
	ContextUsed   int `json:"context_used,omitempty"`
	ContextWindow int `json:"context_window,omitempty"`
}

type HistoryGroup struct {
	Dir      string           `json:"dir"`
	DirKey   string           `json:"dir_key"`
	Sessions []HistorySession `json:"sessions"`
}

type HistorySession struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	ModTime string `json:"mod_time"`
	ModUnix int64  `json:"mod_unix"`
}

type HistoryMessage struct {
	Type   string      `json:"type"` // "user", "assistant", "tool_use", "tool_result"
	Text   string      `json:"text,omitempty"`
	Tool   string      `json:"tool,omitempty"`
	Input  interface{} `json:"input,omitempty"`
	Output string      `json:"output,omitempty"`
}

// --- Session management ---

type PermResponse struct {
	Allow       bool
	Permissions []interface{} // updatedPermissions to send back
}

type Session struct {
	ID             string
	ClaudeID       string // Claude's session_id from first response
	Cwd            string
	PermissionMode string // current permission mode
	MessageCount   int
	Running        bool
	CreatedAt      time.Time
	JSONLPath      string      // path to history JSONL (for resumed sessions)
	Log            []ServerMsg // all messages for this session (for replay)
	QueuedMsgs     []string    // messages queued while session is busy
	// Permission handling
	permChans map[string]chan PermResponse
	writeJSON func(interface{}) // write to claude's stdin
	mu        sync.Mutex
}

// Append adds a message to the session log (caller must NOT hold s.mu)
func (s *Session) Append(msg ServerMsg) {
	s.mu.Lock()
	s.Log = append(s.Log, msg)
	s.mu.Unlock()
}

// Replay sends the full session log to a client, followed by current state
func (s *Session) Replay(c *Client) {
	s.mu.Lock()
	log := make([]ServerMsg, len(s.Log))
	copy(log, s.Log)
	running := s.Running
	permMode := s.PermissionMode
	s.mu.Unlock()
	for _, msg := range log {
		c.Send(msg)
	}
	// Send final state so client knows where things stand
	if running {
		c.Send(ServerMsg{Type: "running", SessionID: s.ID})
	} else {
		c.Send(ServerMsg{Type: "done", SessionID: s.ID})
	}
	if permMode != "" && permMode != "default" {
		c.Send(ServerMsg{Type: "permission_mode", SessionID: s.ID, PermMode: permMode})
	}
}

type SessionManager struct {
	sessions map[string]*Session
	mu       sync.RWMutex
	counter  int
}

func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: make(map[string]*Session)}
}

func (sm *SessionManager) Create(cwd string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.counter++
	id := fmt.Sprintf("s%d", sm.counter)
	s := &Session{
		ID:        id,
		Cwd:       cwd,
		CreatedAt: time.Now(),
	}
	sm.sessions[id] = s
	return s
}

func (sm *SessionManager) Get(id string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[id]
}

func (sm *SessionManager) FindByJSONLPath(path string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, s := range sm.sessions {
		if s.JSONLPath == path {
			return s
		}
	}
	return nil
}

func (sm *SessionManager) List() []SessionInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make([]SessionInfo, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		s.mu.Lock()
		out = append(out, SessionInfo{
			ID:           s.ID,
			ClaudeID:     s.ClaudeID,
			Cwd:          s.Cwd,
			MessageCount: s.MessageCount,
			Running:      s.Running,
			CreatedAt:    s.CreatedAt.Format(time.RFC3339),
		})
		s.mu.Unlock()
	}
	return out
}

// --- Claude CLI runner ---

func runClaudeTurn(s *Session, prompt string, broadcast func(ServerMsg)) {
	s.mu.Lock()
	s.Running = true
	s.MessageCount++
	claudeID := s.ClaudeID
	cwd := s.Cwd
	s.permChans = make(map[string]chan PermResponse)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.Running = false
		s.permChans = nil
		s.mu.Unlock()
	}()

	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--permission-mode", "default",
	}

	if claudeID != "" {
		args = append(args, "--resume", claudeID)
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("stdin pipe error: %v", err)})
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("stdout pipe error: %v", err)})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("stderr pipe error: %v", err)})
		return
	}

	if err := cmd.Start(); err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("start error: %v", err)})
		return
	}

	writeJSON := func(v interface{}) {
		data, _ := json.Marshal(v)
		stdin.Write(append(data, '\n'))
	}

	s.mu.Lock()
	s.writeJSON = writeJSON
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.writeJSON = nil
		s.mu.Unlock()
	}()

	// Send initialize request
	writeJSON(map[string]interface{}{
		"type":       "control_request",
		"request_id": "init_1",
		"request":    map[string]interface{}{"subtype": "initialize"},
	})

	// Send user message
	writeJSON(map[string]interface{}{
		"type":       "user",
		"session_id": "",
		"message": map[string]interface{}{
			"role":    "user",
			"content": prompt,
		},
		"parent_tool_use_id": nil,
	})

	// Drain stderr in background
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("[claude stderr][%s] %s", s.ID, sc.Text())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			log.Printf("[parse error][%s] %s: %s", s.ID, err, line[:min(len(line), 200)])
			continue
		}

		eventType, _ := event["type"].(string)

		switch eventType {
		case "control_request":
			// Permission request from Claude
			requestID, _ := event["request_id"].(string)
			request, _ := event["request"].(map[string]interface{})
			subtype, _ := request["subtype"].(string)

			if subtype == "can_use_tool" {
				toolName, _ := request["tool_name"].(string)
				toolInput := request["input"]
				reason, _ := request["decision_reason"].(string)
				suggestions := request["permission_suggestions"]

				// Create response channel
				ch := make(chan PermResponse, 1)
				s.mu.Lock()
				s.permChans[requestID] = ch
				s.mu.Unlock()

				// Send permission request to browser
				broadcast(ServerMsg{
					Type:            "permission_request",
					SessionID:       s.ID,
					PermID:          requestID,
					PermTool:        toolName,
					PermInput:       toolInput,
					PermReason:      reason,
					PermSuggestions: suggestions,
				})

				// Wait for browser response (with timeout)
				var resp PermResponse
				select {
				case resp = <-ch:
				case <-time.After(5 * time.Minute):
					resp = PermResponse{Allow: false}
				}

				s.mu.Lock()
				delete(s.permChans, requestID)
				s.mu.Unlock()

				// Send control response to Claude
				if resp.Allow {
					respPayload := map[string]interface{}{
						"behavior":     "allow",
						"updatedInput": toolInput,
					}
					if len(resp.Permissions) > 0 {
						respPayload["updatedPermissions"] = resp.Permissions
					}
					writeJSON(map[string]interface{}{
						"type": "control_response",
						"response": map[string]interface{}{
							"subtype":    "success",
							"request_id": requestID,
							"response":   respPayload,
						},
					})
				} else {
					writeJSON(map[string]interface{}{
						"type": "control_response",
						"response": map[string]interface{}{
							"subtype":    "success",
							"request_id": requestID,
							"response": map[string]interface{}{
								"behavior": "deny",
								"message":  "User denied this action",
							},
						},
					})
				}
			}

		case "control_response":
			// Response to our initialize request, ignore

		case "result":
			handleStreamEvent(s, event, broadcast)
			// Close stdin so claude exits
			stdin.Close()

		default:
			handleStreamEvent(s, event, broadcast)
		}
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("[claude exit][%s] %v", s.ID, err)
	}

	broadcast(ServerMsg{Type: "done", SessionID: s.ID})
}

func handleStreamEvent(s *Session, event map[string]interface{}, broadcast func(ServerMsg)) {
	eventType, _ := event["type"].(string)

	switch eventType {
	case "system":
		// Capture session_id from init message
		if sid, ok := event["session_id"].(string); ok && sid != "" {
			s.mu.Lock()
			if s.ClaudeID == "" {
				s.ClaudeID = sid
			}
			s.mu.Unlock()
		}

	case "assistant":
		msg, ok := event["message"].(map[string]interface{})
		if !ok {
			return
		}
		content, ok := msg["content"].([]interface{})
		if !ok {
			return
		}
		for _, block := range content {
			b, ok := block.(map[string]interface{})
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
				broadcast(ServerMsg{Type: "tool_use", SessionID: s.ID, Tool: name, Input: input})
			}
		}
		// Extract usage/cost info
		if usage, ok := msg["usage"].(map[string]interface{}); ok {
			inTok, _ := usage["input_tokens"].(float64)
			outTok, _ := usage["output_tokens"].(float64)
			cacheRead, _ := usage["cache_read_input_tokens"].(float64)
			cacheCreate, _ := usage["cache_creation_input_tokens"].(float64)
			contextUsed := int(inTok + cacheRead + cacheCreate + outTok)
			if inTok > 0 || outTok > 0 {
				broadcast(ServerMsg{
					Type: "cost", SessionID: s.ID,
					Cost: &CostInfo{
						InputTokens:  int(inTok),
						OutputTokens: int(outTok),
						ContextUsed:  contextUsed,
					},
				})
			}
		}

	case "result":
		// Final result message
		if text, ok := event["result"].(string); ok && text != "" {
			broadcast(ServerMsg{Type: "result", SessionID: s.ID, Text: text})
		}
		if sid, ok := event["session_id"].(string); ok && sid != "" {
			s.mu.Lock()
			s.ClaudeID = sid
			s.mu.Unlock()
		}
		// Extract context window from modelUsage
		if mu, ok := event["modelUsage"].(map[string]interface{}); ok {
			for _, v := range mu {
				if info, ok := v.(map[string]interface{}); ok {
					if cw, ok := info["contextWindow"].(float64); ok && cw > 0 {
						broadcast(ServerMsg{
							Type: "cost", SessionID: s.ID,
							Cost: &CostInfo{ContextWindow: int(cw)},
						})
					}
					break
				}
			}
		}

	case "user":
		// Tool results - extract output
		msg, ok := event["message"].(map[string]interface{})
		if !ok {
			return
		}
		content, ok := msg["content"].([]interface{})
		if !ok {
			return
		}
		for _, block := range content {
			b, ok := block.(map[string]interface{})
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
				broadcast(ServerMsg{Type: "tool_result", SessionID: s.ID, Output: truncate(output, 2000)})
			}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}

// --- History scanning ---

func scanHistory() ([]HistoryGroup, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, err
	}

	var groups []HistoryGroup
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirPath := filepath.Join(projectsDir, entry.Name())
		sessionFiles, err := filepath.Glob(filepath.Join(dirPath, "*.jsonl"))
		if err != nil || len(sessionFiles) == 0 {
			continue
		}

		var cwd string
		var sessions []HistorySession
		for _, sf := range sessionFiles {
			info, err := os.Stat(sf)
			if err != nil {
				continue
			}
			sessionID := strings.TrimSuffix(filepath.Base(sf), ".jsonl")
			summary, sessionCwd := getSessionInfo(sf)
			if cwd == "" && sessionCwd != "" {
				cwd = sessionCwd
			}
			sessions = append(sessions, HistorySession{
				ID:      sessionID,
				Summary: summary,
				ModTime: info.ModTime().Format(time.RFC3339),
				ModUnix: info.ModTime().Unix(),
			})
		}

		if cwd == "" {
			// Fallback: derive from directory name (best-effort)
			cwd = "/" + strings.ReplaceAll(entry.Name(), "-", "/")
		}

		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].ModUnix > sessions[j].ModUnix
		})

		groups = append(groups, HistoryGroup{
			Dir:      cwd,
			DirKey:   entry.Name(),
			Sessions: sessions,
		})
	}

	sort.Slice(groups, func(i, j int) bool {
		if len(groups[i].Sessions) == 0 {
			return false
		}
		if len(groups[j].Sessions) == 0 {
			return true
		}
		return groups[i].Sessions[0].ModUnix > groups[j].Sessions[0].ModUnix
	})

	return groups, nil
}

// getSessionInfo reads the first few lines of a JSONL file to extract
// the first user message (as summary) and the cwd.
func getSessionInfo(fpath string) (summary, cwd string) {
	f, err := os.Open(fpath)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for i := 0; i < 30 && scanner.Scan(); i++ {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(scanner.Text()), &obj); err != nil {
			continue
		}
		if c, ok := obj["cwd"].(string); ok && cwd == "" {
			cwd = c
		}
		if obj["type"] == "user" {
			if msg, ok := obj["message"].(map[string]interface{}); ok {
				summary = extractTextContent(msg["content"])
				if summary != "" {
					summary = truncate(summary, 120)
					return
				}
			}
		}
	}
	return
}

func extractTextContent(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		for _, block := range c {
			if b, ok := block.(map[string]interface{}); ok {
				if b["type"] == "text" {
					if t, ok := b["text"].(string); ok && t != "" {
						return t
					}
				}
			}
		}
	}
	return ""
}

type SessionUsage struct {
	ContextUsed   int `json:"context_used"`
	ContextWindow int `json:"context_window"`
}

// parseSessionMessages reads a JSONL file and returns all displayable messages,
// the Claude session ID, and usage stats.
func parseSessionMessages(jsonlPath string) (msgs []HistoryMessage, claudeID string, usage SessionUsage, err error) {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return nil, "", usage, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}

		// Skip sidechains
		if sc, ok := obj["isSidechain"].(bool); ok && sc {
			continue
		}

		msgType, _ := obj["type"].(string)

		switch msgType {
		case "user":
			if sid, ok := obj["sessionId"].(string); ok && sid != "" {
				claudeID = sid
			}
			msg, ok := obj["message"].(map[string]interface{})
			if !ok {
				continue
			}
			content := msg["content"]
			switch c := content.(type) {
			case string:
				if c != "" {
					msgs = append(msgs, HistoryMessage{Type: "user", Text: c})
				}
			case []interface{}:
				for _, block := range c {
					b, ok := block.(map[string]interface{})
					if !ok {
						continue
					}
					bt, _ := b["type"].(string)
					switch bt {
					case "text":
						if t, _ := b["text"].(string); t != "" {
							msgs = append(msgs, HistoryMessage{Type: "user", Text: t})
						}
					case "tool_result":
						output := ""
						switch rc := b["content"].(type) {
						case string:
							output = rc
						default:
							j, _ := json.Marshal(rc)
							output = string(j)
						}
						msgs = append(msgs, HistoryMessage{Type: "tool_result", Output: truncate(output, 2000)})
					}
				}
			}

		case "assistant":
			msg, ok := obj["message"].(map[string]interface{})
			if !ok {
				continue
			}
			// Track latest usage for context
			if u, ok := msg["usage"].(map[string]interface{}); ok {
				inTok, _ := u["input_tokens"].(float64)
				outTok, _ := u["output_tokens"].(float64)
				cacheRead, _ := u["cache_read_input_tokens"].(float64)
				cacheCreate, _ := u["cache_creation_input_tokens"].(float64)
				ctx := int(inTok + cacheRead + cacheCreate + outTok)
				if ctx > 0 {
					usage.ContextUsed = ctx
				}
			}
			content, ok := msg["content"].([]interface{})
			if !ok {
				continue
			}
			for _, block := range content {
				b, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				blockType, _ := b["type"].(string)
				switch blockType {
				case "text":
					text, _ := b["text"].(string)
					if text != "" {
						msgs = append(msgs, HistoryMessage{Type: "assistant", Text: text})
					}
				case "tool_use":
					name, _ := b["name"].(string)
					input := b["input"]
					msgs = append(msgs, HistoryMessage{Type: "tool_use", Tool: name, Input: input})
				}
			}

		case "result":
			if sid, ok := obj["session_id"].(string); ok && sid != "" {
				claudeID = sid
			}
			if mu, ok := obj["modelUsage"].(map[string]interface{}); ok {
				for _, v := range mu {
					if info, ok := v.(map[string]interface{}); ok {
						if cw, ok := info["contextWindow"].(float64); ok && cw > 0 {
							usage.ContextWindow = int(cw)
						}
					}
					break
				}
			}
		}
	}

	return msgs, claudeID, usage, scanner.Err()
}

// --- WebSocket hub ---

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Client struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *Client) Send(msg ServerMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	c.conn.WriteJSON(msg)
}

type Hub struct {
	clients  map[*Client]bool
	sessions *SessionManager
	mu       sync.RWMutex
}

func NewHub() *Hub {
	return &Hub{
		clients:  make(map[*Client]bool),
		sessions: NewSessionManager(),
	}
}

func (h *Hub) addClient(c *Client) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
}

func (h *Hub) removeClient(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *Hub) startTurn(s *Session, text string) {
	s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text})
	h.broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text})
	h.broadcast(ServerMsg{Type: "running", SessionID: s.ID})
	logBroadcast := func(msg ServerMsg) {
		s.Append(msg)
		h.broadcast(msg)
	}
	go func() {
		runClaudeTurn(s, text, logBroadcast)
		// Drain queued messages
		for {
			s.mu.Lock()
			if len(s.QueuedMsgs) == 0 {
				s.mu.Unlock()
				return
			}
			next := s.QueuedMsgs[0]
			s.QueuedMsgs = s.QueuedMsgs[1:]
			s.mu.Unlock()
			// Convert queued to active
			s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})
			h.broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})
			runClaudeTurn(s, next, logBroadcast)
		}
	}()
}

func (h *Hub) broadcast(msg ServerMsg) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		c.Send(msg)
	}
}

func (h *Hub) handleClient(c *Client) {
	defer func() {
		h.removeClient(c)
		c.conn.Close()
	}()
	h.addClient(c)

	// Send current sessions on connect
	c.Send(ServerMsg{Type: "sessions", Sessions: h.sessions.List()})

	for {
		var msg ClientMsg
		if err := c.conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("ws error: %v", err)
			}
			return
		}

		switch msg.Type {
		case "new_session":
			cwd := msg.Cwd
			if cwd == "" {
				home, _ := os.UserHomeDir()
				cwd = home
			}
			// Expand ~
			if strings.HasPrefix(cwd, "~/") {
				home, _ := os.UserHomeDir()
				cwd = home + cwd[1:]
			}
			s := h.sessions.Create(cwd)
			c.Send(ServerMsg{Type: "session_created", SessionID: s.ID, Text: cwd})
			h.broadcast(ServerMsg{Type: "sessions", Sessions: h.sessions.List()})

		case "message":
			s := h.sessions.Get(msg.SessionID)
			if s == nil {
				c.Send(ServerMsg{Type: "error", Error: "unknown session"})
				continue
			}
			s.mu.Lock()
			if s.Running {
				s.QueuedMsgs = append(s.QueuedMsgs, msg.Text)
				s.mu.Unlock()
				// Log queued message and notify all clients
				s.Append(ServerMsg{Type: "queued_message", SessionID: s.ID, Text: msg.Text})
				h.broadcast(ServerMsg{Type: "queued_message", SessionID: s.ID, Text: msg.Text})
				continue
			}
			s.mu.Unlock()
			h.startTurn(s, msg.Text)

		case "permission_response":
			s := h.sessions.Get(msg.SessionID)
			if s == nil {
				continue
			}
			s.mu.Lock()
			ch, ok := s.permChans[msg.PermID]
			s.mu.Unlock()
			if ok {
				var perms []interface{}
				if msg.PermSuggestion != nil {
					perms = []interface{}{msg.PermSuggestion}
					// Track mode changes
					if sm, ok := msg.PermSuggestion.(map[string]interface{}); ok {
						if sm["type"] == "setMode" {
							if mode, ok := sm["mode"].(string); ok {
								s.mu.Lock()
								s.PermissionMode = mode
								s.mu.Unlock()
								h.broadcast(ServerMsg{Type: "permission_mode", SessionID: msg.SessionID, PermMode: mode})
							}
						}
					}
				}
				ch <- PermResponse{Allow: msg.PermAllow, Permissions: perms}
			}

		case "set_permission_mode":
			s := h.sessions.Get(msg.SessionID)
			if s == nil {
				continue
			}
			s.mu.Lock()
			s.PermissionMode = msg.PermMode
			writeJSON := s.writeJSON
			s.mu.Unlock()
			if writeJSON != nil {
				// Send control request to change mode
				writeJSON(map[string]interface{}{
					"type":       "control_request",
					"request_id": fmt.Sprintf("mode_%d", time.Now().UnixNano()),
					"request": map[string]interface{}{
						"subtype": "set_permission_mode",
						"mode":    msg.PermMode,
					},
				})
			}
			h.broadcast(ServerMsg{Type: "permission_mode", SessionID: msg.SessionID, PermMode: msg.PermMode})

		case "switch_session":
			s := h.sessions.Get(msg.SessionID)
			if s == nil {
				c.Send(ServerMsg{Type: "error", Error: "unknown session"})
				continue
			}
			c.Send(ServerMsg{Type: "session_created", SessionID: s.ID, Text: s.Cwd})
			s.Replay(c)

		case "list_sessions":
			c.Send(ServerMsg{Type: "sessions", Sessions: h.sessions.List()})

		case "list_history":
			groups, err := scanHistory()
			if err != nil {
				c.Send(ServerMsg{Type: "error", Error: fmt.Sprintf("scan history: %v", err)})
				continue
			}
			c.Send(ServerMsg{Type: "history", History: groups})

		case "load_session":
			// Validate inputs to prevent path traversal
			if strings.Contains(msg.DirKey, "/") || strings.Contains(msg.DirKey, "..") ||
				strings.Contains(msg.HistoryID, "/") || strings.Contains(msg.HistoryID, "..") {
				c.Send(ServerMsg{Type: "error", Error: "invalid session reference"})
				continue
			}
			home, _ := os.UserHomeDir()
			jsonlPath := filepath.Join(home, ".claude", "projects", msg.DirKey, msg.HistoryID+".jsonl")

			// Reuse existing session if already loaded
			if s := h.sessions.FindByJSONLPath(jsonlPath); s != nil {
				c.Send(ServerMsg{Type: "session_created", SessionID: s.ID, Text: s.Cwd})
				s.Replay(c)
				continue
			}

			allMsgs, claudeID, sessUsage, err := parseSessionMessages(jsonlPath)
			if err != nil {
				c.Send(ServerMsg{Type: "error", Error: fmt.Sprintf("load session: %v", err)})
				continue
			}

			cwd := ""
			if len(allMsgs) > 0 {
				_, cwd = getSessionInfo(jsonlPath)
			}
			if cwd == "" {
				cwd = "/" + strings.ReplaceAll(msg.DirKey, "-", "/")
			}

			s := h.sessions.Create(cwd)
			s.mu.Lock()
			s.ClaudeID = claudeID
			s.JSONLPath = jsonlPath
			s.mu.Unlock()

			// Populate session log from history
			for _, m := range allMsgs {
				sm := ServerMsg{SessionID: s.ID}
				switch m.Type {
				case "user":
					sm.Type = "user_message"
					sm.Text = m.Text
				case "assistant":
					sm.Type = "text"
					sm.Text = m.Text
				case "tool_use":
					sm.Type = "tool_use"
					sm.Tool = m.Tool
					sm.Input = m.Input
				case "tool_result":
					sm.Type = "tool_result"
					sm.Output = m.Output
				default:
					continue
				}
				s.Log = append(s.Log, sm)
			}
			if sessUsage.ContextUsed > 0 || sessUsage.ContextWindow > 0 {
				s.Log = append(s.Log, ServerMsg{
					Type: "cost", SessionID: s.ID,
					Cost: &CostInfo{
						ContextUsed:   sessUsage.ContextUsed,
						ContextWindow: sessUsage.ContextWindow,
					},
				})
			}

			h.broadcast(ServerMsg{Type: "sessions", Sessions: h.sessions.List()})
			c.Send(ServerMsg{Type: "session_created", SessionID: s.ID, Text: s.Cwd})
			s.Replay(c)
		}
	}
}

// --- Main ---

func main() {
	addr := flag.String("addr", ":8222", "listen address")
	flag.Parse()

	hub := NewHub()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data, _ := staticFiles.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("upgrade error: %v", err)
			return
		}
		client := &Client{conn: conn}
		hub.handleClient(client)
	})

	log.Printf("Monet Droid listening on %s", *addr)
	log.Printf("open http://localhost%s on your phone", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatal(err)
	}
}
