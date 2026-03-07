package main

import (
	"bufio"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML string

// --- Internal types ---

type ServerMsg struct {
	Type            string      `json:"type"`
	SessionID       string      `json:"session_id,omitempty"`
	Text            string      `json:"text,omitempty"`
	Tool            string      `json:"tool,omitempty"`
	Input           interface{} `json:"input,omitempty"`
	Output          string      `json:"output,omitempty"`
	Error           string      `json:"error,omitempty"`
	Cost            *CostInfo   `json:"cost,omitempty"`
	PermID          string      `json:"perm_id,omitempty"`
	PermTool        string      `json:"perm_tool,omitempty"`
	PermInput       interface{} `json:"perm_input,omitempty"`
	PermReason      string      `json:"perm_reason,omitempty"`
	PermSuggestions interface{} `json:"perm_suggestions,omitempty"`
	PermMode        string      `json:"perm_mode,omitempty"`
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
	Type   string      `json:"type"`
	Text   string      `json:"text,omitempty"`
	Tool   string      `json:"tool,omitempty"`
	Input  interface{} `json:"input,omitempty"`
	Output string      `json:"output,omitempty"`
}

type SessionUsage struct {
	ContextUsed   int
	ContextWindow int
}

// --- Session management ---

type PermResponse struct {
	Allow       bool
	Permissions []interface{}
}

type Session struct {
	ID             string
	ClaudeID       string
	Cwd            string
	PermissionMode string
	MessageCount   int
	Running        bool
	CreatedAt      time.Time
	JSONLPath      string
	Log        []ServerMsg
	QueuedText string // pending message to send when current turn finishes
	CostAccum      CostInfo // accumulated cost for this session
	permChans      map[string]chan PermResponse
	writeJSON      func(interface{})
	mu             sync.Mutex
}

func (s *Session) Append(msg ServerMsg) {
	s.mu.Lock()
	s.Log = append(s.Log, msg)
	s.mu.Unlock()
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
	s := &Session{ID: id, Cwd: cwd, CreatedAt: time.Now()}
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

func (sm *SessionManager) List() []*Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make([]*Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		out = append(out, s)
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
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("stdin pipe: %v", err)})
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("stdout pipe: %v", err)})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("stderr pipe: %v", err)})
		return
	}

	if err := cmd.Start(); err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("start: %v", err)})
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

	writeJSON(map[string]interface{}{
		"type": "control_request", "request_id": "init_1",
		"request": map[string]interface{}{"subtype": "initialize"},
	})
	writeJSON(map[string]interface{}{
		"type": "user", "session_id": "",
		"message":            map[string]interface{}{"role": "user", "content": prompt},
		"parent_tool_use_id": nil,
	})

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
			requestID, _ := event["request_id"].(string)
			request, _ := event["request"].(map[string]interface{})
			subtype, _ := request["subtype"].(string)
			if subtype == "can_use_tool" {
				toolName, _ := request["tool_name"].(string)
				toolInput := request["input"]
				reason, _ := request["decision_reason"].(string)
				suggestions := request["permission_suggestions"]

				ch := make(chan PermResponse, 1)
				s.mu.Lock()
				s.permChans[requestID] = ch
				s.mu.Unlock()

				broadcast(ServerMsg{
					Type: "permission_request", SessionID: s.ID,
					PermID: requestID, PermTool: toolName, PermInput: toolInput,
					PermReason: reason, PermSuggestions: suggestions,
				})

				var resp PermResponse
				select {
				case resp = <-ch:
				case <-time.After(5 * time.Minute):
					resp = PermResponse{Allow: false}
				}

				s.mu.Lock()
				delete(s.permChans, requestID)
				s.mu.Unlock()

				if resp.Allow {
					payload := map[string]interface{}{"behavior": "allow", "updatedInput": toolInput}
					if len(resp.Permissions) > 0 {
						payload["updatedPermissions"] = resp.Permissions
					}
					writeJSON(map[string]interface{}{
						"type": "control_response",
						"response": map[string]interface{}{
							"subtype": "success", "request_id": requestID, "response": payload,
						},
					})
				} else {
					writeJSON(map[string]interface{}{
						"type": "control_response",
						"response": map[string]interface{}{
							"subtype": "success", "request_id": requestID,
							"response": map[string]interface{}{"behavior": "deny", "message": "User denied this action"},
						},
					})
				}
			}
		case "control_response":
			// ignore
		case "result":
			handleStreamEvent(s, event, broadcast)
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
		if usage, ok := msg["usage"].(map[string]interface{}); ok {
			inTok, _ := usage["input_tokens"].(float64)
			outTok, _ := usage["output_tokens"].(float64)
			cacheRead, _ := usage["cache_read_input_tokens"].(float64)
			cacheCreate, _ := usage["cache_creation_input_tokens"].(float64)
			contextUsed := int(inTok + cacheRead + cacheCreate + outTok)
			if inTok > 0 || outTok > 0 {
				broadcast(ServerMsg{
					Type: "cost", SessionID: s.ID,
					Cost: &CostInfo{InputTokens: int(inTok), OutputTokens: int(outTok), ContextUsed: contextUsed},
				})
			}
		}

	case "result":
		if text, ok := event["result"].(string); ok && text != "" {
			broadcast(ServerMsg{Type: "result", SessionID: s.ID, Text: text})
		}
		if sid, ok := event["session_id"].(string); ok && sid != "" {
			s.mu.Lock()
			s.ClaudeID = sid
			s.mu.Unlock()
		}
		if mu, ok := event["modelUsage"].(map[string]interface{}); ok {
			for _, v := range mu {
				if info, ok := v.(map[string]interface{}); ok {
					if cw, ok := info["contextWindow"].(float64); ok && cw > 0 {
						broadcast(ServerMsg{Type: "cost", SessionID: s.ID, Cost: &CostInfo{ContextWindow: int(cw)}})
					}
					break
				}
			}
		}

	case "user":
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
				ID: sessionID, Summary: summary,
				ModTime: info.ModTime().Format(time.RFC3339), ModUnix: info.ModTime().Unix(),
			})
		}
		if cwd == "" {
			cwd = "/" + strings.ReplaceAll(entry.Name(), "-", "/")
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].ModUnix > sessions[j].ModUnix })
		groups = append(groups, HistoryGroup{Dir: cwd, DirKey: entry.Name(), Sessions: sessions})
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
			switch c := msg["content"].(type) {
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
					if text, _ := b["text"].(string); text != "" {
						msgs = append(msgs, HistoryMessage{Type: "assistant", Text: text})
					}
				case "tool_use":
					name, _ := b["name"].(string)
					msgs = append(msgs, HistoryMessage{Type: "tool_use", Tool: name, Input: b["input"]})
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

// --- HTML rendering ---

var (
	reCodeBlock  = regexp.MustCompile("(?s)```\\w*\n(.*?)```")
	reCodeBlock2 = regexp.MustCompile("(?s)```(.*?)```")
	reInlineCode = regexp.MustCompile("`([^`]+)`")
	reBold       = regexp.MustCompile(`\*\*(.+?)\*\*`)
)

func esc(s string) string { return html.EscapeString(s) }

func renderMarkdown(text string) string {
	s := esc(text)
	s = reCodeBlock.ReplaceAllString(s, "<pre>$1</pre>")
	s = reCodeBlock2.ReplaceAllString(s, "<pre>$1</pre>")
	s = reInlineCode.ReplaceAllString(s, "<code>$1</code>")
	s = reBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = strings.ReplaceAll(s, "\n", "<br>")
	return s
}

func formatToolInput(tool string, input interface{}) string {
	m, ok := input.(map[string]interface{})
	if !ok {
		j, _ := json.MarshalIndent(input, "", "  ")
		return string(j)
	}
	filePath, _ := m["file_path"].(string)
	if filePath == "" {
		filePath, _ = m["path"].(string)
	}
	switch tool {
	case "Bash":
		cmd, _ := m["command"].(string)
		if cmd != "" {
			return cmd
		}
	case "Read", "FileRead":
		if filePath != "" {
			return filePath
		}
	case "Write", "FileWrite":
		content, _ := m["content"].(string)
		if len(content) > 200 {
			content = content[:200]
		}
		return filePath + "\n" + content
	case "Edit", "FileEdit":
		var lines []string
		lines = append(lines, filePath)
		if old, _ := m["old_string"].(string); old != "" {
			lines = append(lines, "--- old ---", old)
		}
		if new_, _ := m["new_string"].(string); new_ != "" {
			lines = append(lines, "+++ new +++", new_)
		}
		return strings.Join(lines, "\n")
	case "Grep":
		if p, _ := m["pattern"].(string); p != "" {
			return p
		}
	case "Glob":
		if p, _ := m["pattern"].(string); p != "" {
			return p
		}
	}
	j, _ := json.MarshalIndent(input, "", "  ")
	return string(j)
}

func formatPermDetail(tool string, input interface{}) string {
	m, ok := input.(map[string]interface{})
	if !ok {
		j, _ := json.MarshalIndent(input, "", "  ")
		return string(j)
	}
	filePath, _ := m["file_path"].(string)
	if filePath == "" {
		filePath, _ = m["path"].(string)
	}
	switch tool {
	case "Bash":
		cmd, _ := m["command"].(string)
		desc, _ := m["description"].(string)
		if desc != "" {
			return desc + "\n\n" + cmd
		}
		return cmd
	case "Edit", "FileEdit":
		var lines []string
		lines = append(lines, filePath)
		if old, _ := m["old_string"].(string); old != "" {
			lines = append(lines, "--- old ---", old)
		}
		if new_, _ := m["new_string"].(string); new_ != "" {
			lines = append(lines, "+++ new +++", new_)
		}
		return strings.Join(lines, "\n")
	case "Write", "FileWrite":
		content, _ := m["content"].(string)
		return filePath + "\n\n" + content
	case "Read", "FileRead":
		return filePath
	}
	j, _ := json.MarshalIndent(input, "", "  ")
	return string(j)
}

func renderMsg(msg ServerMsg) string {
	switch msg.Type {
	case "user_message":
		return fmt.Sprintf(`<div class="msg msg-user"><div class="msg-bubble">%s</div></div>`, esc(msg.Text))
	case "text":
		return fmt.Sprintf(`<div class="msg msg-assistant"><div class="msg-bubble">%s</div></div>`, renderMarkdown(msg.Text))
	case "tool_use":
		detail := formatToolInput(msg.Tool, msg.Input)
		return fmt.Sprintf(`<div class="msg msg-tool"><details class="tool-chip"><summary class="tool-name">⚙ %s</summary><div class="tool-detail">%s</div></details></div>`, esc(msg.Tool), esc(detail))
	case "tool_result":
		return fmt.Sprintf(`<div class="msg msg-tool"><details class="tool-result-chip"><summary class="tool-result-summary">result</summary><div class="tool-result-full">%s</div></details></div>`, esc(msg.Output))
	case "error":
		return fmt.Sprintf(`<div class="msg"><div class="msg-error">✗ %s</div></div>`, esc(msg.Error))
	case "permission_request":
		return renderPermission(msg)
	case "result":
		return "" // result text is already sent as a text event
	}
	return ""
}

func renderPermission(msg ServerMsg) string {
	detail := formatPermDetail(msg.PermTool, msg.PermInput)
	var suggBtns strings.Builder
	if suggestions, ok := msg.PermSuggestions.([]interface{}); ok {
		for _, s := range suggestions {
			sm, ok := s.(map[string]interface{})
			if !ok {
				continue
			}
			var label string
			switch sm["type"] {
			case "setMode":
				mode, _ := sm["mode"].(string)
				if mode == "acceptEdits" {
					label = "Accept Edits"
				} else {
					label = mode
				}
			case "addDirectories":
				dirs, _ := sm["directories"].([]interface{})
				var ds []string
				for _, d := range dirs {
					if ds_, ok := d.(string); ok {
						ds = append(ds, ds_)
					}
				}
				label = "Add " + strings.Join(ds, ", ")
			default:
				t, _ := sm["type"].(string)
				label = t
			}
			sJSON, _ := json.Marshal(s)
			fmt.Fprintf(&suggBtns,
				`<form hx-post="/perm" hx-swap="none" style="flex:1"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="perm_id" value="%s"><input type="hidden" name="allow" value="true"><input type="hidden" name="suggestion" value="%s"><button type="submit" class="perm-allow" style="width:100%%;font-size:11px">%s</button></form>`,
				esc(msg.SessionID), esc(msg.PermID), esc(string(sJSON)), esc(label),
			)
		}
	}

	return fmt.Sprintf(`<div class="perm-prompt" id="perm-%s">
<div class="perm-header">Permission Required</div>
<div class="perm-tool">⚙ %s</div>
%s
<div class="perm-detail">%s</div>
<div class="perm-actions" id="perm-actions-%s">
<form hx-post="/perm" hx-swap="none" style="flex:1"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="perm_id" value="%s"><input type="hidden" name="allow" value="false"><button type="submit" class="perm-deny" style="width:100%%">Deny</button></form>
<form hx-post="/perm" hx-swap="none" style="flex:1"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="perm_id" value="%s"><input type="hidden" name="allow" value="true"><button type="submit" class="perm-allow" style="width:100%%">Allow</button></form>
%s
</div></div>`,
		esc(msg.PermID), esc(msg.PermTool),
		func() string {
			if msg.PermReason != "" {
				return fmt.Sprintf(`<div class="perm-reason">%s</div>`, esc(msg.PermReason))
			}
			return ""
		}(),
		esc(detail), esc(msg.PermID),
		esc(msg.SessionID), esc(msg.PermID),
		esc(msg.SessionID), esc(msg.PermID),
		suggBtns.String(),
	)
}

func renderCostBar(s *Session) string {
	s.mu.Lock()
	c := s.CostAccum
	s.mu.Unlock()
	if c.InputTokens == 0 && c.OutputTokens == 0 && c.ContextUsed == 0 {
		return ""
	}
	text := fmt.Sprintf("↓%s ↑%s", fmtK(c.InputTokens), fmtK(c.OutputTokens))
	if c.ContextUsed > 0 && c.ContextWindow > 0 {
		pct := 100 * c.ContextUsed / c.ContextWindow
		text += fmt.Sprintf(" · context %s/%s (%d%%)", fmtK(c.ContextUsed), fmtK(c.ContextWindow), pct)
	} else if c.ContextUsed > 0 {
		text += fmt.Sprintf(" · context %s", fmtK(c.ContextUsed))
	}
	return text
}

func renderQueueBar(sessionID, text string) string {
	if text == "" {
		return oobSwap("queue-bar", "innerHTML", "")
	}
	return oobSwap("queue-bar", "innerHTML", fmt.Sprintf(
		`<div class="queue-content"><span class="queue-label">queued:</span> <span class="queue-text">%s</span><form hx-post="/cancel-queue" hx-swap="none" style="display:inline"><input type="hidden" name="session_id" value="%s"><button type="submit" class="queue-cancel">✕</button></form></div>`,
		esc(text), esc(sessionID),
	))
}

func fmtK(n int) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	}
}

