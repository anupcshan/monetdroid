package monetdroid

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML string

func GetCID(w http.ResponseWriter, r *http.Request) string {
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

func RegisterRoutes(hub *Hub) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", hub.handleIndex)
	mux.HandleFunc("/events", hub.handleEvents)
	mux.HandleFunc("/new", hub.handleNewSession)
	mux.HandleFunc("/send", hub.handleSend)
	mux.HandleFunc("/perm", hub.handlePerm)
	mux.HandleFunc("/mode", hub.handleMode)
	mux.HandleFunc("/switch", hub.handleSwitch)
	mux.HandleFunc("/load", hub.handleLoad)
	mux.HandleFunc("/stop", hub.handleStop)
	mux.HandleFunc("/cancel-queue", hub.handleCancelQueue)
	mux.HandleFunc("/drawer", hub.handleDrawer)
	return mux
}

func (h *Hub) handleIndex(w http.ResponseWriter, r *http.Request) {
	GetCID(w, r)
	html := indexHTML
	if qs := r.URL.RawQuery; qs != "" {
		html = strings.Replace(html, `sse-connect="/events"`, `sse-connect="/events?`+qs+`"`, 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

func sessionURL(s *Session) string {
	s.Mu.Lock()
	claudeID := s.ClaudeID
	cwd := s.Cwd
	s.Mu.Unlock()
	if claudeID != "" {
		return "/?session=" + claudeID
	}
	return "/?cwd=" + cwd
}

// restoreSession finds a session by ClaudeID (in memory or on disk) and assigns it to the client.
func (h *Hub) restoreSession(cid, claudeID string) {
	client := h.GetOrCreateClient(cid)

	// Check in-memory sessions first
	if s := h.Sessions.FindByClaudeID(claudeID); s != nil {
		client.SetSession(s.ID)
		return
	}

	// Find JSONL on disk
	jsonlPath := FindJSONLByClaudeID(claudeID)
	if jsonlPath == "" {
		return
	}
	if s := h.Sessions.FindByJSONLPath(jsonlPath); s != nil {
		client.SetSession(s.ID)
		return
	}

	allMsgs, loadedClaudeID, sessUsage, err := ParseSessionMessages(jsonlPath)
	if err != nil {
		return
	}

	cwd := ""
	if len(allMsgs) > 0 {
		_, cwd = GetSessionInfo(jsonlPath)
	}
	if cwd == "" {
		dirKey := filepath.Base(filepath.Dir(jsonlPath))
		cwd = "/" + strings.ReplaceAll(dirKey, "-", "/")
	}

	s := h.Sessions.Create(cwd)
	s.Mu.Lock()
	s.ClaudeID = loadedClaudeID
	s.JSONLPath = jsonlPath
	s.CostAccum.TotalCostUSD = sessUsage.TotalCostUSD
	s.CostAccum.ContextUsed = sessUsage.ContextUsed
	s.CostAccum.ContextWindow = sessUsage.ContextWindow
	s.Mu.Unlock()

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

	client.SetSession(s.ID)
}

func (h *Hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	cid := GetCID(w, r)
	client := h.GetOrCreateClient(cid)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	fmt.Fprint(w, FormatSSE("heartbeat", "connected"))
	flusher.Flush()

	// Restore session from query params (forwarded from page URL via sse-connect)
	if sessionID := r.URL.Query().Get("session"); sessionID != "" {
		h.restoreSession(cid, sessionID)
	} else if cwd := r.URL.Query().Get("cwd"); cwd != "" {
		if strings.HasPrefix(cwd, "~/") {
			home, _ := os.UserHomeDir()
			cwd = home + cwd[1:]
		}
		s := h.Sessions.Create(cwd)
		client.SetSession(s.ID)
	}
	if sid := client.ActiveSession(); sid != "" {
		if s := h.Sessions.Get(sid); s != nil {
			h.ReplaySession(cid, s)
		}
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			h.RemoveClient(cid)
			return
		case event := <-client.events:
			fmt.Fprint(w, event)
			flusher.Flush()
		case <-time.After(30 * time.Second):
			fmt.Fprint(w, FormatSSE("heartbeat", "ping"))
			flusher.Flush()
		}
	}
}

func (h *Hub) handleNewSession(w http.ResponseWriter, r *http.Request) {
	cid := GetCID(w, r)
	cwd := r.FormValue("cwd")
	if cwd == "" {
		home, _ := os.UserHomeDir()
		cwd = home
	}
	if strings.HasPrefix(cwd, "~/") {
		home, _ := os.UserHomeDir()
		cwd = home + cwd[1:]
	}

	s := h.Sessions.Create(cwd)
	client := h.GetOrCreateClient(cid)
	client.SetSession(s.ID)
	h.ReplaySession(cid, s)

	w.Header().Set("HX-Replace-Url", sessionURL(s))
}

func (h *Hub) handleSend(w http.ResponseWriter, r *http.Request) {
	cid := GetCID(w, r)
	text := r.FormValue("text")
	if text == "" {
		w.WriteHeader(204)
		return
	}

	client := h.GetOrCreateClient(cid)
	sessionID := client.ActiveSession()
	s := h.Sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	s.Mu.Lock()
	if s.Running {
		if s.QueuedText != "" {
			s.QueuedText += "\n" + text
		} else {
			s.QueuedText = text
		}
		queued := s.QueuedText
		s.Mu.Unlock()
		h.BroadcastToSession(s.ID, FormatSSE("htmx", RenderQueueBar(s.ID, queued)))
	} else {
		s.Mu.Unlock()
		h.StartTurn(s, text)
	}

	w.Header().Set("HX-Replace-Url", sessionURL(s))
}

func (h *Hub) handlePerm(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	permID := r.FormValue("perm_id")
	allow := r.FormValue("allow") == "true"
	suggestionJSON := r.FormValue("suggestion")

	s := h.Sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	s.Mu.Lock()
	ch, ok := s.PermChans[permID]
	s.Mu.Unlock()

	if ok {
		var perms []any
		if suggestionJSON != "" {
			var suggestion any
			if err := json.Unmarshal([]byte(suggestionJSON), &suggestion); err == nil {
				perms = []any{suggestion}
				if sm, ok := suggestion.(map[string]any); ok {
					if sm["type"] == "setMode" {
						if mode, ok := sm["mode"].(string); ok {
							s.Mu.Lock()
							s.PermissionMode = mode
							s.Mu.Unlock()
							h.Broadcast(ServerMsg{Type: "permission_mode", SessionID: sessionID, PermMode: mode})
						}
					}
				}
			}
		}
		ch <- PermResponse{Allow: allow, Permissions: perms}
	}

	var resultHTML string
	if allow {
		label := "Allowed"
		if suggestionJSON != "" {
			label = "Allowed (with suggestion)"
		}
		resultHTML = fmt.Sprintf(`<span style="color:var(--tool);font-size:12px">✓ %s</span>`, Esc(label))
	} else {
		resultHTML = `<span style="color:var(--error);font-size:12px">✗ Denied</span>`
	}
	event := FormatSSE("htmx", OobSwap("perm-actions-"+permID, "innerHTML", resultHTML))
	h.BroadcastToSession(sessionID, event)

	w.WriteHeader(204)
}

func (h *Hub) handleMode(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	mode := r.FormValue("mode")

	s := h.Sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	s.Mu.Lock()
	s.PermissionMode = mode
	writeJSON := s.WriteJSON
	s.Mu.Unlock()

	if writeJSON != nil {
		writeJSON(map[string]any{
			"type": "control_request", "request_id": fmt.Sprintf("mode_%d", time.Now().UnixNano()),
			"request": map[string]any{"subtype": "set_permission_mode", "mode": mode},
		})
	}
	h.Broadcast(ServerMsg{Type: "permission_mode", SessionID: sessionID, PermMode: mode})

	w.WriteHeader(204)
}

func (h *Hub) handleSwitch(w http.ResponseWriter, r *http.Request) {
	cid := GetCID(w, r)
	sessionID := r.FormValue("session_id")

	s := h.Sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	client := h.GetOrCreateClient(cid)
	client.SetSession(s.ID)
	h.ReplaySession(cid, s)

	w.Header().Set("HX-Replace-Url", sessionURL(s))
}

func (h *Hub) handleLoad(w http.ResponseWriter, r *http.Request) {
	cid := GetCID(w, r)
	dirKey := r.FormValue("dir_key")
	historyID := r.FormValue("history_id")

	if strings.Contains(dirKey, "/") || strings.Contains(dirKey, "..") ||
		strings.Contains(historyID, "/") || strings.Contains(historyID, "..") {
		w.WriteHeader(204)
		return
	}

	home, _ := os.UserHomeDir()
	jsonlPath := filepath.Join(home, ".claude", "projects", dirKey, historyID+".jsonl")

	if s := h.Sessions.FindByJSONLPath(jsonlPath); s != nil {
		client := h.GetOrCreateClient(cid)
		client.SetSession(s.ID)
		h.ReplaySession(cid, s)
		w.WriteHeader(204)
		return
	}

	allMsgs, claudeID, sessUsage, err := ParseSessionMessages(jsonlPath)
	if err != nil {
		w.WriteHeader(204)
		return
	}

	cwd := ""
	if len(allMsgs) > 0 {
		_, cwd = GetSessionInfo(jsonlPath)
	}
	if cwd == "" {
		cwd = "/" + strings.ReplaceAll(dirKey, "-", "/")
	}

	s := h.Sessions.Create(cwd)
	s.Mu.Lock()
	s.ClaudeID = claudeID
	s.JSONLPath = jsonlPath
	s.CostAccum.TotalCostUSD = sessUsage.TotalCostUSD
	s.CostAccum.ContextUsed = sessUsage.ContextUsed
	s.CostAccum.ContextWindow = sessUsage.ContextWindow
	s.Mu.Unlock()

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

	client := h.GetOrCreateClient(cid)
	client.SetSession(s.ID)
	h.ReplaySession(cid, s)

	w.Header().Set("HX-Replace-Url", sessionURL(s))
}

func (h *Hub) handleDrawer(w http.ResponseWriter, r *http.Request) {
	var buf strings.Builder

	sessions := h.Sessions.List()
	if len(sessions) > 0 {
		buf.WriteString(`<div class="drawer-section-label">Active Sessions</div>`)
		for _, s := range sessions {
			s.Mu.Lock()
			running := s.Running
			sp := ShortPath(s.Cwd)
			sid := s.ID
			claudeID := s.ClaudeID
			mc := s.MessageCount
			s.Mu.Unlock()
			runHTML := ""
			if running {
				runHTML = `<span class="di-running"></span> running`
			}
			displayID := claudeID
			if displayID == "" {
				displayID = "(new)"
			} else if len(displayID) > 12 {
				displayID = displayID[:12] + "..."
			}
			fmt.Fprintf(&buf,
				`<div class="drawer-item" hx-post="/switch" hx-vals='{"session_id":"%s"}' hx-swap="none" hx-on::after-request="document.getElementById('drawer').hidePopover()"><div class="di-name">%s</div><div class="di-path">%s</div><div class="di-meta">%s %d turns</div></div>`,
				Esc(sid), Esc(displayID), Esc(sp), runHTML, mc,
			)
		}
	}

	groups, err := ScanHistory()
	if err == nil && len(groups) > 0 {
		buf.WriteString(`<div class="drawer-section-label">History</div>`)
		for _, group := range groups {
			sp := ShortPath(group.Dir)
			fmt.Fprintf(&buf, `<details class="history-group"><summary class="history-group-header">%s <span style="color:var(--text2);font-size:10px">(%d)</span><button class="new-session-btn" hx-post="/new" hx-vals='{"cwd":"%s"}' hx-swap="none" hx-on::after-request="document.getElementById('drawer').hidePopover()" onclick="event.stopPropagation()">+</button></summary><div class="history-group-items">`, Esc(sp), len(group.Sessions), Esc(group.Dir))
			for _, sess := range group.Sessions {
				modTime, _ := time.Parse(time.RFC3339, sess.ModTime)
				ago := TimeAgo(modTime)
				summary := sess.Summary
				if summary == "" {
					summary = "(empty)"
				}
				fmt.Fprintf(&buf,
					`<div class="history-item" hx-post="/load" hx-vals='{"dir_key":"%s","history_id":"%s"}' hx-swap="none" hx-on::after-request="document.getElementById('drawer').hidePopover()"><div class="hi-summary">%s</div><div class="hi-time">%s</div></div>`,
					Esc(group.DirKey), Esc(sess.ID), Esc(summary), Esc(ago),
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

func (h *Hub) handleStop(w http.ResponseWriter, r *http.Request) {
	cid := GetCID(w, r)
	client := h.GetOrCreateClient(cid)
	sessionID := client.ActiveSession()
	s := h.Sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	s.Mu.Lock()
	writeJSON := s.WriteJSON
	s.Interrupted = true
	s.Mu.Unlock()

	if writeJSON != nil {
		writeJSON(map[string]any{
			"type":       "control_request",
			"request_id": fmt.Sprintf("interrupt_%d", time.Now().UnixNano()),
			"request":    map[string]any{"subtype": "interrupt"},
		})
	}

	w.WriteHeader(204)
}

func (h *Hub) handleCancelQueue(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	s := h.Sessions.Get(sessionID)
	if s != nil {
		s.Mu.Lock()
		s.QueuedText = ""
		s.Mu.Unlock()
		h.BroadcastToSession(sessionID, FormatSSE("htmx", RenderQueueBar(sessionID, "")))
	}
	w.WriteHeader(204)
}
