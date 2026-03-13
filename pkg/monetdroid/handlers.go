package monetdroid

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	mux.HandleFunc("/perm-answer", hub.handlePermAnswer)
	mux.HandleFunc("/mode", hub.handleMode)
	mux.HandleFunc("/switch", hub.handleSwitch)
	mux.HandleFunc("/load", hub.handleLoad)
	mux.HandleFunc("/stop", hub.handleStop)
	mux.HandleFunc("/cancel-queue", hub.handleCancelQueue)
	mux.HandleFunc("/drawer", hub.handleDrawer)
	mux.HandleFunc("/session-url", handleSessionURL)
	mux.HandleFunc("/diff", hub.handleDiff)
	mux.HandleFunc("/label-edit", hub.handleLabelEdit)
	mux.HandleFunc("/label", hub.handleLabel)
	mux.HandleFunc("/queue", hub.handleQueue)
	mux.HandleFunc("/ack", hub.handleAck)
	mux.HandleFunc("/api/notifications", hub.handleNotifications)
	return mux
}

func (h *Hub) handleIndex(w http.ResponseWriter, r *http.Request) {
	GetCID(w, r)
	html := indexHTML
	if qs := r.URL.RawQuery; qs != "" {
		html = strings.Replace(html, `sse-connect="/events"`, `sse-connect="/events?`+qs+`"`, 1)
		// Hide the empty-state when restoring a session — it gets replaced by replay
		html = strings.Replace(html, `class="empty-state"`, `class="empty-state" style="display:none"`, 1)
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

// loadSessionFromDisk parses a JSONL file and creates an in-memory session.
func (h *Hub) loadSessionFromDisk(jsonlPath string) *Session {
	allMsgs, claudeID, cwd, sessUsage, err := ParseSessionMessages(jsonlPath)
	if err != nil {
		return nil
	}
	if cwd == "" {
		dirKey := filepath.Base(filepath.Dir(jsonlPath))
		cwd = "/" + strings.ReplaceAll(dirKey, "-", "/")
	}

	s := h.Sessions.Create(cwd)
	s.Mu.Lock()
	s.ClaudeID = claudeID
	s.Label = h.Labels.Get(claudeID)
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
			sm.Images = m.Images
			s.MessageCount++
		case "assistant":
			sm.Type = "text"
			sm.Text = m.Text
		case "tool_use":
			sm.Type = "tool_use"
			sm.Tool = m.Tool
			sm.ToolUseID = m.ToolUseID
			sm.Input = m.Input
		case "tool_result":
			sm.Type = "tool_result"
			sm.Tool = m.Tool
			sm.ToolUseID = m.ToolUseID
			sm.Output = m.Output
		default:
			continue
		}
		s.Log = append(s.Log, sm)
	}
	return s
}