func shortPath(p string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// --- SSE format ---

func formatSSE(event, data string) string {
	var buf strings.Builder
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\n")
	for _, line := range strings.Split(data, "\n") {
		buf.WriteString("data: ")
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
	return buf.String()
}

func oobSwap(id, strategy, content string) string {
	return fmt.Sprintf(`<div id="%s" hx-swap-oob="%s">%s</div>`, id, strategy, content)
}

// --- SSE Hub ---

type SSEClient struct {
	id        string
	sessionID string
	events    chan string
	done      chan struct{}
	mu        sync.Mutex
}

func (c *SSEClient) Send(event string) {
	select {
	case c.events <- event:
	default:
		// drop if buffer full
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
	sessions *SessionManager
	mu       sync.RWMutex
}

func NewHub() *Hub {
	return &Hub{
		clients:  make(map[string]*SSEClient),
		sessions: NewSessionManager(),
	}
}

func (h *Hub) getOrCreateClient(cid string) *SSEClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.clients[cid]; ok {
		return c
	}
	c := &SSEClient{id: cid, events: make(chan string, 64), done: make(chan struct{})}
	h.clients[cid] = c
	return c
}

func (h *Hub) removeClient(cid string) {
	h.mu.Lock()
	delete(h.clients, cid)
	h.mu.Unlock()
}

func (h *Hub) broadcastToSession(sessionID string, event string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	sent := 0
	for _, c := range h.clients {
		if c.ActiveSession() == sessionID {
			c.Send(event)
			sent++
		}
	}
	if sent == 0 {
		log.Printf("[broadcastToSession] no clients viewing session %s (total clients: %d)", sessionID, len(h.clients))
	}
}

func (h *Hub) sendToClient(cid string, event string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if c, ok := h.clients[cid]; ok {
		c.Send(event)
	}
}

// broadcast sends a ServerMsg to all clients viewing that session.
// Also handles cost accumulation on the session.
func (h *Hub) broadcast(msg ServerMsg) {
	sessionID := msg.SessionID
	if sessionID == "" {
		return
	}
	s := h.sessions.Get(sessionID)

	// Accumulate cost
	if msg.Type == "cost" && msg.Cost != nil && s != nil {
		s.mu.Lock()
		s.CostAccum.InputTokens += msg.Cost.InputTokens
		s.CostAccum.OutputTokens += msg.Cost.OutputTokens
		if msg.Cost.ContextUsed > 0 {
			s.CostAccum.ContextUsed = msg.Cost.ContextUsed
		}
		if msg.Cost.ContextWindow > 0 {
			s.CostAccum.ContextWindow = msg.Cost.ContextWindow
		}
		s.mu.Unlock()
	}

	// Render message HTML
	msgHTML := renderMsg(msg)

	// Build SSE event with OOB swaps
	var parts []string
	if msgHTML != "" {
		parts = append(parts, oobSwap("messages", "beforeend", msgHTML))
	}

	// Include cost bar update
	if msg.Type == "cost" && s != nil {
		costText := renderCostBar(s)
		parts = append(parts, oobSwap("cost-bar", "innerHTML", costText))
	}

	thinkingHTML := `<div class="thinking-indicator" id="thinking"><span></span><span></span><span></span></div>`
	emptyThinking := oobSwap("thinking", "outerHTML", `<div id="thinking"></div>`)

	// Running/done indicator
	if msg.Type == "running" {
		parts = append(parts, oobSwap("running-dot", "outerHTML", `<span class="di-running" id="running-dot"></span>`))
		parts = append(parts, oobSwap("messages", "beforeend", thinkingHTML))
	}
	if msg.Type == "done" {
		parts = append(parts, oobSwap("running-dot", "outerHTML", `<span id="running-dot" style="display:none"></span>`))
		parts = append(parts, emptyThinking)
	}
	// Replace thinking with message, then re-add thinking after tool results
	if msgHTML != "" {
		parts = append(parts, oobSwap("thinking", "outerHTML", ""))
		if msg.Type == "tool_use" || msg.Type == "tool_result" {
			// Claude is still working — re-add thinking after this message
			parts = append(parts, oobSwap("messages", "beforeend", thinkingHTML))
		}
	}

	// Permission mode
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
			modeHTML = fmt.Sprintf(`<span class="mode-label">%s</span><form hx-post="/mode" hx-swap="none" style="display:inline"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="mode" value="default"><button type="submit" class="mode-reset">reset to default</button></form>`, esc(label), esc(sessionID))
		}
		parts = append(parts, oobSwap("mode-bar", "innerHTML", modeHTML))
	}

	if len(parts) == 0 {
		return
	}
	event := formatSSE("htmx", strings.Join(parts, "\n"))
	log.Printf("[broadcast][%s] type=%s parts=%d", sessionID, msg.Type, len(parts))
	h.broadcastToSession(sessionID, event)
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
		// Run queued message if any
		s.mu.Lock()
		next := s.QueuedText
		s.QueuedText = ""
		s.mu.Unlock()
		if next != "" {
			// Clear queue display
			h.broadcastToSession(s.ID, formatSSE("htmx", renderQueueBar(s.ID, "")))
			s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})
			h.broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})
			h.broadcast(ServerMsg{Type: "running", SessionID: s.ID})
			runClaudeTurn(s, next, logBroadcast)
		}
	}()
}

