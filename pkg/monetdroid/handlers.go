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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML string


func RegisterRoutes(hub *Hub) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", hub.handleIndex)
	mux.HandleFunc("/events", hub.handleEvents)
	mux.HandleFunc("/new", hub.handleNewSession)
	mux.HandleFunc("/new-workstream", hub.handleNewWorkstream)
	mux.HandleFunc("/send", hub.handleSend)
	mux.HandleFunc("/perm", hub.handlePerm)
	mux.HandleFunc("/perm-answer", hub.handlePermAnswer)
	mux.HandleFunc("/mode", hub.handleMode)
	mux.HandleFunc("/stop", hub.handleStop)
	mux.HandleFunc("/close", hub.handleClose)
	mux.HandleFunc("/cancel-queue", hub.handleCancelQueue)
	mux.HandleFunc("/drawer", hub.handleDrawer)
	mux.HandleFunc("/files", hub.handleFiles)
	mux.HandleFunc("/files/stage", hub.handleFilesStage)
	mux.HandleFunc("/files/unstage", hub.handleFilesUnstage)
	mux.HandleFunc("/label-edit", hub.handleLabelEdit)
	mux.HandleFunc("/label", hub.handleLabel)
	mux.HandleFunc("/queue", hub.handleQueue)
	mux.HandleFunc("/archive", hub.handleArchive)
	mux.HandleFunc("/api/notifications", hub.handleNotifications)
	return mux
}

func (h *Hub) handleIndex(w http.ResponseWriter, r *http.Request) {
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
	return "/?session=" + s.ID
}

// loadSessionFromDisk parses a JSONL file and creates an in-memory session.
func (h *Hub) loadSessionFromDisk(jsonlPath string) *Session {
	allMsgs, claudeID, cwd, branches, sessUsage, err := ParseSessionMessages(jsonlPath)
	if err != nil {
		return nil
	}
	if cwd == "" {
		dirKey := filepath.Base(filepath.Dir(jsonlPath))
		cwd = "/" + strings.ReplaceAll(dirKey, "-", "/")
	}

	s := h.Sessions.Create(claudeID, cwd)
	s.InitFromHistory(h.Labels.Get(claudeID), jsonlPath, branches, CostInfo(sessUsage))

	for _, m := range allMsgs {
		sm := ServerMsg{SessionID: s.ID}
		switch m.Type {
		case "user":
			sm.Type = "user_message"
			sm.Text = m.Text
			sm.Images = m.Images
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
		case "compact_boundary":
			sm.Type = "compact_boundary"
		default:
			continue
		}
		s.Log = append(s.Log, sm)
	}
	return s
}

func (h *Hub) handleLabelEdit(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session_id")
	s := h.Sessions.Get(sid)
	if s == nil {
		w.WriteHeader(204)
		return
	}
	label, cwd := s.GetLabelAndCwd()
	value := label
	if value == "" {
		value = ShortPath(cwd)
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<form id="session-label" hx-post="/label" hx-swap="outerHTML" hx-include="#session-id" style="flex:1;display:flex"><input class="session-label-input" name="label" value="%s" autofocus hx-on:keydown="if(event.key==='Escape'){this.closest('form').outerHTML='<div class=\'session-label\' id=\'session-label\' hx-get=\'/label-edit\' hx-target=\'#session-label\' hx-swap=\'outerHTML\' hx-include=\'#session-id\'>'+this.dataset.original+'</div>';htmx.process(document.getElementById('session-label'))}" data-original="%s"></input></form>`,
		Esc(value), Esc(Esc(value)))
}

func (h *Hub) handleLabel(w http.ResponseWriter, r *http.Request) {
	sid := r.FormValue("session_id")
	s := h.Sessions.Get(sid)
	if s == nil {
		w.WriteHeader(204)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	label = s.UpdateLabel(label)
	h.Labels.Set(s.ID, label)
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<div class="session-label" id="session-label" hx-get="/label-edit" hx-target="#session-label" hx-swap="outerHTML" hx-include="#session-id">%s</div>`, Esc(label))
}