func (h *Hub) handleLabelEdit(w http.ResponseWriter, r *http.Request) {
	cid := GetCID(w, r)
	client := h.GetOrCreateClient(cid)
	sid := client.ActiveSession()
	if sid == "" {
		w.WriteHeader(204)
		return
	}
	s := h.Sessions.Get(sid)
	if s == nil {
		w.WriteHeader(204)
		return
	}
	s.Mu.Lock()
	label := s.Label
	cwd := s.Cwd
	s.Mu.Unlock()
	value := label
	if value == "" {
		value = ShortPath(cwd)
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<form id="session-label" hx-post="/label" hx-swap="outerHTML" style="flex:1;display:flex"><input class="session-label-input" name="label" value="%s" autofocus hx-on:keydown="if(event.key==='Escape'){this.closest('form').outerHTML='<div class=\'session-label\' id=\'session-label\' hx-get=\'/label-edit\' hx-target=\'#session-label\' hx-swap=\'outerHTML\'>'+this.dataset.original+'</div>';htmx.process(document.getElementById('session-label'))}" data-original="%s"></input></form>`,
		Esc(value), Esc(Esc(value)))
}

func (h *Hub) handleLabel(w http.ResponseWriter, r *http.Request) {
	cid := GetCID(w, r)
	client := h.GetOrCreateClient(cid)
	sid := client.ActiveSession()
	if sid == "" {
		w.WriteHeader(204)
		return
	}
	s := h.Sessions.Get(sid)
	if s == nil {
		w.WriteHeader(204)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	s.Mu.Lock()
	s.Label = label
	claudeID := s.ClaudeID
	if label == "" {
		label = ShortPath(s.Cwd)
	}
	s.Mu.Unlock()
	if claudeID != "" {
		h.Labels.Set(claudeID, s.Label)
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<div class="session-label" id="session-label" hx-get="/label-edit" hx-target="#session-label" hx-swap="outerHTML">%s</div>`, Esc(label))
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

	if s := h.loadSessionFromDisk(jsonlPath); s != nil {
		client.SetSession(s.ID)
	}
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
		h.Queue.Ack(sessionID)
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
			// Write replay directly to avoid channel race with old SSE connection
			fmt.Fprint(w, h.BuildReplay(s))
			flusher.Flush()
		}
	} else {
		// No active session — show the notification queue on the landing page
		if queueHTML := RenderQueue(h.Queue.List()); queueHTML != "" {
			fmt.Fprint(w, FormatSSE("htmx", OobSwap("messages", "innerHTML",
				`<div class="queue-header">Needs Attention</div>`+queueHTML)))
			flusher.Flush()
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
	if label := r.FormValue("label"); label != "" {
		s.Mu.Lock()
		s.Label = label
		s.Mu.Unlock()
	}
	client := h.GetOrCreateClient(cid)
	client.SetSession(s.ID)
	h.ReplaySession(cid, s)

	w.Header().Set("HX-Replace-Url", sessionURL(s))
}

func (h *Hub) handleSend(w http.ResponseWriter, r *http.Request) {
	cid := GetCID(w, r)

	// Parse multipart form (supports file uploads). 10MB limit.
	r.ParseMultipartForm(10 << 20)
	text := r.FormValue("text")

	var images []ImageData
	if r.MultipartForm != nil {
		for _, fh := range r.MultipartForm.File["images"] {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				continue
			}
			mediaType := fh.Header.Get("Content-Type")
			if mediaType == "" {
				mediaType = "image/jpeg"
			}
			images = append(images, ImageData{
				MediaType: mediaType,
				Data:      base64.StdEncoding.EncodeToString(data),
			})
		}
	}

	if text == "" && len(images) == 0 {
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
		h.StartTurn(s, text, images)
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

	s.RemovePermission(permID)

	w.WriteHeader(204)
}

func (h *Hub) handlePermAnswer(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	permID := r.FormValue("perm_id")

	s := h.Sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	s.Mu.Lock()
	ch, ok := s.PermChans[permID]
	s.Mu.Unlock()

	if !ok {
		w.WriteHeader(204)
		return
	}

	// Reconstruct the original input from the stored permission request
	var originalInput map[string]any
	s.Mu.Lock()
	for _, m := range s.Log {
		if m.Type == "permission_request" && m.PermID == permID {
			if mi, ok := m.PermInput.(map[string]any); ok {
				originalInput = mi
			}
			break
		}
	}
	s.Mu.Unlock()

	if originalInput == nil {
		w.WriteHeader(204)
		return
	}

	// Build the answers map from form values
	questions, _ := originalInput["questions"].([]any)
	answers := make(map[string]string)
	for qi, q := range questions {
		qm, ok := q.(map[string]any)
		if !ok {
			continue
		}
		question, _ := qm["question"].(string)
		fieldName := fmt.Sprintf("answer_%d", qi)
		multiSelect, _ := qm["multiSelect"].(bool)

		if multiSelect {
			// Collect all selected values
			r.ParseForm()
			vals := r.Form[fieldName]
			var selected []string
			for _, v := range vals {
				if v == "__other__" {
					if other := r.FormValue(fieldName + "_other"); other != "" {
						selected = append(selected, other)
					}
				} else {
					selected = append(selected, v)
				}
			}
			answers[question] = strings.Join(selected, ", ")
		} else {
			val := r.FormValue(fieldName)
			if val == "__other__" {
				val = r.FormValue(fieldName + "_other")
			}
			answers[question] = val
		}
	}

	// Build updatedInput with answers
	updatedInput := make(map[string]any)
	for k, v := range originalInput {
		updatedInput[k] = v
	}
	updatedInput["answers"] = answers

	ch <- PermResponse{Allow: true, UpdatedInput: updatedInput}

	// Replace the entire ask-user form with a compact answered summary
	var summaryHTML strings.Builder
	for question, answer := range answers {
		fmt.Fprintf(&summaryHTML, `<div class="ask-answered"><span class="ask-text">%s</span> <span style="color:var(--tool)">%s</span></div>`, Esc(question), Esc(answer))
	}
	event := FormatSSE("htmx", OobSwap("perm-"+permID, "innerHTML", summaryHTML.String()))
	h.BroadcastToSession(sessionID, event)

	s.RemovePermission(permID)
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
	proc := s.proc
	s.Mu.Unlock()

	if proc != nil && !proc.IsDead() {
		if err := proc.SetPermissionMode(mode); err != nil {
			log.Printf("[mode] error setting permission mode: %v", err)
		}
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

	s := h.loadSessionFromDisk(jsonlPath)
	if s == nil {
		w.WriteHeader(204)
		return
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
			mc := s.MessageCount
			label := s.Label
			summary := ""
			for _, m := range s.Log {
				if m.Type == "user_message" && m.Text != "" {
					summary = m.Text
					break
				}
			}
			s.Mu.Unlock()
			if label != "" {
				summary = label
			} else if summary == "" {
				summary = "(new)"
			} else if len(summary) > 80 {
				summary = summary[:80] + "…"
			}
			runHTML := ""
			if running {
				runHTML = `<span class="di-running"></span> running`
			}
			fmt.Fprintf(&buf,
				`<div class="drawer-item" hx-post="/switch" hx-vals='{"session_id":"%s"}' hx-swap="none" hx-on::after-request="document.getElementById('drawer').hidePopover()"><div class="di-name">%s</div><div class="di-path">%s</div><div class="di-meta">%s %d turns</div></div>`,
				Esc(sid), Esc(summary), Esc(sp), runHTML, mc,
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
				summary := h.Labels.Get(sess.ID)
				if summary == "" {
					summary = sess.Summary
				}
				if summary == "" {
					summary = "(empty)"
				}
				turnsStr := ""
				if sess.NumTurns > 0 {
					turnsStr = fmt.Sprintf(" · %d turns", sess.NumTurns)
				}
				fmt.Fprintf(&buf,
					`<div class="history-item" hx-post="/load" hx-vals='{"dir_key":"%s","history_id":"%s"}' hx-swap="none" hx-on::after-request="document.getElementById('drawer').hidePopover()"><div class="hi-summary">%s</div><div class="hi-time">%s%s</div></div>`,
					Esc(group.DirKey), Esc(sess.ID), Esc(summary), Esc(ago), turnsStr,
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
	proc := s.proc
	s.Interrupted = true
	s.Mu.Unlock()

	if proc != nil && !proc.IsDead() {
		if err := proc.Interrupt(); err != nil {
			log.Printf("[stop] error sending interrupt: %v", err)
		}
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

func (h *Hub) handleDiff(w http.ResponseWriter, r *http.Request) {
	claudeID := r.URL.Query().Get("session")
	s := h.Sessions.FindByClaudeID(claudeID)
	if s == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.Mu.Lock()
	cwd := s.Cwd
	s.Mu.Unlock()

	files, _ := GitDiffFiles(cwd)
	fullDiff, _ := GitDiffFull(cwd)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(RenderDiffPage(claudeID, cwd, files, fullDiff)))
}

func (h *Hub) handleNotifications(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	b := make([]byte, 8)
	rand.Read(b)
	clientID := hex.EncodeToString(b)

	nc := h.AddNotifyClient(clientID)
	defer h.RemoveNotifyClient(clientID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprint(w, FormatSSE("heartbeat", "connected"))
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-nc.events:
			fmt.Fprint(w, event)
			flusher.Flush()
		case <-time.After(30 * time.Second):
			fmt.Fprint(w, FormatSSE("heartbeat", "ping"))
			flusher.Flush()
		}
	}
}

func (h *Hub) handleQueue(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(RenderQueue(h.Queue.List())))
}

func (h *Hub) handleAck(w http.ResponseWriter, r *http.Request) {
	claudeID := r.FormValue("claude_id")
	if claudeID != "" {
		h.Queue.Ack(claudeID)
	}
	w.WriteHeader(204)
}

// handleSessionURL is the target of an HTMX request triggered by the
// url-state OOB swap (see Broadcast in hub.go). Its only job is to return
// the HX-Replace-Url header so the browser URL updates to /?session=<id>.
// Must return 200 (not 204) — HTMX ignores HX-Replace-Url on 204.
func handleSessionURL(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if session != "" {
		w.Header().Set("HX-Replace-Url", "/?session="+session)
	}
	w.Write(nil)
}