// replaySession sends the full session state to a specific client
func (h *Hub) replaySession(cid string, s *Session) {
	s.mu.Lock()
	log_ := make([]ServerMsg, len(s.Log))
	copy(log_, s.Log)
	running := s.Running
	permMode := s.PermissionMode
	cwd := s.Cwd
	queuedText := s.QueuedText
	s.mu.Unlock()

	// Render all messages
	var msgsHTML strings.Builder
	for _, msg := range log_ {
		msgsHTML.WriteString(renderMsg(msg))
	}

	// Build one big SSE event with all OOB swaps
	var parts []string
	parts = append(parts, oobSwap("session-label", "innerHTML", esc(shortPath(cwd))))
	parts = append(parts, oobSwap("messages", "innerHTML", msgsHTML.String()))
	parts = append(parts, oobSwap("cost-bar", "innerHTML", renderCostBar(s)))

	if running {
		parts = append(parts, oobSwap("running-dot", "outerHTML", `<span class="di-running" id="running-dot"></span>`))
	} else {
		parts = append(parts, oobSwap("running-dot", "outerHTML", `<span id="running-dot" style="display:none"></span>`))
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
		modeHTML = fmt.Sprintf(`<span class="mode-label">%s</span><form hx-post="/mode" hx-swap="none" style="display:inline"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="mode" value="default"><button type="submit" class="mode-reset">reset to default</button></form>`, esc(label), esc(s.ID))
	}
	parts = append(parts, oobSwap("mode-bar", "innerHTML", modeHTML))
	parts = append(parts, renderQueueBar(s.ID, queuedText))

	event := formatSSE("htmx", strings.Join(parts, "\n"))
	h.sendToClient(cid, event)
}

