package monetdroid

import (
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/anupcshan/monetdroid/pkg/claude"
)

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

	w.WriteHeader(http.StatusNoContent)
}