// restoreSession finds a session by ID (in memory or on disk) and assigns it to the client.
func (h *Hub) restoreSession(client *SSEClient, id string) {
	if s := h.Sessions.Get(id); s != nil {
		client.SetSession(s.ID)
		return
	}

	// Find JSONL on disk
	jsonlPath := FindJSONLByClaudeID(id)
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

	// Each SSE connection gets its own unique client ID — no cookies.
	b := make([]byte, 16)
	rand.Read(b)
	connID := hex.EncodeToString(b)

	client := &SSEClient{id: connID, events: make(chan SSEEvent, 64)}
	h.mu.Lock()
	h.clients[connID] = client
	h.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	fmt.Fprint(w, FormatSSE("heartbeat", "connected"))
	flusher.Flush()

	// Restore session from query params (forwarded from page URL via sse-connect)
	if sessionID := r.URL.Query().Get("session"); sessionID != "" {
		h.restoreSession(client, sessionID)
	} else if cwd := r.URL.Query().Get("cwd"); cwd != "" {
		if strings.HasPrefix(cwd, "~/") {
			home, _ := os.UserHomeDir()
			cwd = home + cwd[1:]
		}
		client.mu.Lock()
		client.cwd = cwd
		client.label = r.URL.Query().Get("label")
		client.mu.Unlock()
	}

	// Replay: write all stored events directly, then use seq to dedup live events.
	var lastSeq uint64
	if sid := client.ActiveSession(); sid != "" {
		if s := h.Sessions.Get(sid); s != nil {
			// Seed the EventLog if this is the first client to view this session.
			if _, seq := s.EventLog.Snapshot(); seq == 0 {
				h.SeedEventLog(s)
			}
			lastSeq = h.BuildReplay(s, w, flusher)
		}
	} else {
		client.mu.Lock()
		cwd := client.cwd
		label := client.label
		client.mu.Unlock()
		if cwd != "" {
			// Pre-session state: show chrome with cwd label and empty messages
			sessionLabel := label
			if sessionLabel == "" {
				sessionLabel = ShortPath(cwd)
			}
			var chromeParts []string
			chromeParts = append(chromeParts, OobSwap("session-label", "innerHTML", Esc(sessionLabel)))
			chromeParts = append(chromeParts, OobSwap("session-cwd", "outerHTML",
				fmt.Sprintf(`<input type="hidden" name="cwd" id="session-cwd" value="%s">`, Esc(cwd))))
			chromeParts = append(chromeParts, OobSwap("session-label-value", "outerHTML",
				fmt.Sprintf(`<input type="hidden" name="label" id="session-label-value" value="%s">`, Esc(label))))
			chromeParts = append(chromeParts, CwdCopyButton(cwd))
			chromeParts = append(chromeParts, OobSwap("messages", "innerHTML", ""))
			fmt.Fprint(w, FormatSSE("htmx", strings.Join(chromeParts, "\n")))
			flusher.Flush()
		} else {
			// No active session — show tracked sessions on the landing page
			if sessHTML := RenderTrackedSessions(h.Tracker.List()); sessHTML != "" {
				fmt.Fprint(w, FormatSSE("htmx", OobSwap("messages", "innerHTML",
					`<div class="queue-header">Sessions</div>`+sessHTML)))
				flusher.Flush()
			}
		}
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			h.RemoveClient(connID)
			return
		case sseEvent := <-client.events:
			if sseEvent.Seq <= lastSeq {
				continue // already sent during replay
			}
			fmt.Fprint(w, sseEvent.Event)
			flusher.Flush()
		case <-time.After(30 * time.Second):
			fmt.Fprint(w, FormatSSE("heartbeat", "ping"))
			flusher.Flush()
		}
	}
}

func (h *Hub) handleNewSession(w http.ResponseWriter, r *http.Request) {
	cwd := r.FormValue("cwd")
	if cwd == "" {
		home, _ := os.UserHomeDir()
		cwd = home
	}
	if strings.HasPrefix(cwd, "~/") {
		home, _ := os.UserHomeDir()
		cwd = home + cwd[1:]
	}

	u := "/?cwd=" + url.QueryEscape(cwd)
	if label := r.FormValue("label"); label != "" {
		u += "&label=" + url.QueryEscape(label)
	}
	w.Header().Set("HX-Redirect", u)
}

