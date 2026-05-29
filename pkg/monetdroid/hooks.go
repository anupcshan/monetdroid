package monetdroid

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude"
)

// HookLogEntry records a single hook event for the debug log.
type HookLogEntry struct {
	Timestamp time.Time
	EventName string
	SessionID string
	AgentID   string
	ToolName  string
	ToolUseID string
	Body      string // full raw JSON body
}

// hookLog is a bounded in-memory log of hook events for debugging.
type hookLog struct {
	mu      sync.Mutex
	entries []HookLogEntry
	maxSize int
}

func newHookLog(maxSize int) *hookLog {
	return &hookLog{maxSize: maxSize}
}

func (l *hookLog) Append(entry HookLogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) >= l.maxSize {
		l.entries = l.entries[1:]
	}
	l.entries = append(l.entries, entry)
}

// Snapshot returns entries newest first.
func (l *hookLog) List() []HookLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]HookLogEntry, len(l.entries))
	for i, entry := range l.entries {
		out[len(l.entries)-1-i] = entry
	}
	return out
}

// hookRegistry is the routing table from URL token to handler. Lives on the
// Hub so handlers are registered and unregistered alongside the claude
// processes they belong to.
type hookRegistry struct {
	mu       sync.RWMutex
	handlers map[string]claude.HookHandlerFunc
}

func (r *hookRegistry) register(token string, h claude.HookHandlerFunc) {
	r.mu.Lock()
	if r.handlers == nil {
		r.handlers = make(map[string]claude.HookHandlerFunc)
	}
	r.handlers[token] = h
	r.mu.Unlock()
}

func (r *hookRegistry) unregister(token string) {
	r.mu.Lock()
	delete(r.handlers, token)
	r.mu.Unlock()
}

func (r *hookRegistry) lookup(token string) (claude.HookHandlerFunc, bool) {
	r.mu.RLock()
	h, ok := r.handlers[token]
	r.mu.RUnlock()
	return h, ok
}

// RegisterHookHandler associates a routing token with a hook handler. The
// token must appear in the URL claude is configured to POST to (see HookURL).
func (h *Hub) RegisterHookHandler(token string, handler claude.HookHandlerFunc) {
	h.hooks.register(token, handler)
}

// UnregisterHookHandler removes a registration. Safe to call for unknown tokens.
func (h *Hub) UnregisterHookHandler(token string) {
	h.hooks.unregister(token)
}

// HookURL returns the full URL claude should POST hook events to for the
// given routing token.
func (h *Hub) HookURL(token string) string {
	return h.hookBaseURL + "/hooks/" + token
}

// handleHook is the HTTP entry point for claude's hook POSTs. The URL path
// must be /hooks/<token> where <token> matches a previously-registered
// handler. The request body is forwarded to the handler; a non-nil error
// becomes HTTP 500. Success returns 204 with no body.
func (h *Hub) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := strings.TrimPrefix(r.URL.Path, "/hooks/")
	if token == "" || strings.Contains(token, "/") {
		http.Error(w, "invalid hook URL", http.StatusBadRequest)
		return
	}

	handler, ok := h.hooks.lookup(token)
	if !ok {
		http.Error(w, "no hook handler registered for token", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := handler(body); err != nil {
		log.Printf("[hook handler][%s] %v", token, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Log the hook event for the debug page.
	var env struct {
		EventName string `json:"hook_event_name"`
		SessionID string `json:"session_id"`
		AgentID   string `json:"agent_id"`
		ToolName  string `json:"tool_name"`
		ToolUseID string `json:"tool_use_id"`
	}
	if err := json.Unmarshal(body, &env); err == nil {
		h.hookLog.Append(HookLogEntry{
			Timestamp: time.Now(),
			EventName: env.EventName,
			SessionID: env.SessionID,
			AgentID:   env.AgentID,
			ToolName:  env.ToolName,
			ToolUseID: env.ToolUseID,
			Body:      string(body),
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) GetHookLog() []HookLogEntry {
	return h.hookLog.List()
}