// --- HTTP handlers ---

func getCID(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie("cid")
	if err == nil && cookie.Value != "" {
		return cookie.Value
	}
	b := make([]byte, 16)
	rand.Read(b)
	cid := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: "cid", Value: cid, Path: "/", MaxAge: 86400 * 365})
	return cid
}

func (h *Hub) handleIndex(w http.ResponseWriter, r *http.Request) {
	getCID(w, r) // ensure cookie is set
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

func (h *Hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	cid := getCID(w, r)
	client := h.getOrCreateClient(cid)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	// Send initial heartbeat
	fmt.Fprint(w, formatSSE("heartbeat", "connected"))
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			h.removeClient(cid)
			return
		case event := <-client.events:
			log.Printf("[sse][%s] sending %d bytes", cid[:8], len(event))
			fmt.Fprint(w, event)
			flusher.Flush()
		case <-time.After(30 * time.Second):
			fmt.Fprint(w, formatSSE("heartbeat", "ping"))
			flusher.Flush()
		}
	}
}

func (h *Hub) handleNewSession(w http.ResponseWriter, r *http.Request) {
	cid := getCID(w, r)
	cwd := r.FormValue("cwd")
	if cwd == "" {
		home, _ := os.UserHomeDir()
		cwd = home
	}
	if strings.HasPrefix(cwd, "~/") {
		home, _ := os.UserHomeDir()
		cwd = home + cwd[1:]
	}

	s := h.sessions.Create(cwd)

	client := h.getOrCreateClient(cid)
	client.SetSession(s.ID)
	h.replaySession(cid, s)

	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(204)
}