func (h *Hub) handleNewWorkstream(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	cwd := r.FormValue("cwd")
	if cwd == "" {
		http.Error(w, "cwd is required", http.StatusBadRequest)
		return
	}
	if strings.HasPrefix(cwd, "~/") {
		home, _ := os.UserHomeDir()
		cwd = home + cwd[1:]
	}

	wtPath, err := CreateWorkstream(cwd, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	u := "/?cwd=" + url.QueryEscape(wtPath) + "&label=" + url.QueryEscape(name)
	w.Header().Set("HX-Redirect", u)
}

func (h *Hub) handleSend(w http.ResponseWriter, r *http.Request) {
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

	sessionID := r.FormValue("session_id")
	s := h.Sessions.Get(sessionID)

	if s == nil {
		// New session: start process, wait for ClaudeID, create session
		cwd := r.FormValue("cwd")
		if cwd == "" {
			w.WriteHeader(204)
			return
		}
		label := r.FormValue("label")

		// The broadcast closure captures `s` by reference. It's safe because
		// scan waits on p.ready (which is closed after s is set and BindSession is called).
		logBroadcast := func(msg ServerMsg) {
			s.Append(msg)
			h.Broadcast(msg)
		}
		proc, err := StartProcess(nil, cwd, logBroadcast, "")
		if err != nil {
			log.Printf("[send] start process failed: %s", err)
			w.WriteHeader(500)
			return
		}

		// Send the user message to trigger the stream
		if err := proc.SendUserMessage(text, images); err != nil {
			proc.Kill()
			log.Printf("[send] send message failed: %s", err)
			w.WriteHeader(500)
			return
		}

		// Wait for the ClaudeID from the stream
		var claudeID string
		select {
		case claudeID = <-proc.sessionIDCh:
		case <-proc.dead:
			log.Printf("[send] process died before session ID")
			w.WriteHeader(500)
			return
		case <-time.After(30 * time.Second):
			proc.Kill()
			log.Printf("[send] timeout waiting for session ID")
			w.WriteHeader(500)
			return
		}

		// Create the session with the real ClaudeID
		s = h.Sessions.Create(claudeID, cwd)
		autoLabel := label == ""
		if autoLabel {
			label = text
			if len(label) > 60 {
				label = label[:60] + "..."
			}
		}
		s.InitLive(label, autoLabel, true, proc)

		if label != "" {
			h.Labels.Set(claudeID, label)
		}

		// Bind SSE clients that were waiting on this cwd
		h.mu.RLock()
		for _, c := range h.clients {
			c.mu.Lock()
			if c.cwd == cwd && c.sessionID == "" {
				c.sessionID = s.ID
				c.cwd = ""
			}
			c.mu.Unlock()
		}
		h.mu.RUnlock()

		// Send chrome setup (session-id hidden field, label) to bound clients
		sessionLabel := label
		if autoLabel {
			sessionLabel = "(auto) " + sessionLabel
		}
		h.BroadcastToSession(s.ID, FormatSSE("htmx", strings.Join([]string{
			OobSwap("session-id", "outerHTML",
				fmt.Sprintf(`<input type="hidden" name="session_id" id="session-id" value="%s">`, Esc(s.ID))),
			OobSwap("session-label", "innerHTML", Esc(sessionLabel)),
			OobSwap("close-btn", "outerHTML",
				`<form id="close-btn" hx-post="/close" hx-swap="none" hx-include="#session-id"><button class="header-btn" type="submit" title="Close session">✕</button></form>`),
			CwdCopyButton(s.GetCwd()),
		}, "\n")))

		// Broadcast user message and running state
		s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text, Images: images})
		h.Broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text, Images: images})
		h.Broadcast(ServerMsg{Type: "running", SessionID: s.ID})

		// Unblock scan goroutine to start broadcasting stream events
		proc.BindSession(s)

		// Wait for turn completion in background
		go func() {
			select {
			case <-proc.turnDone:
			case <-proc.dead:
			}
			s.SetRunning(false)
			h.Broadcast(ServerMsg{Type: "done", SessionID: s.ID})
		}()

		w.Header().Set("HX-Replace-Url", sessionURL(s))
		return
	}

	if queued, queuedText := s.EnqueueMessage(text); queued {
		h.BroadcastToSession(s.ID, FormatSSE("htmx", RenderQueueBar(s.ID, queuedText)))
	} else {
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

	ch, ok := s.GetPermChan(permID)

	if ok {
		var perms []PermSuggestion
		if suggestionJSON != "" {
			var suggestion PermSuggestion
			if err := json.Unmarshal([]byte(suggestionJSON), &suggestion); err == nil {
				perms = []PermSuggestion{suggestion}
				if suggestion.Type == "setMode" && suggestion.Mode != "" {
					s.SetPermissionMode(suggestion.Mode)
					h.Broadcast(ServerMsg{Type: "permission_mode", SessionID: sessionID, PermMode: suggestion.Mode})
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

	ch, ok := s.GetPermChan(permID)

	if !ok {
		w.WriteHeader(204)
		return
	}

	// Reconstruct the original input from the stored permission request
	permInput := s.FindPermInput(permID)

	if permInput == nil || permInput.Ask == nil || len(permInput.Ask.Questions) == 0 {
		w.WriteHeader(204)
		return
	}

	// Build the answers map from form values
	answers := make(map[string]string)
	for qi, q := range permInput.Ask.Questions {
		fieldName := fmt.Sprintf("answer_%d", qi)

		if q.MultiSelect {
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
			answers[q.Question] = strings.Join(selected, ", ")
		} else {
			val := r.FormValue(fieldName)
			if val == "__other__" {
				val = r.FormValue(fieldName + "_other")
			}
			answers[q.Question] = val
		}
	}

	ch <- PermResponse{Allow: true, UpdatedInput: buildAskUserResponse(permInput, answers)}

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

	proc := s.SetPermModeAndGetProc(mode)

	if proc != nil && !proc.IsDead() {
		if err := proc.SetPermissionMode(mode); err != nil {
			log.Printf("[mode] error setting permission mode: %v", err)
		}
	}
	h.Broadcast(ServerMsg{Type: "permission_mode", SessionID: sessionID, PermMode: mode})

	w.WriteHeader(204)
}


func (h *Hub) handleDrawer(w http.ResponseWriter, r *http.Request) {
	var buf strings.Builder

	tracked := h.Tracker.List()
	if len(tracked) > 0 {
		buf.WriteString(`<div class="drawer-section-label">Sessions</div>`)
		for _, ts := range tracked {
			summary := ts.Label
			if ts.AutoLabel && summary != "" {
				summary = "(auto) " + summary
			}
			if summary == "" {
				summary = "(no label)"
			}

			sp := ShortPath(MainWorktree(ts.Cwd))

			var statusHTML string
			switch ts.Status {
			case "running":
				statusHTML = `<span class="di-running"></span> running`
			case "completed":
				statusHTML = `<span style="color:var(--tool)">&#x2713; done</span>`
			case "blocked":
				statusHTML = `<span style="color:var(--accent)">&#x25CF; blocked</span>`
			}

			metaExtra := ""
			if s := h.Sessions.Get(ts.ClaudeID); s != nil {
				mc, ctxUsed, ctxWindow := s.Stats()
				if mc > 0 {
					metaExtra = fmt.Sprintf(" · %d msgs", mc)
				}
				if ctxUsed > 0 {
					metaExtra += " · " + FormatTokens(ctxUsed, ctxWindow)
				}
			} else {
				metaExtra = " · " + TimeAgo(time.UnixMilli(ts.UpdatedAtMillis))
			}

			branchHTML := renderBranchChips(ts.Branches)
			fmt.Fprintf(&buf,
				`<div class="drawer-item-row"><a class="drawer-item" href="/?session=%s" onclick="document.getElementById('drawer').hidePopover()"><div class="di-name"><span class="di-name-text">%s</span>%s</div><div class="di-path">%s</div><div class="di-meta">%s%s</div></a>`+
					`<form hx-post="/archive" hx-swap="delete" hx-target="closest .drawer-item-row"><input type="hidden" name="claude_id" value="%s"><button type="submit" class="drawer-close-btn" title="Archive session" onclick="event.stopPropagation()">✕</button></form></div>`,
				Esc(ts.ClaudeID), Esc(summary), branchHTML, Esc(sp), statusHTML, metaExtra, Esc(ts.ClaudeID),
			)
		}
	}

	groups, err := ScanHistory()
	if err == nil && len(groups) > 0 {
		buf.WriteString(`<div class="drawer-section-label">History</div>`)
		for i, group := range groups {
			sp := ShortPath(group.Dir)
			popoverID := fmt.Sprintf("new-ws-%d", i)
			fmt.Fprintf(&buf, `<details class="history-group"><summary class="history-group-header">%s <span style="color:var(--text2);font-size:10px">(%d)</span><button class="new-session-btn" popovertarget="%s" onclick="event.stopPropagation()">+</button></summary><div class="history-group-items">`, Esc(sp), len(group.Sessions), popoverID)
			for _, sess := range group.Sessions {
				ago := TimeAgo(sess.ModTime)
				summary := h.Labels.Get(sess.ID)
				if summary == "" && sess.Summary != "" {
					s := sess.Summary
					if len(s) > 60 {
						s = s[:60] + "…"
					}
					summary = "(auto) " + s
				}
				if summary == "" {
					summary = "(empty)"
				}
				msgsStr := ""
				if sess.NumMsgs > 0 {
					msgsStr = fmt.Sprintf(" · %d msgs", sess.NumMsgs)
				}
				if sess.ContextUsed > 0 {
					msgsStr += fmt.Sprintf(" · %s", FormatTokens(sess.ContextUsed, sess.ContextWindow))
				}
				branchHTML := renderBranchChips(sess.Branches)
				fmt.Fprintf(&buf,
					`<a class="history-item" href="/?session=%s" onclick="document.getElementById('drawer').hidePopover()"><div class="hi-summary"><span class="hi-summary-text">%s</span>%s</div><div class="hi-time">%s%s</div></a>`,
					Esc(sess.ID), Esc(summary), branchHTML, Esc(ago), msgsStr,
				)
			}
			buf.WriteString(`</div></details>`)
			fmt.Fprintf(&buf,
				`<div popover id="%s" class="ws-popover">`+
					`<h3>New Workstream</h3>`+
					`<form hx-post="/new-workstream" hx-swap="none" hx-on::after-request="document.getElementById('drawer').hidePopover();document.getElementById('%s').hidePopover()">`+
					`<input type="hidden" name="cwd" value="%s">`+
					`<label>Name</label>`+
					`<input type="text" name="name" placeholder="auth-refactor" required>`+
					`<div class="modal-actions">`+
					`<button type="button" class="btn-cancel" popovertarget="%s" popovertargetaction="hide">Cancel</button>`+
					`<button type="submit" class="btn-create">Create</button>`+
					`</div></form></div>`,
				popoverID, popoverID, Esc(group.Dir), popoverID,
			)
		}
	}

	if len(tracked) == 0 && (err != nil || len(groups) == 0) {
		buf.WriteString(`<div style="padding:20px;text-align:center;color:var(--text2);font-size:13px">No sessions yet</div>`)
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(buf.String()))
}

func (h *Hub) handleStop(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	s := h.Sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	proc := s.InterruptAndGetProc()

	if proc != nil && !proc.IsDead() {
		if err := proc.Interrupt(); err != nil {
			log.Printf("[stop] error sending interrupt: %v", err)
		}
	}

	w.WriteHeader(204)
}

func (h *Hub) handleClose(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	s := h.Sessions.Remove(sessionID)
	if s == nil {
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(204)
		return
	}

	s.Close()

	if r.FormValue("from") == "drawer" {
		w.WriteHeader(200)
		return
	}
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(204)
}

func (h *Hub) handleCancelQueue(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	s := h.Sessions.Get(sessionID)
	if s != nil {
		s.ClearQueue()
		h.BroadcastToSession(sessionID, FormatSSE("htmx", RenderQueueBar(sessionID, "")))
	}
	w.WriteHeader(204)
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
	w.Write([]byte(RenderTrackedSessions(h.Tracker.List())))
}

func (h *Hub) handleArchive(w http.ResponseWriter, r *http.Request) {
	claudeID := r.FormValue("claude_id")
	if claudeID != "" {
		h.Tracker.Archive(claudeID)
		if s := h.Sessions.Remove(claudeID); s != nil {
			s.Close()
		}
	}
	w.WriteHeader(200)
}