func (h *Hub) handleSend(w http.ResponseWriter, r *http.Request) {
	cid := getCID(w, r)
	text := r.FormValue("text")
	if text == "" {
		w.WriteHeader(204)
		return
	}

	client := h.getOrCreateClient(cid)
	sessionID := client.ActiveSession()
	s := h.sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	s.mu.Lock()
	if s.Running {
		if s.QueuedText != "" {
			s.QueuedText += "\n" + text
		} else {
			s.QueuedText = text
		}
		queued := s.QueuedText
		s.mu.Unlock()
		// Update queue display for all viewers
		h.broadcastToSession(s.ID, formatSSE("htmx", renderQueueBar(s.ID, queued)))
	} else {
		s.mu.Unlock()
		h.startTurn(s, text)
	}

	w.WriteHeader(204)
}

func (h *Hub) handlePerm(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	permID := r.FormValue("perm_id")
	allow := r.FormValue("allow") == "true"
	suggestionJSON := r.FormValue("suggestion")

	s := h.sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	s.mu.Lock()
	ch, ok := s.permChans[permID]
	s.mu.Unlock()

	if ok {
		var perms []interface{}
		if suggestionJSON != "" {
			var suggestion interface{}
			if err := json.Unmarshal([]byte(suggestionJSON), &suggestion); err == nil {
				perms = []interface{}{suggestion}
				if sm, ok := suggestion.(map[string]interface{}); ok {
					if sm["type"] == "setMode" {
						if mode, ok := sm["mode"].(string); ok {
							s.mu.Lock()
							s.PermissionMode = mode
							s.mu.Unlock()
							h.broadcast(ServerMsg{Type: "permission_mode", SessionID: sessionID, PermMode: mode})
						}
					}
				}
			}
		}
		ch <- PermResponse{Allow: allow, Permissions: perms}
	}

	// Replace the permission buttons with result text
	var resultHTML string
	if allow {
		label := "Allowed"
		if suggestionJSON != "" {
			label = "Allowed (with suggestion)"
		}
		resultHTML = fmt.Sprintf(`<span style="color:var(--tool);font-size:12px">✓ %s</span>`, esc(label))
	} else {
		resultHTML = `<span style="color:var(--error);font-size:12px">✗ Denied</span>`
	}
	event := formatSSE("htmx", oobSwap("perm-actions-"+permID, "innerHTML", resultHTML))
	h.broadcastToSession(sessionID, event)

	w.WriteHeader(204)
}

func (h *Hub) handleMode(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	mode := r.FormValue("mode")

	s := h.sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	s.mu.Lock()
	s.PermissionMode = mode
	writeJSON := s.writeJSON
	s.mu.Unlock()

	if writeJSON != nil {
		writeJSON(map[string]interface{}{
			"type": "control_request", "request_id": fmt.Sprintf("mode_%d", time.Now().UnixNano()),
			"request": map[string]interface{}{"subtype": "set_permission_mode", "mode": mode},
		})
	}
	h.broadcast(ServerMsg{Type: "permission_mode", SessionID: sessionID, PermMode: mode})

	w.WriteHeader(204)
}

func (h *Hub) handleSwitch(w http.ResponseWriter, r *http.Request) {
	cid := getCID(w, r)
	sessionID := r.FormValue("session_id")

	s := h.sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	client := h.getOrCreateClient(cid)
	client.SetSession(s.ID)
	h.replaySession(cid, s)

	w.WriteHeader(204)
}

func (h *Hub) handleLoad(w http.ResponseWriter, r *http.Request) {
	cid := getCID(w, r)
	dirKey := r.FormValue("dir_key")
	historyID := r.FormValue("history_id")

	if strings.Contains(dirKey, "/") || strings.Contains(dirKey, "..") ||
		strings.Contains(historyID, "/") || strings.Contains(historyID, "..") {
		w.WriteHeader(204)
		return
	}

	home, _ := os.UserHomeDir()
	jsonlPath := filepath.Join(home, ".claude", "projects", dirKey, historyID+".jsonl")

	// Reuse existing session
	if s := h.sessions.FindByJSONLPath(jsonlPath); s != nil {
		client := h.getOrCreateClient(cid)
		client.SetSession(s.ID)
		h.replaySession(cid, s)
		w.WriteHeader(204)
		return
	}

	allMsgs, claudeID, sessUsage, err := parseSessionMessages(jsonlPath)
	if err != nil {
		w.WriteHeader(204)
		return
	}

	cwd := ""
	if len(allMsgs) > 0 {
		_, cwd = getSessionInfo(jsonlPath)
	}
	if cwd == "" {
		cwd = "/" + strings.ReplaceAll(dirKey, "-", "/")
	}

	s := h.sessions.Create(cwd)
	s.mu.Lock()
	s.ClaudeID = claudeID
	s.JSONLPath = jsonlPath
	// Set cost from history
	s.CostAccum.ContextUsed = sessUsage.ContextUsed
	s.CostAccum.ContextWindow = sessUsage.ContextWindow
	s.mu.Unlock()

	// Populate log
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

	client := h.getOrCreateClient(cid)
	client.SetSession(s.ID)
	h.replaySession(cid, s)

	w.WriteHeader(204)
}

func (h *Hub) handleDrawer(w http.ResponseWriter, r *http.Request) {
	var buf strings.Builder

	// Active sessions
	sessions := h.sessions.List()
	if len(sessions) > 0 {
		buf.WriteString(`<div class="drawer-section-label">Active Sessions</div>`)
		for _, s := range sessions {
			s.mu.Lock()
			running := s.Running
			sp := shortPath(s.Cwd)
			sid := s.ID
			mc := s.MessageCount
			s.mu.Unlock()
			runHTML := ""
			if running {
				runHTML = `<span class="di-running"></span> running`
			}
			fmt.Fprintf(&buf,
				`<div class="drawer-item" hx-post="/switch" hx-vals='{"session_id":"%s"}' hx-swap="none" hx-on::after-request="document.getElementById('drawer').hidePopover()"><div class="di-name">%s</div><div class="di-path">%s</div><div class="di-meta">%s %d turns</div></div>`,
				esc(sid), esc(sid), esc(sp), runHTML, mc,
			)
		}
	}

	// History
	groups, err := scanHistory()
	if err == nil && len(groups) > 0 {
		buf.WriteString(`<div class="drawer-section-label">History</div>`)
		for _, group := range groups {
			sp := shortPath(group.Dir)
			fmt.Fprintf(&buf, `<details class="history-group"><summary class="history-group-header">%s <span style="color:var(--text2);font-size:10px">(%d)</span></summary><div class="history-group-items">`, esc(sp), len(group.Sessions))
			for _, sess := range group.Sessions {
				modTime, _ := time.Parse(time.RFC3339, sess.ModTime)
				ago := timeAgo(modTime)
				summary := sess.Summary
				if summary == "" {
					summary = "(empty)"
				}
				fmt.Fprintf(&buf,
					`<div class="history-item" hx-post="/load" hx-vals='{"dir_key":"%s","history_id":"%s"}' hx-swap="none" hx-on::after-request="document.getElementById('drawer').hidePopover()"><div class="hi-summary">%s</div><div class="hi-time">%s</div></div>`,
					esc(group.DirKey), esc(sess.ID), esc(summary), esc(ago),
				)
			}
			buf.WriteString(`</div></details>`)
		}
	}

	if len(sessions) == 0 && (err != nil || len(groups) == 0) {
		buf.WriteString(`<div style="padding:20px;text-align:center;color:var(--text2);font-size:13px">No sessions yet</div>`)
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(buf.String()))
}

func (h *Hub) handleCancelQueue(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	s := h.sessions.Get(sessionID)
	if s != nil {
		s.mu.Lock()
		s.QueuedText = ""
		s.mu.Unlock()
		h.broadcastToSession(sessionID, formatSSE("htmx", renderQueueBar(sessionID, "")))
	}
	w.WriteHeader(204)
}

// --- Main ---

func main() {
	addr := flag.String("addr", ":8222", "listen address")
	flag.Parse()

	hub := NewHub()

	http.HandleFunc("/", hub.handleIndex)
	http.HandleFunc("/events", hub.handleEvents)
	http.HandleFunc("/new", hub.handleNewSession)
	http.HandleFunc("/send", hub.handleSend)
	http.HandleFunc("/perm", hub.handlePerm)
	http.HandleFunc("/mode", hub.handleMode)
	http.HandleFunc("/switch", hub.handleSwitch)
	http.HandleFunc("/load", hub.handleLoad)
	http.HandleFunc("/cancel-queue", hub.handleCancelQueue)
	http.HandleFunc("/drawer", hub.handleDrawer)

	log.Printf("Monet Droid listening on %s", *addr)
	log.Printf("open http://localhost%s", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatal(err)
	}
}
